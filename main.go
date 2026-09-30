// Command kandev-plugin-lambda-microvm is a Kandev remote executor provider that runs
// each agent session in an AWS Lambda MicroVM.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/provider"
	"github.com/kandev/kandev/pkg/pluginsdk"
)

type plugin struct {
	pluginsdk.UnimplementedPlugin

	mu       sync.Mutex
	provider *provider.Provider
}

var _ pluginsdk.ExecutorProviderPlugin = (*plugin)(nil)

func main() {
	pluginsdk.Serve(&plugin{})
}

// ensureProvider builds the provider on first use, because the host connection is
// injected after the plugin starts serving.
func (p *plugin) ensureProvider() (*provider.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	host := p.Host()
	if host == nil {
		return nil, errors.New("the host is not connected yet")
	}
	dataDir := os.Getenv("KANDEV_PLUGIN_DATA_DIR")
	if dataDir == "" {
		return nil, errors.New("KANDEV_PLUGIN_DATA_DIR is not set")
	}
	cfg := provider.Config{StateDir: filepath.Join(dataDir, "executor-state"), Secrets: host}
	if executorHost, ok := host.(pluginsdk.ExecutorProviderHost); ok {
		cfg.Checkpoint = func(ctx context.Context, req *pluginsdk.CheckpointExecutorResourceRequest) error {
			_, err := executorHost.ExecutorProvider().CheckpointExecutorResource(ctx, req)
			return err
		}
	}
	built, err := provider.New(cfg)
	if err != nil {
		return nil, err
	}
	p.provider = built
	return built, nil
}

func (p *plugin) ValidateExecutorProfile(_ context.Context, req *pluginsdk.ValidateExecutorProfileRequest) (*pluginsdk.ValidateExecutorProfileResponse, error) {
	return provider.ValidateProfile(req), nil
}

func (p *plugin) ProvisionExecutorEnvironment(ctx context.Context, req *pluginsdk.ProvisionExecutorEnvironmentRequest) (*pluginsdk.ProvisionExecutorEnvironmentResponse, error) {
	built, err := p.ensureProvider()
	if err != nil {
		return nil, err
	}
	return built.ProvisionExecutorEnvironment(ctx, req)
}

func (p *plugin) RecoverExecutorOperation(ctx context.Context, req *pluginsdk.RecoverExecutorOperationRequest) (*pluginsdk.RecoverExecutorOperationResponse, error) {
	built, err := p.ensureProvider()
	if err != nil {
		return nil, err
	}
	return built.RecoverExecutorOperation(ctx, req)
}

func (p *plugin) AttachExecutorEnvironment(ctx context.Context, req *pluginsdk.AttachExecutorEnvironmentRequest) (*pluginsdk.AttachExecutorEnvironmentResponse, error) {
	built, err := p.ensureProvider()
	if err != nil {
		return nil, err
	}
	return built.AttachExecutorEnvironment(ctx, req)
}

func (p *plugin) InspectExecutorEnvironment(ctx context.Context, req *pluginsdk.InspectExecutorEnvironmentRequest) (*pluginsdk.InspectExecutorEnvironmentResponse, error) {
	built, err := p.ensureProvider()
	if err != nil {
		return nil, err
	}
	return built.InspectExecutorEnvironment(ctx, req)
}

func (p *plugin) ResolveExecutorConnection(ctx context.Context, req *pluginsdk.ResolveExecutorConnectionRequest) (*pluginsdk.ResolveExecutorConnectionResponse, error) {
	built, err := p.ensureProvider()
	if err != nil {
		return nil, err
	}
	return built.ResolveExecutorConnection(ctx, req)
}

func (p *plugin) DestroyExecutorEnvironment(ctx context.Context, req *pluginsdk.DestroyExecutorEnvironmentRequest) (*pluginsdk.DestroyExecutorEnvironmentResponse, error) {
	built, err := p.ensureProvider()
	if err != nil {
		return nil, err
	}
	return built.DestroyExecutorEnvironment(ctx, req)
}
