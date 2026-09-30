package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a fake agentctl: when started by the bootstrap it serves
// /health on -port, and only when it received a bootstrap nonce.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_AGENTCTL") == "1" {
		fs := flag.NewFlagSet("agentctl", flag.ExitOnError)
		port := fs.Int("port", 0, "")
		fs.String("workdir", "", "")
		_ = fs.Parse(os.Args[1:])
		if os.Getenv("AGENTCTL_BOOTSTRAP_NONCE") == "" {
			os.Exit(3)
		}
		_ = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestParseRunPayload(t *testing.T) {
	valid := runPayload{RuntimePath: "/opt/kandev/agentctl", RuntimePort: 8765, BootstrapNonce: "n", WorkspaceDir: "/workspace"}
	cases := map[string]func(*runPayload){
		"missing runtime path": func(p *runPayload) { p.RuntimePath = "" },
		"invalid port":         func(p *runPayload) { p.RuntimePort = 70000 },
		"missing nonce":        func(p *runPayload) { p.BootstrapNonce = "" },
		"missing workspace":    func(p *runPayload) { p.WorkspaceDir = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			raw, _ := json.Marshal(p)
			if _, err := parseRunPayload(string(raw)); err == nil {
				t.Fatal("payload was accepted")
			}
		})
	}
	raw, _ := json.Marshal(valid)
	got, err := parseRunPayload(string(raw))
	if err != nil || got.HealthTimeoutSeconds != 60 {
		t.Fatalf("parseRunPayload(valid) = %+v, %v", got, err)
	}
	if _, err := parseRunPayload("not json"); err == nil {
		t.Fatal("non-JSON payload was accepted")
	}
}

func TestRunHookStartsRuntimeThatOutlivesTheRequest(t *testing.T) {
	t.Setenv("FAKE_AGENTCTL", "1")
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &bootstrap{log: slog.New(slog.DiscardHandler), lifetime: ctx}
	t.Cleanup(func() { b.stopRuntime("test") })
	server := httptest.NewServer(b.handler())
	t.Cleanup(server.Close)

	post := func(hook, payload string) int {
		body, _ := json.Marshal(hookRequest{MicrovmID: "mvm", RunHookPayload: payload})
		resp, err := http.Post(server.URL+hookBasePath+hook, "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	payload, _ := json.Marshal(runPayload{
		RuntimePath: os.Args[0], RuntimePort: port, BootstrapNonce: "nonce",
		WorkspaceDir: filepath.Join(t.TempDir(), "ws"), HealthTimeoutSeconds: 20,
	})

	if got := post("/run", string(payload)); got != http.StatusOK {
		t.Fatalf("run hook = %d", got)
	}
	time.Sleep(200 * time.Millisecond)
	if err := waitHealthy(ctx, port, time.Second); err != nil {
		t.Fatalf("runtime stopped after the run hook returned: %v", err)
	}
	if got := post("/run", string(payload)); got != http.StatusOK {
		t.Fatalf("repeated run hook = %d", got)
	}
	if got := post("/resume", ""); got != http.StatusOK {
		t.Fatalf("resume hook = %d", got)
	}
	if got := post("/terminate", ""); got != http.StatusOK {
		t.Fatalf("terminate hook = %d", got)
	}
	if err := waitHealthy(ctx, port, 500*time.Millisecond); err == nil {
		t.Fatal("runtime still serving after terminate")
	}
	if got := post("/run", "{}"); got != http.StatusBadRequest {
		t.Fatalf("run hook with invalid payload = %d, want 400", got)
	}
}

func TestImageCommandMatchesFlags(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile.microvm")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^CMD\s+(\[.*\])`).FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatal("Dockerfile.microvm has no CMD")
	}
	var args []string
	if err := json.Unmarshal([]byte(match[1]), &args); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("image", flag.ContinueOnError)
	hookPort := registerFlags(fs)
	if err := fs.Parse(args); err != nil || *hookPort != 8080 {
		t.Fatalf("image CMD %v: hook port %d, %v", args, *hookPort, err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
