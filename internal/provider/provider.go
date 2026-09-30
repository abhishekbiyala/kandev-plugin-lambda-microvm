// Package provider implements the Kandev executor provider contract for AWS Lambda
// MicroVM environments.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/awsmicrovm"
	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/microvm"
	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/state"
	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	// hookPort is where the image's bootstrap answers platform lifecycle hooks.
	hookPort int32 = 8080
	// defaultRuntimePort is used when the bootstrap descriptor names no port.
	defaultRuntimePort = 8765

	environmentLifetime = 8 * time.Hour
	leaseLifetime       = 30 * time.Minute
	// readyWait stays inside the host's 15 second connection resolver deadline.
	readyWait     = 12 * time.Second
	terminateWait = time.Minute

	agentctlPath  = "/opt/kandev/agentctl"
	workspacePath = "/workspace"
	healthTimeout = 90 * time.Second
	runHookPath   = "/aws/lambda-microvms/runtime/v1/run"

	platform         = "linux-arm64"
	stateVersion     = 1
	messageIDPrefix  = "provider.lambda-microvm."
	retryAfter       = 5 * time.Second
	credentialPrefix = "executor-operation."
)

// ClientFactory builds a platform client for a region.
type ClientFactory func(ctx context.Context, region string, credentials awsmicrovm.Credentials) (microvm.API, error)

// SecretVault is the host's plugin-owned secret store.
type SecretVault interface {
	GetSecret(ctx context.Context, key string) (string, bool, error)
	SetSecret(ctx context.Context, key, value string) error
	DeleteSecret(ctx context.Context, key string) error
}

// Checkpointer forwards a resource checkpoint to the host.
type Checkpointer func(context.Context, *pluginsdk.CheckpointExecutorResourceRequest) error

// Config configures New.
type Config struct {
	StateDir   string
	Secrets    SecretVault
	Checkpoint Checkpointer
	// Clients defaults to the AWS SDK.
	Clients ClientFactory
}

// Provider implements pluginsdk.ExecutorProviderPlugin.
type Provider struct {
	clients    ClientFactory
	secrets    SecretVault
	checkpoint Checkpointer
	store      *state.Store
	// hookMu serializes run hook delivery so an environment starts one runtime.
	hookMu sync.Mutex
}

var _ pluginsdk.ExecutorProviderPlugin = (*Provider)(nil)

// pollInterval is how often platform state is re-read while waiting.
var pollInterval = 2 * time.Second

// New opens the provider's state and returns a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.StateDir == "" || cfg.Secrets == nil {
		return nil, errors.New("provider requires a state directory and a secret vault")
	}
	store, err := state.Open(filepath.Join(cfg.StateDir, "executors.json"))
	if err != nil {
		return nil, err
	}
	clients := cfg.Clients
	if clients == nil {
		clients = func(ctx context.Context, region string, credentials awsmicrovm.Credentials) (microvm.API, error) {
			return awsmicrovm.New(ctx, region, credentials)
		}
	}
	return &Provider{clients: clients, secrets: cfg.Secrets, checkpoint: cfg.Checkpoint, store: store}, nil
}

type profile struct {
	region           string
	imageARN         string
	executionRoleARN string
	credentials      awsmicrovm.Credentials
}

func readProfile(snapshot *pluginsdk.ExecutorProfileSnapshot) (profile, []*pluginsdk.ExecutorProviderFieldError) {
	config, secrets := snapshot.GetConfig(), snapshot.GetSecretValues()
	value := func(key string) string { return strings.TrimSpace(config[key]) }
	p := profile{
		region:           value("region"),
		imageARN:         value("image_arn"),
		executionRoleARN: value("execution_role_arn"),
		credentials: awsmicrovm.Credentials{
			Source:     value("credential_source"),
			Profile:    value("aws_profile"),
			RoleARN:    value("role_arn"),
			ExternalID: value("external_id"),
		},
	}
	var errs []*pluginsdk.ExecutorProviderFieldError
	fail := func(field, code string) {
		errs = append(errs, &pluginsdk.ExecutorProviderFieldError{Field: field, Code: code, MessageId: messageIDPrefix + code})
	}
	require := func(field, value string) {
		if value == "" {
			fail(field, "required")
		}
	}
	require("region", p.region)
	require("image_arn", p.imageARN)
	switch p.credentials.Source {
	case awsmicrovm.SourceHost:
	case awsmicrovm.SourceProfile:
		require("aws_profile", p.credentials.Profile)
	case awsmicrovm.SourceStatic:
		p.credentials.AccessKeyID = secrets["access_key_id"]
		p.credentials.SecretAccessKey = secrets["secret_access_key"]
		p.credentials.SessionToken = secrets["session_token"]
		require("access_key_id", p.credentials.AccessKeyID)
		require("secret_access_key", p.credentials.SecretAccessKey)
	default:
		fail("credential_source", "invalid")
	}
	for field, arn := range map[string]string{"role_arn": p.credentials.RoleARN, "execution_role_arn": p.executionRoleARN} {
		if arn != "" && !strings.HasPrefix(arn, "arn:") {
			fail(field, "invalid")
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].GetField() < errs[j].GetField() })
	return p, errs
}

