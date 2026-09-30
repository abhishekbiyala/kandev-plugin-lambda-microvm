package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// runPayload is the run hook payload the provider sends.
type runPayload struct {
	RuntimePath          string `json:"runtime_path"`
	RuntimePort          int    `json:"runtime_port"`
	BootstrapNonce       string `json:"bootstrap_nonce"`
	WorkspaceDir         string `json:"workspace_dir"`
	HealthTimeoutSeconds int    `json:"health_timeout_seconds"`
}

func parseRunPayload(raw string) (*runPayload, error) {
	var p runPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("run hook payload is not JSON: %w", err)
	}
	switch {
	case p.RuntimePath == "":
		return nil, errors.New("run hook payload is missing runtime_path")
	case p.RuntimePort <= 0 || p.RuntimePort > 65535:
		return nil, fmt.Errorf("run hook payload has an invalid runtime_port %d", p.RuntimePort)
	case p.BootstrapNonce == "":
		return nil, errors.New("run hook payload is missing bootstrap_nonce")
	case p.WorkspaceDir == "":
		return nil, errors.New("run hook payload is missing workspace_dir")
	}
	if p.HealthTimeoutSeconds <= 0 {
		p.HealthTimeoutSeconds = 60
	}
	return &p, nil
}

type runtimeProcess struct {
	cmd  *exec.Cmd
	port int
}

// startRuntime starts agentctl's control server on the requested port and waits
// until it is healthy. agentctl listens on all interfaces because the host reaches
// it through the platform endpoint; the bootstrap nonce gates its first handshake.
func startRuntime(ctx context.Context, p *runPayload) (*runtimeProcess, error) {
	if err := os.MkdirAll(p.WorkspaceDir, 0o700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, p.RuntimePath, "-port", strconv.Itoa(p.RuntimePort), "-workdir", p.WorkspaceDir)
	cmd.Env = append(os.Environ(), "AGENTCTL_BOOTSTRAP_NONCE="+p.BootstrapNonce, "AGENTCTL_LISTEN_HOST=")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	proc := &runtimeProcess{cmd: cmd, port: p.RuntimePort}
	if err := waitHealthy(ctx, p.RuntimePort, time.Duration(p.HealthTimeoutSeconds)*time.Second); err != nil {
		proc.stop()
		return nil, err
	}
	return proc, nil
}

func (p *runtimeProcess) stop() {
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func waitHealthy(ctx context.Context, port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("health check returned %d", resp.StatusCode)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("session runtime not healthy within %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
