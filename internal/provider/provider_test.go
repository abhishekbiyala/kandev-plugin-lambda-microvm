package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/awsmicrovm"
	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/microvm"
	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/state"
	"github.com/kandev/kandev/pkg/pluginsdk"
)

type fakeAPI struct {
	mu        sync.Mutex
	launchErr error
	launches  []microvm.Launch
	// states is returned by successive Describe calls; the last one repeats.
	states      []microvm.State
	describeErr error
	hookStatus  int
	hookPosts   []string
	mintedPorts []int32
	terminated  int
}

func (f *fakeAPI) Launch(_ context.Context, spec microvm.Launch) (microvm.Environment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, spec)
	if f.launchErr != nil {
		return microvm.Environment{}, f.launchErr
	}
	return microvm.Environment{ID: "mvm-1", State: microvm.StatePending}, nil
}

func (f *fakeAPI) Describe(context.Context, string) (microvm.Environment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.describeErr != nil {
		return microvm.Environment{}, f.describeErr
	}
	state := microvm.StateRunning
	if len(f.states) > 0 {
		state = f.states[0]
		if len(f.states) > 1 {
			f.states = f.states[1:]
		}
	}
	return microvm.Environment{ID: "mvm-1", Endpoint: "mvm-1.example", State: state}, nil
}

func (f *fakeAPI) MintToken(_ context.Context, _ string, port int32, lifetime time.Duration) (microvm.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintedPorts = append(f.mintedPorts, port)
	return microvm.Token{Value: "token", ExpiresAt: time.Now().Add(lifetime)}, nil
}

func (f *fakeAPI) Terminate(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated++
	return nil
}

func (f *fakeAPI) PostHook(_ context.Context, _, _ string, port int32, path, body string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if port != hookPort {
		return 0, errors.New("hook posted to the wrong port")
	}
	f.hookPosts = append(f.hookPosts, path+" "+body)
	if f.hookStatus != 0 {
		return f.hookStatus, nil
	}
	return 200, nil
}

func (f *fakeAPI) List(context.Context) ([]microvm.Environment, error) { return nil, nil }

type fakeVault struct {
	mu      sync.Mutex
	secrets map[string]string
}

func (v *fakeVault) GetSecret(_ context.Context, key string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.secrets[key]
	return value, ok, nil
}

func (v *fakeVault) SetSecret(_ context.Context, key, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.secrets[key] = value
	return nil
}

func (v *fakeVault) DeleteSecret(_ context.Context, key string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.secrets, key)
	return nil
}

type harness struct {
	provider    *Provider
	api         *fakeAPI
	vault       *fakeVault
	checkpoints []*pluginsdk.CheckpointExecutorResourceRequest
	dir         string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pollInterval = time.Millisecond
	h := &harness{api: &fakeAPI{}, vault: &fakeVault{secrets: map[string]string{}}, dir: t.TempDir()}
	h.provider = h.open(t)
	return h
}

