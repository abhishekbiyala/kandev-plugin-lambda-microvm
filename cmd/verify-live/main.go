// Command verify-live runs the provider against a real AWS account: it launches an
// environment, reaches agentctl through connection leases the way Kandev does
// (control handshake, instance creation, instance health), then destroys the
// environment and confirms nothing it created is still running.
//
// Usage: go run ./cmd/verify-live -region <region> -image <image-arn> [-source host|profile] [-aws-profile name]
// [-role-arn arn [-external-id id]] [-execution-role-arn arn] [-exec 'shell command']
//
// With -execution-role-arn it also checks that processes inside the environment can
// read the environment role's credentials from instance metadata (IMDSv2).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/awsmicrovm"
	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/microvm"
	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/provider"
	"github.com/kandev/kandev/pkg/pluginsdk"
)

const controlPort = 8765

func main() {
	region := flag.String("region", "", "AWS region")
	image := flag.String("image", "", "MicroVM image ARN")
	source := flag.String("source", "host", "credential source: host or profile")
	awsProfile := flag.String("aws-profile", "", "shared config profile for -source profile")
	roleARN := flag.String("role-arn", "", "optional role to assume")
	externalID := flag.String("external-id", "", "external ID for -role-arn")
	executionRoleARN := flag.String("execution-role-arn", "", "optional role the environment runs as")
	command := flag.String("exec", "", "optional shell command to run inside the environment, e.g. to check an image")
	flag.Parse()
	if *region == "" || *image == "" {
		fmt.Fprintln(os.Stderr, "-region and -image are required")
		os.Exit(2)
	}
	profile := map[string]string{
		"region": *region, "image_arn": *image, "credential_source": *source,
		"aws_profile": *awsProfile, "role_arn": *roleARN, "external_id": *externalID, "execution_role_arn": *executionRoleARN,
	}
	if err := run(context.Background(), profile, *command); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func run(ctx context.Context, profile map[string]string, command string) (err error) {
	api, err := awsmicrovm.New(ctx, profile["region"], awsmicrovm.Credentials{
		Source: profile["credential_source"], Profile: profile["aws_profile"], RoleARN: profile["role_arn"], ExternalID: profile["external_id"],
	})
	if err != nil {
		return err
	}
	stateDir, err := os.MkdirTemp("", "verify-live-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)
	p, err := provider.New(provider.Config{StateDir: stateDir, Secrets: &memoryVault{values: map[string]string{}}})
	if err != nil {
		return err
	}

	var launched []string
	defer func() { err = errors.Join(err, confirmGone(api, launched)) }()

	op := &pluginsdk.ExecutorProviderRequestContext{OperationId: "verify-" + randomHex(8), InputDigest: "verify"}
	nonce := randomHex(16)
	provisioned, err := p.ProvisionExecutorEnvironment(ctx, &pluginsdk.ProvisionExecutorEnvironmentRequest{
		Context:       op,
		Profile:       &pluginsdk.ExecutorProfileSnapshot{Config: profile},
		BootstrapJson: fmt.Sprintf(`{"nonce":%q,"runtime_port":%d}`, nonce, controlPort),
	})
	if err != nil {
		return err
	}
	if provisioned.GetError() != nil {
		return fmt.Errorf("provision: %s", provisioned.GetError().GetCode())
	}
	resource := provisioned.GetResource()
	launched = append(launched, resource.GetResourceHandle())
	step("provisioned, platform %s", resource.GetPlatform())

	control, err := leaseWithRetry(ctx, p, resource, controlPort, 5*time.Minute)
	if err != nil {
		return err
	}
	step("control lease issued; run hook delivered")

	var handshake struct {
		Token string `json:"token"`
	}
	if err := call(ctx, control, http.MethodPost, "/auth/handshake", "", map[string]string{"nonce": nonce}, &handshake); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	step("agentctl handshake accepted the nonce")

	var instance struct {
		Port int `json:"port"`
	}
	if err := call(ctx, control, http.MethodPost, "/api/v1/instances", handshake.Token,
		map[string]string{"id": op.GetOperationId(), "workspace_path": "/workspace"}, &instance); err != nil {
		return fmt.Errorf("create instance: %w", err)
	}
	step("agentctl created an instance on port %d", instance.Port)

	session, err := leaseWithRetry(ctx, p, resource, uint32(instance.Port), time.Minute)
	if err != nil {
		return err
	}
	if err := call(ctx, session, http.MethodGet, "/health", handshake.Token, nil, nil); err != nil {
		return fmt.Errorf("instance health: %w", err)
	}
	step("instance port answers through its own lease")

	if profile["execution_role_arn"] != "" {
		identity, err := runInEnvironment(ctx, session, handshake.Token,
			`t=$(curl -s -m 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token); `+
				`base=http://169.254.169.254/latest/meta-data/iam/security-credentials; `+
				`role=$(curl -s -m 3 -H "X-aws-ec2-metadata-token: $t" $base/); `+
				`curl -s -m 3 -H "X-aws-ec2-metadata-token: $t" $base/$role | grep -oE '"(Code|Type)" *: *"[^"]*"|"AccessKeyId"' | tr '\n' ' '`)
		if err != nil || !strings.Contains(identity, `"Code":"Success"`) {
			return fmt.Errorf("environment role credentials are not available in the environment: %q %v", identity, err)
		}
		step("environment role credentials are available through instance metadata")
	}

	if command != "" {
		started := time.Now()
		output, err := runInEnvironment(ctx, session, handshake.Token, command)
		if err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		step("exec finished in %s:\n%s", time.Since(started).Round(time.Second), output)
	}

	inspected, err := p.InspectExecutorEnvironment(ctx, &pluginsdk.InspectExecutorEnvironmentRequest{Context: op, Resource: resource})
	if err != nil || inspected.GetState() != "running" {
		return fmt.Errorf("inspect: %v %v", inspected.GetState(), err)
	}
	step("inspect reports running")

	destroyed, err := p.DestroyExecutorEnvironment(ctx, &pluginsdk.DestroyExecutorEnvironmentRequest{Context: op, Resource: resource})
	if err != nil || !destroyed.GetConfirmedAbsent() {
		return fmt.Errorf("destroy: %v %v", destroyed.GetError().GetCode(), err)
	}
	step("destroy confirmed absence")
	return nil
}

// runInEnvironment runs a shell command through agentctl's process API and returns
// its output. agentctl forgets a process when it exits, so the command prints a
// marker and stays alive until the output has been read.
func runInEnvironment(ctx context.Context, lease *pluginsdk.ExecutorConnectionLease, token, command string) (string, error) {
	const marker = "__verify_done__"
	var started struct {
		Process struct {
			ID string `json:"id"`
		} `json:"process"`
	}
	if err := call(ctx, lease, http.MethodPost, "/api/v1/processes/start", token, map[string]string{
		"session_id": "verify", "kind": "user_command", "working_dir": "/workspace",
		"command": "{ " + command + "; } 2>&1; echo " + marker + "; sleep 60",
	}, &started); err != nil {
		return "", err
	}
	defer func() {
		_ = call(ctx, lease, http.MethodPost, "/api/v1/processes/stop", token, map[string]string{"process_id": started.Process.ID}, nil)
	}()
	for range 300 {
		var info struct {
			Output []struct {
				Data string `json:"data"`
			} `json:"output"`
		}
		if err := call(ctx, lease, http.MethodGet, "/api/v1/processes/"+started.Process.ID+"?include_output=true", token, nil, &info); err != nil {
			return "", err
		}
		var out strings.Builder
		for _, chunk := range info.Output {
			out.WriteString(chunk.Data)
		}
		if before, found := strings.CutSuffix(strings.TrimSpace(out.String()), marker); found {
			return strings.TrimSpace(before), nil
		}
		time.Sleep(time.Second)
	}
	return "", errors.New("the command did not finish")
}

func leaseWithRetry(ctx context.Context, p *provider.Provider, resource *pluginsdk.ExecutorResourceDescriptor, port uint32, budget time.Duration) (*pluginsdk.ExecutorConnectionLease, error) {
	deadline := time.Now().Add(budget)
	for {
		response, err := p.ResolveExecutorConnection(ctx, &pluginsdk.ResolveExecutorConnectionRequest{
			Resource: resource, Purpose: "agentctl", RuntimePort: port,
		})
		if err != nil {
			return nil, err
		}
		if response.GetLease() != nil {
			return response.GetLease(), nil
		}
		if response.GetError().GetRetryAfterSeconds() == 0 || time.Now().After(deadline) {
			return nil, fmt.Errorf("lease for port %d: %s", port, response.GetError().GetCode())
		}
		time.Sleep(5 * time.Second)
	}
}

func call(ctx context.Context, lease *pluginsdk.ExecutorConnectionLease, method, path, bearer string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, lease.GetBaseUrl()+path, reader)
	if err != nil {
		return err
	}
	for name, value := range lease.GetHttpHeaders() {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// confirmGone terminates anything this run launched and waits until the platform no
// longer lists it as running.
func confirmGone(api microvm.API, launched []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, id := range launched {
		_ = api.Terminate(ctx, id)
	}
	for {
		running, err := api.List(ctx)
		if err != nil {
			return err
		}
		remaining := 0
		for _, environment := range running {
			for _, id := range launched {
				if environment.ID == id && environment.State != microvm.StateTerminated {
					remaining++
				}
			}
		}
		if remaining == 0 {
			step("no launched environment remains")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d launched environment(s) still running", remaining)
		case <-time.After(10 * time.Second):
		}
	}
}

func step(format string, args ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type memoryVault struct{ values map[string]string }

func (v *memoryVault) GetSecret(_ context.Context, key string) (string, bool, error) {
	value, ok := v.values[key]
	return value, ok, nil
}

func (v *memoryVault) SetSecret(_ context.Context, key, value string) error {
	v.values[key] = value
	return nil
}

func (v *memoryVault) DeleteSecret(_ context.Context, key string) error {
	delete(v.values, key)
	return nil
}
