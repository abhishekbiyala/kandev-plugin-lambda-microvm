// Package awsmicrovm implements microvm.API with the AWS Lambda MicroVM SDK.
package awsmicrovm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/microvm"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

const hookTokenLifetime = 10 * time.Minute

type client interface {
	CreateMicrovmImage(context.Context, *lambdamicrovms.CreateMicrovmImageInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmImageOutput, error)
	GetMicrovmImage(context.Context, *lambdamicrovms.GetMicrovmImageInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmImageOutput, error)
	RunMicrovm(context.Context, *lambdamicrovms.RunMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error)
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	CreateMicrovmAuthToken(context.Context, *lambdamicrovms.CreateMicrovmAuthTokenInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error)
	ListMicrovms(context.Context, *lambdamicrovms.ListMicrovmsInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ListMicrovmsOutput, error)
	TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error)
}

// Service is a microvm.API for one region.
type Service struct {
	client       client
	http         *http.Client
	region       string
	pollInterval time.Duration
}

// Credential sources.
const (
	// SourceHost uses the SDK default chain: environment, shared config, and the
	// EC2, ECS, or EKS role of the machine running Kandev.
	SourceHost = "host"
	// SourceProfile uses a named profile from the shared AWS config files.
	SourceProfile = "profile"
	// SourceStatic uses access keys stored in the executor profile.
	SourceStatic = "static"
)