func capabilities() *pluginsdk.ExecutorProviderCapabilities {
	return &pluginsdk.ExecutorProviderCapabilities{
		Terminal: true, Files: true, Git: true, EmbeddedEditor: true, Preview: true, Reattach: true,
		Retention: "bounded", MaximumLifetimeSeconds: uint64(environmentLifetime / time.Second),
	}
}

func providerError(code string, retryable bool) *pluginsdk.ExecutorProviderError {
	out := &pluginsdk.ExecutorProviderError{Code: code, MessageId: messageIDPrefix + code}
	if retryable {
		out.RetryAfterSeconds = uint32(retryAfter / time.Second)
	}
	return out
}

// apiError logs err's cause and converts it to a provider error.
func apiError(err error) *pluginsdk.ExecutorProviderError {
	log.Printf("lambda-microvm: %v", err)
	typed, ok := microvm.AsError(err)
	if !ok {
		return providerError("unexpected", true)
	}
	return providerError(typed.Code, typed.Retryable)
}

func descriptor(environment state.Environment) *pluginsdk.ExecutorResourceDescriptor {
	payload, _ := json.Marshal(map[string]string{
		"microvm_id": environment.Handle, "image_arn": environment.ImageARN, "region": environment.Region,
	})
	return &pluginsdk.ExecutorResourceDescriptor{
		ResourceHandle: environment.Handle,
		StateJson:      string(payload),
		Platform:       platform,
		Capabilities:   capabilities(),
		ExpiresAt:      environment.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Retention:      "bounded",
		StateVersion:   stateVersion,
	}
}

// clientToken derives the launch idempotency token from the host operation, so a
// retried launch returns the environment the first attempt created.
func clientToken(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return "kandev-" + hex.EncodeToString(sum[:16])
}

func credentialKey(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return credentialPrefix + hex.EncodeToString(sum[:16])
}

// ValidateExecutorProfile checks a profile without touching AWS.
func (p *Provider) ValidateExecutorProfile(_ context.Context, req *pluginsdk.ValidateExecutorProfileRequest) (*pluginsdk.ValidateExecutorProfileResponse, error) {
	return ValidateProfile(req), nil
}

// ValidateProfile checks a profile. It needs no provider, because the host validates
// before the plugin's host connection is ready.
func ValidateProfile(req *pluginsdk.ValidateExecutorProfileRequest) *pluginsdk.ValidateExecutorProfileResponse {
	_, fieldErrors := readProfile(req.GetProfile())
	return &pluginsdk.ValidateExecutorProfileResponse{Capabilities: capabilities(), FieldErrors: fieldErrors}
}

