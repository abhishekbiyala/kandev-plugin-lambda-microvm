// Command microvm-bootstrap is the entrypoint of the Kandev Lambda MicroVM image.
// It answers the platform's lifecycle hooks on the hook port and, on the run hook,
// starts the agentctl binary baked into the image and waits for it to be healthy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const hookBasePath = "/aws/lambda-microvms/runtime/v1"

type bootstrap struct {
	log *slog.Logger
	// lifetime scopes the runtime process. A hook request context ends when the
	// handler returns, so it cannot own the runtime.
	lifetime context.Context

	mu      sync.Mutex
	runtime *runtimeProcess
}

func registerFlags(fs *flag.FlagSet) *int {
	return fs.Int("hook-port", 8080, "port the platform posts lifecycle hooks to")
}

func main() {
	hookPort := registerFlags(flag.CommandLine)
	flag.Parse()

	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &bootstrap{log: slog.New(slog.NewJSONHandler(os.Stderr, nil)), lifetime: lifetime}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", *hookPort), Handler: b.handler(), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		<-signals
		b.stopRuntime("signal")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	b.log.Info("serving lifecycle hooks", "port", *hookPort)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		b.log.Error("hook server stopped", "error", err)
		os.Exit(1)
	}
}

func (b *bootstrap) handler() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, http.StatusOK) }
	mux.HandleFunc(hookBasePath+"/ready", ok)
	mux.HandleFunc(hookBasePath+"/validate", ok)
	mux.HandleFunc(hookBasePath+"/suspend", ok)
	mux.HandleFunc(hookBasePath+"/run", b.onRun)
	mux.HandleFunc(hookBasePath+"/resume", b.onResume)
	mux.HandleFunc(hookBasePath+"/terminate", b.onTerminate)
	return mux
}
