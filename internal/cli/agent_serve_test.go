package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/app"
	"github.com/stretchr/testify/require"
)

func TestAgentServeUsesFlagEnvironmentYAMLBuiltinPrecedence(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, writeAgentServeConfig(configPath))
	var got app.AgentServiceConfig
	application := &fakeApplication{}
	deps := validDependencies()
	deps.Getenv = mapGetenv(map[string]string{
		"AGENT_WHITEBOARD_AGENT_PORT":                "2002",
		"AGENT_WHITEBOARD_PROVIDER_IDLE_TIMEOUT":     "4m",
		"AGENT_WHITEBOARD_AGENT_SHUTDOWN_TIMEOUT":    "5s",
		"AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE":    "/env/pi",
		"AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE": "/env/codex",
	})
	deps.NewAgentApplication = func(config app.AgentServiceConfig) (Application, error) {
		got = config
		return application, nil
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{
		"--config", configPath, "agent", "serve",
		"--port", "2003", "--provider-idle-timeout", "6m",
		"--shutdown-timeout", "7s", "--pi-executable", "/flag/pi",
		"--codex-executable", "/flag/codex",
	})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, app.AgentServiceConfig{
		ConfigPath: configPath, Port: 2003, PiExecutable: "/flag/pi", CodexExecutable: "/flag/codex",
		IdleTimeout: 6 * time.Minute, ShutdownTimeout: 7 * time.Second,
	}, got)
	require.Equal(t, int32(1), application.closeCalls.Load())
}

func TestAgentServeUsesCodexExecutableEnvironment(t *testing.T) {
	var got app.AgentServiceConfig
	application := &fakeApplication{}
	deps := validDependencies()
	deps.Getenv = mapGetenv(map[string]string{
		"AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE": "/env/codex",
	})
	deps.NewAgentApplication = func(config app.AgentServiceConfig) (Application, error) {
		got = config
		return application, nil
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "serve"})

	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, "/env/codex", got.CodexExecutable)
}

func TestAgentServeCancellationIsGraceful(t *testing.T) {
	started := make(chan struct{})
	application := &fakeApplication{listen: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	deps := validDependencies()
	deps.NewAgentApplication = func(app.AgentServiceConfig) (Application, error) { return application, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "serve"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	<-started
	cancel()
	require.NoError(t, <-done)
	require.Equal(t, int32(1), application.closeCalls.Load())
}

func TestAgentServeRejectsNilFactoryResult(t *testing.T) {
	deps := validDependencies()
	deps.NewAgentApplication = func(app.AgentServiceConfig) (Application, error) { return nil, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "serve"})
	require.EqualError(t, root.ExecuteContext(context.Background()), "agent application factory returned nil")
}

func TestAgentTrustCommandRemainsConfigurationOnly(t *testing.T) {
	deps := validDependencies()
	deps.NewAgentApplication = func(app.AgentServiceConfig) (Application, error) {
		return nil, errors.New("agent serve must not be constructed")
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml"), "agent", "trust", "list"})
	// Trust commands are annotated to avoid general configuration loading and
	// report the selected configuration error directly.
	require.ErrorContains(t, root.ExecuteContext(context.Background()), "configuration file does not exist")
}

func writeAgentServeConfig(path string) error {
	return os.WriteFile(path, []byte("version: 1\nagent:\n  port: 2001\n  provider_idle_timeout: 2m\n  shutdown_timeout: 3s\n"), 0o600)
}