// ProvisionExecutorEnvironment launches an environment for a host operation.
func (p *Provider) ProvisionExecutorEnvironment(ctx context.Context, req *pluginsdk.ProvisionExecutorEnvironmentRequest) (*pluginsdk.ProvisionExecutorEnvironmentResponse, error) {
	operation := req.GetContext()
	if operation.GetOperationId() == "" {
		return nil, errors.New("missing operation identity")
	}
	prof, fieldErrors := readProfile(req.GetProfile())
	if len(fieldErrors) > 0 {
		return &pluginsdk.ProvisionExecutorEnvironmentResponse{Error: providerError("invalid_config", false)}, nil
	}
	reserved, err := p.store.Reserve(state.Environment{
		OperationID: operation.GetOperationId(), InputDigest: operation.GetInputDigest(),
		ImageARN: prof.imageARN, Region: prof.region, BootstrapJSON: req.GetBootstrapJson(),
		ExpiresAt: time.Now().UTC().Add(environmentLifetime),
	})
	if errors.Is(err, state.ErrInputMismatch) {
		return &pluginsdk.ProvisionExecutorEnvironmentResponse{Error: providerError("conflict", false)}, nil
	}
	if err != nil {
		return nil, err
	}
	if reserved.Handle != "" {
		return &pluginsdk.ProvisionExecutorEnvironmentResponse{Resource: descriptor(reserved)}, nil
	}
	environment, err := p.launch(ctx, reserved, prof)
	if err != nil {
		_ = p.store.Forget(reserved.OperationID)
		_ = p.secrets.DeleteSecret(ctx, credentialKey(reserved.OperationID))
		return &pluginsdk.ProvisionExecutorEnvironmentResponse{Error: apiError(err)}, nil
	}
	resource := descriptor(environment)
	if p.checkpoint != nil {
		if err := p.checkpoint(ctx, &pluginsdk.CheckpointExecutorResourceRequest{Context: operation, Resource: resource, Phase: "provisioned"}); err != nil {
			log.Printf("lambda-microvm: checkpoint: %v", err)
		}
	}
	return &pluginsdk.ProvisionExecutorEnvironmentResponse{Resource: resource}, nil
}

func (p *Provider) launch(ctx context.Context, reserved state.Environment, prof profile) (state.Environment, error) {
	encoded, _ := json.Marshal(prof.credentials)
	if err := p.secrets.SetSecret(ctx, credentialKey(reserved.OperationID), string(encoded)); err != nil {
		return state.Environment{}, microvm.Errorf("credentials_unavailable", true, err, "the profile credential could not be stored")
	}
	client, err := p.clients(ctx, prof.region, prof.credentials)
	if err != nil {
		return state.Environment{}, err
	}
	launched, err := client.Launch(ctx, microvm.Launch{
		ImageARN:         prof.imageARN,
		MaximumDuration:  environmentLifetime,
		IngressConnector: awsmicrovm.ConnectorARN(prof.region, "ALL_INGRESS"),
		EgressConnector:  awsmicrovm.ConnectorARN(prof.region, "INTERNET_EGRESS"),
		ClientToken:      clientToken(reserved.OperationID),
		ExecutionRoleARN: prof.executionRoleARN,
	})
	if err != nil {
		return state.Environment{}, err
	}
	return p.store.Complete(reserved.OperationID, launched.ID)
}

// RecoverExecutorOperation reports what an interrupted provision left behind.
func (p *Provider) RecoverExecutorOperation(_ context.Context, req *pluginsdk.RecoverExecutorOperationRequest) (*pluginsdk.RecoverExecutorOperationResponse, error) {
	environment, found := p.store.ByOperation(req.GetContext().GetOperationId())
	switch {
	case !found:
		return &pluginsdk.RecoverExecutorOperationResponse{Outcome: "absent"}, nil
	case environment.InputDigest != req.GetContext().GetInputDigest():
		return &pluginsdk.RecoverExecutorOperationResponse{Error: providerError("conflict", false)}, nil
	case environment.Handle == "":
		return &pluginsdk.RecoverExecutorOperationResponse{Outcome: "unknown"}, nil
	default:
		return &pluginsdk.RecoverExecutorOperationResponse{Outcome: "found", Resource: descriptor(environment)}, nil
	}
}

// AttachExecutorEnvironment confirms a recorded environment is serving.
func (p *Provider) AttachExecutorEnvironment(ctx context.Context, req *pluginsdk.AttachExecutorEnvironmentRequest) (*pluginsdk.AttachExecutorEnvironmentResponse, error) {
	environment, client, perr := p.owned(ctx, req.GetResource())
	if perr != nil {
		return &pluginsdk.AttachExecutorEnvironmentResponse{Error: perr}, nil
	}
	if _, err := p.ensureServing(ctx, client, environment); err != nil {
		return &pluginsdk.AttachExecutorEnvironmentResponse{Error: apiError(err)}, nil
	}
	return &pluginsdk.AttachExecutorEnvironmentResponse{Resource: descriptor(environment)}, nil
}