// Credentials says where AWS credentials come from. Only SourceStatic carries
// secrets; the other sources are resolved again on every call, so rotated and
// short-lived credentials keep working.
type Credentials struct {
	Source          string `json:"source"`
	Profile         string `json:"profile,omitempty"`
	RoleARN         string `json:"role_arn,omitempty"`
	ExternalID      string `json:"external_id,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
}

// New builds a Service for region with credentials from creds.
func New(ctx context.Context, region string, creds Credentials) (*Service, error) {
	cfg, err := LoadConfig(ctx, region, creds)
	if err != nil {
		return nil, err
	}
	return newService(lambdamicrovms.NewFromConfig(cfg), region), nil
}

// LoadConfig resolves an AWS config for creds, assuming creds.RoleARN when set.
func LoadConfig(ctx context.Context, region string, creds Credentials) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	switch creds.Source {
	case SourceHost:
	case SourceProfile:
		opts = append(opts, awsconfig.WithSharedConfigProfile(creds.Profile))
	case SourceStatic:
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)))
	default:
		return aws.Config{}, microvm.Errorf("invalid_config", false, nil, "unknown credential source %q", creds.Source)
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, microvm.Errorf("credentials_unavailable", true, err, "AWS credentials could not be loaded")
	}
	if creds.RoleARN != "" {
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), creds.RoleARN,
			func(o *stscreds.AssumeRoleOptions) {
				o.RoleSessionName = "kandev-lambda-microvm"
				if creds.ExternalID != "" {
					o.ExternalID = aws.String(creds.ExternalID)
				}
			}))
	}
	return cfg, nil
}

func newService(c client, region string) *Service {
	return &Service{client: c, http: &http.Client{Timeout: 3 * time.Minute}, region: region, pollInterval: 10 * time.Second}
}

// ConnectorARN returns the AWS-managed network connector with the given name.
func ConnectorARN(region, name string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:%s", region, name)
}

// CreateImage builds a customer image from the al2023 base and waits until it is usable.
func (s *Service) CreateImage(ctx context.Context, name, buildRoleARN, artifactURI string) (string, error) {
	created, err := s.client.CreateMicrovmImage(ctx, &lambdamicrovms.CreateMicrovmImageInput{
		Name:         aws.String(name),
		BaseImageArn: aws.String(fmt.Sprintf("arn:aws:lambda:%s:aws:microvm-image:al2023-1", s.region)),
		BuildRoleArn: aws.String(buildRoleARN),
		CodeArtifact: &types.CodeArtifactMemberUri{Value: artifactURI},
	})
	if err != nil {
		return "", mapError("image_build_failed", err)
	}
	arn := aws.ToString(created.ImageArn)
	for {
		got, err := s.client.GetMicrovmImage(ctx, &lambdamicrovms.GetMicrovmImageInput{ImageIdentifier: aws.String(arn)})
		if err != nil && !isAPIError(err, "ResourceNotFoundException") {
			return "", mapError("image_build_failed", err)
		}
		if err == nil {
			switch got.State {
			case types.MicrovmImageStateCreated:
				return arn, nil
			case types.MicrovmImageStateCreateFailed:
				return "", microvm.Errorf("image_build_failed", false, nil, "the image build failed")
			}
		}
		if err := sleep(ctx, s.pollInterval); err != nil {
			return "", err
		}
	}
}

// Launch starts an environment.
func (s *Service) Launch(ctx context.Context, spec microvm.Launch) (microvm.Environment, error) {
	launched, err := s.client.RunMicrovm(ctx, &lambdamicrovms.RunMicrovmInput{
		ImageIdentifier:          aws.String(spec.ImageARN),
		MaximumDurationInSeconds: aws.Int32(int32(spec.MaximumDuration / time.Second)),
		IngressNetworkConnectors: []string{spec.IngressConnector},
		EgressNetworkConnectors:  []string{spec.EgressConnector},
		ClientToken:              aws.String(spec.ClientToken),
		ExecutionRoleArn:         optional(spec.ExecutionRoleARN),
	})
	if err != nil {
		return microvm.Environment{}, mapError("launch_failed", err)
	}
	if aws.ToString(launched.MicrovmId) == "" {
		return microvm.Environment{}, microvm.Errorf("launch_failed", true, nil, "the platform returned no environment id")
	}
	return microvm.Environment{
		ID:       aws.ToString(launched.MicrovmId),
		Endpoint: aws.ToString(launched.Endpoint),
		State:    microvm.StatePending,
	}, nil
}

// Describe reads an environment's state and endpoint.
func (s *Service) Describe(ctx context.Context, id string) (microvm.Environment, error) {
	got, err := s.client.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(id)})
	if err != nil {
		return microvm.Environment{}, mapError("describe_failed", err)
	}
	return microvm.Environment{ID: id, Endpoint: aws.ToString(got.Endpoint), State: mapState(got.State)}, nil
}

func mapState(state types.MicrovmState) microvm.State {
	switch state {
	case types.MicrovmStatePending:
		return microvm.StatePending
	case types.MicrovmStateRunning:
		return microvm.StateRunning
	case types.MicrovmStateSuspending, types.MicrovmStateSuspended:
		return microvm.StateSuspended
	case types.MicrovmStateTerminating, types.MicrovmStateTerminated:
		return microvm.StateTerminated
	default:
		return microvm.StateUnknown
	}
}

// MintToken issues an endpoint credential for a single port.
func (s *Service) MintToken(ctx context.Context, id string, port int32, lifetime time.Duration) (microvm.Token, error) {
	created, err := s.client.CreateMicrovmAuthToken(ctx, &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier:   aws.String(id),
		AllowedPorts:        []types.PortSpecification{&types.PortSpecificationMemberPort{Value: port}},
		ExpirationInMinutes: aws.Int32(int32(lifetime / time.Minute)),
	})
	if err != nil {
		// Right after launch the platform can reject a token for a running environment
		// until its state propagates.
		if isAPIError(err, "ResourceNotFoundException", "ConflictException", "ResourceConflictException") {
			return microvm.Token{}, microvm.Errorf("environment_not_ready", true, err, "the environment cannot authorize a token yet")
		}
		return microvm.Token{}, mapError("token_failed", err)
	}
	value := created.AuthToken["X-aws-proxy-auth"]
	if value == "" {
		return microvm.Token{}, microvm.Errorf("token_failed", true, nil, "the platform returned no endpoint credential")
	}
	return microvm.Token{Value: value, ExpiresAt: time.Now().UTC().Add(lifetime)}, nil
}

// Terminate ends an environment.
func (s *Service) Terminate(ctx context.Context, id string) error {
	_, err := s.client.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: aws.String(id)})
	if err != nil && !isAPIError(err, "ResourceNotFoundException") {
		return mapError("terminate_failed", err)
	}
	return nil
}

// PostHook posts a lifecycle hook through the platform endpoint with a token scoped to port.
func (s *Service) PostHook(ctx context.Context, id, endpoint string, port int32, path, body string) (int, error) {
	token, err := s.MintToken(ctx, id, port, hookTokenLifetime)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+endpoint+path, strings.NewReader(body))
	if err != nil {
		return 0, microvm.Errorf("invalid_request", false, err, "the hook request could not be built")
	}
	request.Header.Set("X-aws-proxy-auth", token.Value)
	request.Header.Set("X-aws-proxy-port", strconv.Itoa(int(port)))
	request.Header.Set("Content-Type", "application/json")
	response, err := s.http.Do(request)
	if err != nil {
		return 0, microvm.Errorf("run_hook_failed", true, err, "the hook could not be delivered")
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode, nil
}

// List returns every environment in the region that is not terminated.
func (s *Service) List(ctx context.Context) ([]microvm.Environment, error) {
	var out []microvm.Environment
	var token *string
	for {
		page, err := s.client.ListMicrovms(ctx, &lambdamicrovms.ListMicrovmsInput{NextToken: token})
		if err != nil {
			return nil, mapError("list_failed", err)
		}
		for _, item := range page.Items {
			if item.State != types.MicrovmStateTerminated {
				out = append(out, microvm.Environment{ID: aws.ToString(item.MicrovmId), State: mapState(item.State)})
			}
		}
		if aws.ToString(page.NextToken) == "" {
			return out, nil
		}
		token = page.NextToken
	}
}

// mapError converts an SDK error into a microvm.Error. The SDK error is kept as
// the cause for logging, but it never goes into Code or Message.
func mapError(code string, err error) error {
	switch {
	case isAPIError(err, "ResourceNotFoundException", "NotFoundException"):
		return microvm.Errorf(microvm.CodeNotFound, false, err, "the environment or image does not exist")
	case isAPIError(err, "TooManyRequestsException", "ThrottlingException"):
		return microvm.Errorf(code, true, err, "the platform throttled the request")
	case isAPIError(err, "AccessDeniedException"):
		return microvm.Errorf("permission_denied", false, err, "the profile's AWS identity lacks a required permission")
	case isAPIError(err, "ValidationException", "InvalidParameterValueException"):
		return microvm.Errorf("invalid_request", false, err, "the platform rejected the request")
	default:
		return microvm.Errorf(code, true, err, "the platform call failed")
	}
}

func isAPIError(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, code := range codes {
		if apiErr.ErrorCode() == code {
			return true
		}
	}
	return false
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return aws.String(value)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
