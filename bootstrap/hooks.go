package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// hookRequest is the body the platform posts to a hook.
type hookRequest struct {
	MicrovmID      string `json:"microvmId"`
	RunHookPayload string `json:"runHookPayload"`
}

// onRun starts the runtime. The platform routes no application traffic until the
// run hook returns 200, so it answers only after the runtime is healthy.
func (b *bootstrap) onRun(w http.ResponseWriter, r *http.Request) {
	payload, err := readRunPayload(r)
	if err != nil {
		b.log.Error("run hook payload rejected", "error", err)
		writeStatus(w, http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runtime != nil {
		writeStatus(w, http.StatusOK)
		return
	}
	proc, err := startRuntime(b.lifetime, payload)
	if err != nil {
		b.log.Error("session runtime did not start", "error", err)
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	b.runtime = proc
	b.log.Info("session runtime ready", "port", payload.RuntimePort)
	writeStatus(w, http.StatusOK)
}

// onResume confirms that the runtime restored from the snapshot is healthy.
func (b *bootstrap) onResume(w http.ResponseWriter, _ *http.Request) {
	b.mu.Lock()
	proc := b.runtime
	b.mu.Unlock()
	if proc == nil {
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	if err := waitHealthy(b.lifetime, proc.port, 30*time.Second); err != nil {
		b.log.Error("runtime unhealthy after resume", "error", err)
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	writeStatus(w, http.StatusOK)
}

func (b *bootstrap) onTerminate(w http.ResponseWriter, _ *http.Request) {
	b.stopRuntime("terminate hook")
	writeStatus(w, http.StatusOK)
}

func (b *bootstrap) stopRuntime(reason string) {
	b.mu.Lock()
	proc := b.runtime
	b.runtime = nil
	b.mu.Unlock()
	if proc != nil {
		proc.stop()
		b.log.Info("session runtime stopped", "reason", reason)
	}
}

func readRunPayload(r *http.Request) (*runPayload, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
	if err != nil {
		return nil, err
	}
	var req hookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("run hook body is not JSON: %w", err)
	}
	return parseRunPayload(req.RunHookPayload)
}

func writeStatus(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{}`))
}