func (h *harness) open(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Config{
		StateDir: h.dir,
		Secrets:  h.vault,
		Checkpoint: func(_ context.Context, req *pluginsdk.CheckpointExecutorResourceRequest) error {
			h.checkpoints = append(h.checkpoints, req)
			return nil
		},
		Clients: func(_ context.Context, region string, credentials awsmicrovm.Credentials) (microvm.API, error) {
			if region != "us-west-2" || credentials.AccessKeyID != "AKID" {
				return nil, errors.New("client built with the wrong region or credentials")
			}
			return h.api, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testProfile() *pluginsdk.ExecutorProfileSnapshot {
	return &pluginsdk.ExecutorProfileSnapshot{
		Config:       map[string]string{"region": "us-west-2", "image_arn": "arn:image", "credential_source": "static"},
		SecretValues: map[string]string{"access_key_id": "AKID", "secret_access_key": "SECRET"},
	}
}

func operation(id, digest string) *pluginsdk.ExecutorProviderRequestContext {
	return &pluginsdk.ExecutorProviderRequestContext{OperationId: id, InputDigest: digest}
}

func (h *harness) provision(t *testing.T, op *pluginsdk.ExecutorProviderRequestContext) *pluginsdk.ProvisionExecutorEnvironmentResponse {
	t.Helper()
	response, err := h.provider.ProvisionExecutorEnvironment(context.Background(), &pluginsdk.ProvisionExecutorEnvironmentRequest{
		Context: op, Profile: testProfile(), BootstrapJson: `{"nonce":"nonce-1","runtime_port":8765}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestValidateProfileChecksEachCredentialSource(t *testing.T) {
	validate := func(config, secrets map[string]string) string {
		response := ValidateProfile(&pluginsdk.ValidateExecutorProfileRequest{Profile: &pluginsdk.ExecutorProfileSnapshot{Config: config, SecretValues: secrets}})
		if !response.GetCapabilities().GetReattach() {
			t.Fatal("capabilities missing")
		}
		var fields []string
		for _, fieldError := range response.GetFieldErrors() {
			fields = append(fields, fieldError.GetField()+":"+fieldError.GetCode())
		}
		return strings.Join(fields, ",")
	}
	base := func(extra map[string]string) map[string]string {
		config := map[string]string{"region": "us-west-2", "image_arn": "arn:image"}
		for k, v := range extra {
			config[k] = v
		}
		return config
	}
	for name, tc := range map[string]struct {
		config, secrets map[string]string
		want            string
	}{
		"host":                 {base(map[string]string{"credential_source": "host"}), nil, ""},
		"host with role":       {base(map[string]string{"credential_source": "host", "role_arn": "arn:aws:iam::1:role/r"}), nil, ""},
		"profile":              {base(map[string]string{"credential_source": "profile", "aws_profile": "dev"}), nil, ""},
		"profile without name": {base(map[string]string{"credential_source": "profile"}), nil, "aws_profile:required"},
		"static":               {base(map[string]string{"credential_source": "static"}), map[string]string{"access_key_id": "a", "secret_access_key": "b"}, ""},
		"static without keys":  {base(map[string]string{"credential_source": "static"}), nil, "access_key_id:required,secret_access_key:required"},
		"unknown source":       {base(map[string]string{"credential_source": "magic"}), nil, "credential_source:invalid"},
		"bad role arns": {base(map[string]string{"credential_source": "host", "role_arn": "admin", "execution_role_arn": "x"}), nil,
			"execution_role_arn:invalid,role_arn:invalid"},
		"missing basics": {map[string]string{"credential_source": "host"}, nil, "image_arn:required,region:required"},
	} {
		if got := validate(tc.config, tc.secrets); got != tc.want {
			t.Errorf("%s: field errors = %q, want %q", name, got, tc.want)
		}
	}
}

func TestProvisionStoresNoSecretsForHostCredentials(t *testing.T) {
	h := newHarness(t)
	var gotCredentials awsmicrovm.Credentials
	h.provider.clients = func(_ context.Context, _ string, credentials awsmicrovm.Credentials) (microvm.API, error) {
		gotCredentials = credentials
		return h.api, nil
	}
	profile := &pluginsdk.ExecutorProfileSnapshot{Config: map[string]string{
		"region": "us-west-2", "image_arn": "arn:image", "credential_source": "host",
		"role_arn": "arn:aws:iam::1:role/launcher", "execution_role_arn": "arn:aws:iam::1:role/agent",
	}}
	response, _ := h.provider.ProvisionExecutorEnvironment(context.Background(), &pluginsdk.ProvisionExecutorEnvironmentRequest{
		Context: operation("op-1", "digest"), Profile: profile, BootstrapJson: `{"nonce":"n"}`,
	})
	if response.GetError() != nil {
		t.Fatalf("provision error = %v", response.GetError())
	}
	if gotCredentials.Source != "host" || gotCredentials.RoleARN != "arn:aws:iam::1:role/launcher" {
		t.Fatalf("credentials = %+v", gotCredentials)
	}
	if h.api.launches[0].ExecutionRoleARN != "arn:aws:iam::1:role/agent" {
		t.Fatalf("launch spec = %+v", h.api.launches[0])
	}
	for _, stored := range h.vault.secrets {
		if strings.Contains(stored, "access_key") {
			t.Fatalf("stored credential carries keys: %s", stored)
		}
	}
}

func TestProvisionIsIdempotentPerOperation(t *testing.T) {
	h := newHarness(t)
	first := h.provision(t, operation("op-1", "digest"))
	if first.GetError() != nil {
		t.Fatalf("provision error = %v", first.GetError())
	}
	resource := first.GetResource()
	if resource.GetResourceHandle() != "mvm-1" || resource.GetPlatform() != "linux-arm64" || resource.GetStateVersion() != 1 {
		t.Fatalf("resource = %+v", resource)
	}
	if len(h.checkpoints) != 1 || h.checkpoints[0].GetPhase() != "provisioned" {
		t.Fatalf("checkpoints = %v", h.checkpoints)
	}

	// A new provider instance models a plugin restart.
	h.provider = h.open(t)
	if again := h.provision(t, operation("op-1", "digest")); again.GetResource().GetResourceHandle() != "mvm-1" {
		t.Fatalf("retry = %+v", again)
	}
	if len(h.api.launches) != 1 {
		t.Fatalf("launches = %d, want 1", len(h.api.launches))
	}
	if conflict := h.provision(t, operation("op-1", "other")); conflict.GetError().GetCode() != "conflict" {
		t.Fatalf("replay with different inputs = %+v", conflict)
	}
	spec := h.api.launches[0]
	if spec.ClientToken != clientToken("op-1") || !strings.HasSuffix(spec.IngressConnector, ":ALL_INGRESS") ||
		!strings.HasSuffix(spec.EgressConnector, ":INTERNET_EGRESS") || spec.MaximumDuration != 8*time.Hour {
		t.Fatalf("launch spec = %+v", spec)
	}
}

func TestProvisionFailureLeavesNothingBehind(t *testing.T) {
	h := newHarness(t)
	h.api.launchErr = microvm.Errorf("permission_denied", false, nil, "denied")
	if got := h.provision(t, operation("op-1", "digest")).GetError().GetCode(); got != "permission_denied" {
		t.Fatalf("error code = %q", got)
	}
	if len(h.vault.secrets) != 0 {
		t.Fatal("credential left in the vault")
	}
	recovered, _ := h.provider.RecoverExecutorOperation(context.Background(), &pluginsdk.RecoverExecutorOperationRequest{Context: operation("op-1", "digest")})
	if recovered.GetOutcome() != "absent" {
		t.Fatalf("recover = %+v", recovered)
	}
}

func TestRecoverExecutorOperation(t *testing.T) {
	h := newHarness(t)
	h.provision(t, operation("op-1", "digest"))
	recover := func(op *pluginsdk.ExecutorProviderRequestContext) *pluginsdk.RecoverExecutorOperationResponse {
		response, err := h.provider.RecoverExecutorOperation(context.Background(), &pluginsdk.RecoverExecutorOperationRequest{Context: op})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	if got := recover(operation("op-1", "digest")); got.GetOutcome() != "found" || got.GetResource().GetResourceHandle() != "mvm-1" {
		t.Fatalf("recover = %+v", got)
	}
	if got := recover(operation("op-1", "other")); got.GetError().GetCode() != "conflict" {
		t.Fatalf("recover with different inputs = %+v", got)
	}
	if _, err := h.provider.store.Reserve(state.Environment{OperationID: "op-2", InputDigest: "digest"}); err != nil {
		t.Fatal(err)
	}
	if got := recover(operation("op-2", "digest")); got.GetOutcome() != "unknown" {
		t.Fatalf("recover of an unfinished launch = %+v", got)
	}
}

func TestResolveConnectionStartsRuntimeOnceAndLeasesOnlyTheRequestedPort(t *testing.T) {
	h := newHarness(t)
	resource := h.provision(t, operation("op-1", "digest")).GetResource()
	h.api.states = []microvm.State{microvm.StatePending, microvm.StateRunning}

	var wg sync.WaitGroup
	responses := make([]*pluginsdk.ResolveExecutorConnectionResponse, 3)
	for i, port := range []uint32{8765, 41001, 41001} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i], _ = h.provider.ResolveExecutorConnection(context.Background(), &pluginsdk.ResolveExecutorConnectionRequest{
				Resource: resource, Purpose: "agentctl", RuntimePort: port,
			})
		}()
	}
	wg.Wait()
	for i, response := range responses {
		lease := response.GetLease()
		if lease == nil || lease.GetBaseUrl() != "https://mvm-1.example" || lease.GetHttpHeaders()["X-aws-proxy-auth"] != "token" {
			t.Fatalf("response %d = %+v", i, response)
		}
	}
	if len(h.api.hookPosts) != 1 || !strings.Contains(h.api.hookPosts[0], `\"bootstrap_nonce\":\"nonce-1\"`) {
		t.Fatalf("run hook posts = %v", h.api.hookPosts)
	}
	for _, port := range h.api.mintedPorts {
		if port == hookPort {
			t.Fatal("a lease was minted for the hook port")
		}
	}
	hook, _ := h.provider.ResolveExecutorConnection(context.Background(), &pluginsdk.ResolveExecutorConnectionRequest{
		Resource: resource, Purpose: "agentctl", RuntimePort: uint32(hookPort),
	})
	if hook.GetError().GetCode() != "unsupported" {
		t.Fatalf("hook port lease = %+v", hook)
	}
}

func TestResolveConnectionReportsARejectedRunHook(t *testing.T) {
	h := newHarness(t)
	resource := h.provision(t, operation("op-1", "digest")).GetResource()
	h.api.hookStatus = 500
	response, _ := h.provider.ResolveExecutorConnection(context.Background(), &pluginsdk.ResolveExecutorConnectionRequest{
		Resource: resource, Purpose: "agentctl", RuntimePort: 8765,
	})
	if response.GetError().GetCode() != "run_hook_failed" || response.GetError().GetRetryAfterSeconds() == 0 {
		t.Fatalf("response = %+v", response)
	}
}

func TestInspectMapsPlatformStates(t *testing.T) {
	h := newHarness(t)
	resource := h.provision(t, operation("op-1", "digest")).GetResource()
	inspect := func() string {
		response, _ := h.provider.InspectExecutorEnvironment(context.Background(), &pluginsdk.InspectExecutorEnvironmentRequest{Resource: resource})
		return response.GetState()
	}
	for state, want := range map[microvm.State]string{
		microvm.StateRunning: "running", microvm.StateSuspended: "suspended",
		microvm.StateTerminated: "terminated", microvm.StatePending: "unknown",
	} {
		h.api.states = []microvm.State{state}
		if got := inspect(); got != want {
			t.Errorf("%s inspected as %q, want %q", state, got, want)
		}
	}
	h.api.describeErr = microvm.Errorf(microvm.CodeNotFound, false, nil, "gone")
	if got := inspect(); got != "absent" {
		t.Errorf("missing environment inspected as %q", got)
	}
	unknown, _ := h.provider.InspectExecutorEnvironment(context.Background(), &pluginsdk.InspectExecutorEnvironmentRequest{
		Resource: &pluginsdk.ExecutorResourceDescriptor{ResourceHandle: "other"},
	})
	if unknown.GetState() != "absent" {
		t.Errorf("unowned environment inspected as %q", unknown.GetState())
	}
}

func TestDestroyConfirmsTerminationBeforeForgetting(t *testing.T) {
	h := newHarness(t)
	resource := h.provision(t, operation("op-1", "digest")).GetResource()
	h.api.states = []microvm.State{microvm.StateRunning, microvm.StateTerminated}
	destroy := func() *pluginsdk.DestroyExecutorEnvironmentResponse {
		response, err := h.provider.DestroyExecutorEnvironment(context.Background(), &pluginsdk.DestroyExecutorEnvironmentRequest{Resource: resource})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	if got := destroy(); !got.GetConfirmedAbsent() || h.api.terminated != 1 {
		t.Fatalf("destroy = %+v, terminated %d", got, h.api.terminated)
	}
	if len(h.vault.secrets) != 0 {
		t.Fatal("credential left in the vault")
	}
	if got := destroy(); !got.GetConfirmedAbsent() || h.api.terminated != 1 {
		t.Fatalf("second destroy = %+v", got)
	}
}
