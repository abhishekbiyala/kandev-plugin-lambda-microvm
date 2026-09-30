package awsmicrovm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/microvm"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/smithy-go"
)

func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: "arn:aws:lambda:us-west-2:000000000000:microvm:secret"}
}

func TestMapErrorClassifiesByCodeWithoutLeakingDetail(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		retryable  bool
	}{
		{"ResourceNotFoundException", microvm.CodeNotFound, false},
		{"ThrottlingException", "launch_failed", true},
		{"AccessDeniedException", "permission_denied", false},
		{"ValidationException", "invalid_request", false},
		{"SomethingNew", "launch_failed", true},
	} {
		cause := apiErr(tc.code)
		got, ok := microvm.AsError(mapError("launch_failed", cause))
		if !ok || got.Code != tc.want || got.Retryable != tc.retryable || !errors.Is(got, cause) {
			t.Errorf("%s mapped to %+v", tc.code, got)
		}
		if strings.Contains(got.Code+got.Message, "000000000000") {
			t.Errorf("%s leaked the account into the boundary error", tc.code)
		}
	}
}

func TestMapStateFoldsTransitions(t *testing.T) {
	for in, want := range map[types.MicrovmState]microvm.State{
		types.MicrovmStatePending: microvm.StatePending, types.MicrovmStateRunning: microvm.StateRunning,
		types.MicrovmStateSuspending: microvm.StateSuspended, types.MicrovmStateTerminating: microvm.StateTerminated,
		"NEW": microvm.StateUnknown,
	} {
		if got := mapState(in); got != want {
			t.Errorf("mapState(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestLaunchSendsClientTokenAndConnectors(t *testing.T) {
	fake := &fakeClient{}
	service := newService(fake, "us-west-2")
	env, err := service.Launch(context.Background(), microvm.Launch{
		ImageARN: "image", IngressConnector: "in", EgressConnector: "out", ClientToken: "token",
	})
	if err != nil || env.ID != "mvm-1" {
		t.Fatalf("Launch = %+v, %v", env, err)
	}
	in := fake.launched
	if aws.ToString(in.ClientToken) != "token" || in.IngressNetworkConnectors[0] != "in" || in.EgressNetworkConnectors[0] != "out" {
		t.Fatalf("RunMicrovm input = %+v", in)
	}
}

func TestDescribeAndTerminateOfAMissingEnvironment(t *testing.T) {
	service := newService(&fakeClient{err: apiErr("ResourceNotFoundException")}, "us-west-2")
	if _, err := service.Describe(context.Background(), "mvm-1"); !microvm.IsNotFound(err) {
		t.Errorf("Describe = %v, want not found", err)
	}
	if err := service.Terminate(context.Background(), "mvm-1"); err != nil {
		t.Errorf("Terminate = %v, want success", err)
	}
}

func TestMintTokenScopesOnePort(t *testing.T) {
	fake := &fakeClient{}
	token, err := newService(fake, "us-west-2").MintToken(context.Background(), "mvm-1", 41001, 0)
	if err != nil || token.Value != "token" {
		t.Fatalf("MintToken = %+v, %v", token, err)
	}
	port, ok := fake.minted.AllowedPorts[0].(*types.PortSpecificationMemberPort)
	if len(fake.minted.AllowedPorts) != 1 || !ok || port.Value != 41001 {
		t.Fatalf("allowed ports = %#v", fake.minted.AllowedPorts)
	}
	fake.err = apiErr("ConflictException")
	if _, err := newService(fake, "us-west-2").MintToken(context.Background(), "mvm-1", 41001, 0); !isRetryable(err) {
		t.Fatalf("MintToken on a settling environment = %v, want retryable", err)
	}
}

func TestPostHookAddressesTheHookPort(t *testing.T) {
	var gotPort, gotAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPort, gotAuth = r.Header.Get("X-aws-proxy-port"), r.Header.Get("X-aws-proxy-auth")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	service := newService(&fakeClient{}, "us-west-2")
	service.http = server.Client()
	status, err := service.PostHook(context.Background(), "mvm-1", strings.TrimPrefix(server.URL, "https://"), 8080, "/run", "{}")
	if err != nil || status != http.StatusAccepted || gotPort != "8080" || gotAuth != "token" {
		t.Fatalf("PostHook = %d, %v; port %q auth %q", status, err, gotPort, gotAuth)
	}
}

func isRetryable(err error) bool {
	typed, ok := microvm.AsError(err)
	return ok && typed.Retryable
}

type fakeClient struct {
	err      error
	launched *lambdamicrovms.RunMicrovmInput
	minted   *lambdamicrovms.CreateMicrovmAuthTokenInput
}

func (f *fakeClient) CreateMicrovmImage(context.Context, *lambdamicrovms.CreateMicrovmImageInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmImageOutput, error) {
	return nil, f.err
}

func (f *fakeClient) GetMicrovmImage(context.Context, *lambdamicrovms.GetMicrovmImageInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmImageOutput, error) {
	return nil, f.err
}

func (f *fakeClient) RunMicrovm(_ context.Context, in *lambdamicrovms.RunMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	f.launched = in
	return &lambdamicrovms.RunMicrovmOutput{MicrovmId: aws.String("mvm-1")}, f.err
}

func (f *fakeClient) GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &lambdamicrovms.GetMicrovmOutput{State: types.MicrovmStateRunning}, nil
}

func (f *fakeClient) CreateMicrovmAuthToken(_ context.Context, in *lambdamicrovms.CreateMicrovmAuthTokenInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	f.minted = in
	if f.err != nil {
		return nil, f.err
	}
	return &lambdamicrovms.CreateMicrovmAuthTokenOutput{AuthToken: map[string]string{"X-aws-proxy-auth": "token"}}, nil
}

func (f *fakeClient) ListMicrovms(context.Context, *lambdamicrovms.ListMicrovmsInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ListMicrovmsOutput, error) {
	return &lambdamicrovms.ListMicrovmsOutput{}, f.err
}

func (f *fakeClient) TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	return nil, f.err
}

func TestLoadConfigResolvesEachSource(t *testing.T) {
	dir := t.TempDir()
	credentialsFile := dir + "/credentials"
	if err := os.WriteFile(credentialsFile, []byte("[named]\naws_access_key_id = NAMED\naws_secret_access_key = s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
	t.Setenv("AWS_CONFIG_FILE", dir+"/config")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "HOST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	ctx := context.Background()
	for source, want := range map[Credentials]string{
		{Source: SourceHost}:                                                "HOST",
		{Source: SourceProfile, Profile: "named"}:                           "NAMED",
		{Source: SourceStatic, AccessKeyID: "STATIC", SecretAccessKey: "s"}: "STATIC",
	} {
		cfg, err := LoadConfig(ctx, "us-west-2", source)
		if err != nil {
			t.Fatalf("%s: %v", source.Source, err)
		}
		got, err := cfg.Credentials.Retrieve(ctx)
		if err != nil || got.AccessKeyID != want {
			t.Errorf("%s resolved %q, %v; want %q", source.Source, got.AccessKeyID, err, want)
		}
	}
	if _, err := LoadConfig(ctx, "us-west-2", Credentials{Source: "magic"}); err == nil {
		t.Error("an unknown source was accepted")
	}
}