// InspectExecutorEnvironment reports the environment's state and expiry.
func (p *Provider) InspectExecutorEnvironment(ctx context.Context, req *pluginsdk.InspectExecutorEnvironmentRequest) (*pluginsdk.InspectExecutorEnvironmentResponse, error) {
	environment, client, perr := p.owned(ctx, req.GetResource())
	if perr != nil {
		if perr.GetCode() == "resource_not_found" {
			return &pluginsdk.InspectExecutorEnvironmentResponse{State: "absent", Reason: "not_owned"}, nil
		}
		return &pluginsdk.InspectExecutorEnvironmentResponse{State: "unknown", Reason: perr.GetCode()}, nil
	}
	live, err := client.Describe(ctx, environment.Handle)
	if microvm.IsNotFound(err) {
		return &pluginsdk.InspectExecutorEnvironmentResponse{State: "absent"}, nil
	}
	if err != nil {
		return &pluginsdk.InspectExecutorEnvironmentResponse{State: "unknown", Reason: apiError(err).GetCode()}, nil
	}
	return &pluginsdk.InspectExecutorEnvironmentResponse{
		State:     hostState(live.State),
		ExpiresAt: environment.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

func hostState(state microvm.State) string {
	switch state {
	case microvm.StateRunning, microvm.StateSuspended, microvm.StateTerminated:
		return string(state)
	default:
		return "unknown"
	}
}

// ResolveExecutorConnection issues a lease for one port inside the environment.
func (p *Provider) ResolveExecutorConnection(ctx context.Context, req *pluginsdk.ResolveExecutorConnectionRequest) (*pluginsdk.ResolveExecutorConnectionResponse, error) {
	port := req.GetRuntimePort()
	if req.GetPurpose() != "agentctl" || port == 0 || port > 65535 || port == uint32(hookPort) {
		return &pluginsdk.ResolveExecutorConnectionResponse{Error: providerError("unsupported", false)}, nil
	}
	environment, client, perr := p.owned(ctx, req.GetResource())
	if perr != nil {
		return &pluginsdk.ResolveExecutorConnectionResponse{Error: perr}, nil
	}
	live, err := p.ensureServing(ctx, client, environment)
	if err != nil {
		return &pluginsdk.ResolveExecutorConnectionResponse{Error: apiError(err)}, nil
	}
	token, err := client.MintToken(ctx, environment.Handle, int32(port), leaseLifetime)
	if err != nil {
		return &pluginsdk.ResolveExecutorConnectionResponse{Error: apiError(err)}, nil
	}
	headers := map[string]string{"X-aws-proxy-auth": token.Value, "X-aws-proxy-port": strconv.Itoa(int(port))}
	return &pluginsdk.ResolveExecutorConnectionResponse{Lease: &pluginsdk.ExecutorConnectionLease{
		BaseUrl:          "https://" + live.Endpoint,
		HttpHeaders:      headers,
		WebsocketHeaders: headers,
		ExpiresAt:        token.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Generation:       strconv.FormatInt(token.ExpiresAt.UnixNano(), 36),
	}}, nil
}

// DestroyExecutorEnvironment terminates the environment and reports absence only
// once the platform confirms it.
func (p *Provider) DestroyExecutorEnvironment(ctx context.Context, req *pluginsdk.DestroyExecutorEnvironmentRequest) (*pluginsdk.DestroyExecutorEnvironmentResponse, error) {
	environment, client, perr := p.owned(ctx, req.GetResource())
	if perr != nil {
		if perr.GetCode() == "resource_not_found" {
			return &pluginsdk.DestroyExecutorEnvironmentResponse{ConfirmedAbsent: true}, nil
		}
		return &pluginsdk.DestroyExecutorEnvironmentResponse{Error: perr}, nil
	}
	if err := client.Terminate(ctx, environment.Handle); err != nil {
		return &pluginsdk.DestroyExecutorEnvironmentResponse{Error: apiError(err)}, nil
	}
	if _, err := waitFor(ctx, client, environment.Handle, terminateWait, func(s microvm.State) bool { return s == microvm.StateTerminated }); err != nil {
		return &pluginsdk.DestroyExecutorEnvironmentResponse{Error: apiError(err)}, nil
	}
	if err := p.store.Forget(environment.OperationID); err != nil {
		return nil, err
	}
	_ = p.secrets.DeleteSecret(ctx, credentialKey(environment.OperationID))
	return &pluginsdk.DestroyExecutorEnvironmentResponse{ConfirmedAbsent: true}, nil
}

// owned resolves a host resource to its record and a client using the stored credential.
func (p *Provider) owned(ctx context.Context, resource *pluginsdk.ExecutorResourceDescriptor) (state.Environment, microvm.API, *pluginsdk.ExecutorProviderError) {
	environment, found := p.store.ByHandle(resource.GetResourceHandle())
	if !found {
		return state.Environment{}, nil, providerError("resource_not_found", false)
	}
	raw, found, err := p.secrets.GetSecret(ctx, credentialKey(environment.OperationID))
	if err != nil || !found {
		return state.Environment{}, nil, apiError(microvm.Errorf("credentials_unavailable", true, err, "the stored credential could not be read"))
	}
	var credentials awsmicrovm.Credentials
	if err := json.Unmarshal([]byte(raw), &credentials); err != nil {
		return state.Environment{}, nil, apiError(microvm.Errorf("credentials_unavailable", false, err, "the stored credential is malformed"))
	}
	client, err := p.clients(ctx, environment.Region, credentials)
	if err != nil {
		return state.Environment{}, nil, apiError(err)
	}
	return environment, client, nil
}

// ensureServing waits for the environment to run and posts its run hook once. The
// host's fresh launch goes straight to ResolveExecutorConnection, so both it and
// Attach come through here.
func (p *Provider) ensureServing(ctx context.Context, client microvm.API, environment state.Environment) (microvm.Environment, error) {
	live, err := waitFor(ctx, client, environment.Handle, readyWait, func(s microvm.State) bool { return s == microvm.StateRunning })
	if err != nil {
		return live, err
	}
	if live.Endpoint == "" {
		return live, microvm.Errorf("environment_not_ready", true, nil, "the environment has no endpoint yet")
	}
	p.hookMu.Lock()
	defer p.hookMu.Unlock()
	if current, _ := p.store.ByHandle(environment.Handle); current.RunHookDelivered {
		return live, nil
	}
	if err := postRunHook(ctx, client, environment, live.Endpoint); err != nil {
		return live, err
	}
	return live, p.store.MarkRunHookDelivered(environment.Handle)
}

// runHookPayload is the bootstrap's run hook contract.
type runHookPayload struct {
	RuntimePath          string `json:"runtime_path"`
	RuntimePort          int    `json:"runtime_port"`
	BootstrapNonce       string `json:"bootstrap_nonce"`
	WorkspaceDir         string `json:"workspace_dir"`
	HealthTimeoutSeconds int    `json:"health_timeout_seconds"`
}

func postRunHook(ctx context.Context, client microvm.API, environment state.Environment, endpoint string) error {
	var bootstrap struct {
		Nonce       string `json:"nonce"`
		RuntimePort int    `json:"runtime_port"`
	}
	if err := json.Unmarshal([]byte(environment.BootstrapJSON), &bootstrap); err != nil || bootstrap.Nonce == "" {
		return microvm.Errorf("invalid_request", false, err, "the bootstrap descriptor has no nonce")
	}
	if bootstrap.RuntimePort == 0 {
		bootstrap.RuntimePort = defaultRuntimePort
	}
	payload, _ := json.Marshal(runHookPayload{
		RuntimePath: agentctlPath, RuntimePort: bootstrap.RuntimePort, BootstrapNonce: bootstrap.Nonce,
		WorkspaceDir: workspacePath, HealthTimeoutSeconds: int(healthTimeout / time.Second),
	})
	body, _ := json.Marshal(map[string]string{"microvmId": environment.Handle, "runHookPayload": string(payload)})
	status, err := client.PostHook(ctx, environment.Handle, endpoint, hookPort, runHookPath, string(body))
	if err != nil {
		return err
	}
	if status != 200 {
		return microvm.Errorf("run_hook_failed", true, nil, "the bootstrap answered the run hook with status %d", status)
	}
	return nil
}

// waitFor polls until done reports true for the state, or budget elapses. A missing
// environment counts as terminated.
func waitFor(ctx context.Context, client microvm.API, handle string, budget time.Duration, done func(microvm.State) bool) (microvm.Environment, error) {
	deadline := time.Now().Add(budget)
	for {
		live, err := client.Describe(ctx, handle)
		if microvm.IsNotFound(err) {
			live, err = microvm.Environment{ID: handle, State: microvm.StateTerminated}, nil
		}
		switch {
		case err != nil:
			return live, err
		case done(live.State):
			return live, nil
		case live.State == microvm.StateTerminated:
			return live, microvm.Errorf("environment_terminated", false, nil, "the environment has terminated")
		case !time.Now().Before(deadline):
			return live, microvm.Errorf("environment_not_ready", true, nil, "the environment is still %s", live.State)
		}
		select {
		case <-ctx.Done():
			return live, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
