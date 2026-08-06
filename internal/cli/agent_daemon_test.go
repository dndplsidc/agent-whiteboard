package cli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/app"
	generalconfig "github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/edocsss/agent-whiteboard/internal/launchagent"
	"github.com/stretchr/testify/require"
)

type fakeLaunchAgentManager struct {
	installConfig launchagent.Config
	installCtx    context.Context
	status        launchagent.Status
	statusCtx     context.Context
	operation     string
	operationCtx  context.Context
	err           error
}

func (manager *fakeLaunchAgentManager) Install(ctx context.Context, config launchagent.Config) error {
	manager.installCtx = ctx
	manager.installConfig = config
	return manager.err
}
func (manager *fakeLaunchAgentManager) Status(ctx context.Context) (launchagent.Status, error) {
	manager.statusCtx = ctx
	return manager.status, manager.err
}
func (manager *fakeLaunchAgentManager) Restart(ctx context.Context) error {
	manager.operation, manager.operationCtx = "restart", ctx
	return manager.err
}
func (manager *fakeLaunchAgentManager) Stop(ctx context.Context) error {
	manager.operation, manager.operationCtx = "stop", ctx
	return manager.err
}
func (manager *fakeLaunchAgentManager) Uninstall(ctx context.Context) error {
	manager.operation, manager.operationCtx = "uninstall", ctx
	return manager.err
}

func TestAgentDaemonInstallCapturesAbsoluteInputsAndProviderOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	manager := &fakeLaunchAgentManager{}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
	deps.ExecutablePath = func() (string, error) { return "bin/agent-whiteboard", nil }
	deps.Getenv = mapGetenv(map[string]string{
		launchagent.PiExecutableEnvironment:    "/env/pi",
		launchagent.CodexExecutableEnvironment: "/env/codex",
	})
	var foregroundCalls int
	deps.NewAgentApplication = func(app.AgentServiceConfig) (Application, error) {
		foregroundCalls++
		return nil, errors.New("foreground construction is forbidden")
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), struct{}{}, "daemon")
	root.SetArgs([]string{
		"--config", "relative/config.yaml", "agent", "serve", "--daemon",
		"--pi-executable", "/flag/pi", "--codex-executable", "/flag/codex",
	})
	require.NoError(t, root.ExecuteContext(ctx))

	require.Equal(t, filepath.Join(mustWorkingDirectory(t), "bin", "agent-whiteboard"), manager.installConfig.Executable)
	require.Equal(t, filepath.Join(mustWorkingDirectory(t), "relative", "config.yaml"), manager.installConfig.ConfigPath)
	require.Len(t, manager.installConfig.Providers, 2)
	require.Equal(t, launchagent.ProviderPi, manager.installConfig.Providers[0].ProviderName())
	require.Equal(t, launchagent.ProviderPi, manager.installConfig.Providers[0].ExecutableName())
	require.Equal(t, launchagent.ProviderCodex, manager.installConfig.Providers[1].ProviderName())
	require.Equal(t, launchagent.ProviderCodex, manager.installConfig.Providers[1].ExecutableName())
	require.NotNil(t, manager.installConfig.ExecutableResolver)
	resolved, err := manager.installConfig.ExecutableResolver.LookPath(launchagent.ProviderPi)
	require.NoError(t, err)
	require.Equal(t, "/flag/pi", resolved)
	resolved, err = manager.installConfig.ExecutableResolver.LookPath(launchagent.ProviderCodex)
	require.NoError(t, err)
	require.Equal(t, "/flag/codex", resolved)
	require.Same(t, ctx, manager.installCtx)
	require.Zero(t, foregroundCalls)
}

func TestAgentDaemonInstallJSONSuccess(t *testing.T) {
	manager := &fakeLaunchAgentManager{}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
	deps.ExecutablePath = func() (string, error) { return "/agent-whiteboard", nil }
	var stdout bytes.Buffer
	deps.Stdout = &stdout
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"--json", "agent", "serve", "--daemon"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, "{\"schema_version\":1}\n", stdout.String())
}

func TestAgentDaemonInstallUsesEnvironmentProviderOverridesAndDefaultConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	manager := &fakeLaunchAgentManager{}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
	deps.ExecutablePath = func() (string, error) { return "/agent-whiteboard", nil }
	deps.Getenv = mapGetenv(map[string]string{
		launchagent.PiExecutableEnvironment:    "/env/pi",
		launchagent.CodexExecutableEnvironment: "/env/codex",
	})
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "serve", "--daemon"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, filepath.Join(home, ".agent-whiteboard", "config.yaml"), manager.installConfig.ConfigPath)
	resolved, err := manager.installConfig.ExecutableResolver.LookPath(launchagent.ProviderPi)
	require.NoError(t, err)
	require.Equal(t, "/env/pi", resolved)
	resolved, err = manager.installConfig.ExecutableResolver.LookPath(launchagent.ProviderCodex)
	require.NoError(t, err)
	require.Equal(t, "/env/codex", resolved)
}

func TestAgentDaemonInstallWithoutOverrideUsesOrdinaryResolver(t *testing.T) {
	manager := &fakeLaunchAgentManager{}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
	deps.ExecutablePath = func() (string, error) { return "/agent-whiteboard", nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "serve", "--daemon"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Nil(t, manager.installConfig.ExecutableResolver)
	require.Len(t, manager.installConfig.Providers, 2)
}

func TestAgentDaemonStatusRedactsLaunchAgentDetails(t *testing.T) {
	manager := &fakeLaunchAgentManager{status: launchagent.Status{Installed: true, Loaded: true, Running: true, PID: 1234}}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
	var stdout bytes.Buffer
	deps.Stdout = &stdout
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "daemon", "status"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, "installed: true\nloaded: true\nrunning: true\npid: 1234\n", stdout.String())
	require.NotContains(t, stdout.String(), "launchctl")
	require.NotContains(t, stdout.String(), "LaunchAgents")

	stdout.Reset()
	root, err = NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"--json", "agent", "daemon", "status"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, "{\"schema_version\":1,\"installed\":true,\"loaded\":true,\"running\":true,\"pid\":1234}\n", stdout.String())
}

func TestAgentDaemonStatusRejectsInvalidPIDCombinations(t *testing.T) {
	for name, status := range map[string]launchagent.Status{
		"running without PID":    {Installed: true, Loaded: true, Running: true},
		"running while unloaded": {Installed: true, Running: true, PID: 12},
		"stopped with PID":       {Installed: true, Loaded: true, PID: 12},
	} {
		t.Run(name, func(t *testing.T) {
			manager := &fakeLaunchAgentManager{status: status}
			deps := validDependencies()
			deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs([]string{"--json", "agent", "daemon", "status"})
			require.EqualError(t, root.ExecuteContext(context.Background()), "launch agent manager returned invalid status")
		})
	}
}

func TestAgentDaemonLifecycleDispatchesContextAndJSONMutation(t *testing.T) {
	for _, operation := range []string{"restart", "stop", "uninstall"} {
		t.Run(operation, func(t *testing.T) {
			manager := &fakeLaunchAgentManager{}
			deps := validDependencies()
			deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
			var stdout bytes.Buffer
			deps.Stdout = &stdout
			root, err := NewRoot(deps)
			require.NoError(t, err)
			ctx := context.WithValue(context.Background(), struct{}{}, operation)
			root.SetArgs([]string{"--json", "agent", "daemon", operation})
			require.NoError(t, root.ExecuteContext(ctx))
			require.Equal(t, operation, manager.operation)
			require.Same(t, ctx, manager.operationCtx)
			require.Equal(t, "{\"schema_version\":1}\n", stdout.String())
		})
	}
}

func TestAgentServeRejectsExplicitEmptyProviderExecutable(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "foreground Pi", args: []string{"agent", "serve", "--pi-executable="}, want: "Pi executable must not be empty"},
		{name: "daemon Pi", args: []string{"agent", "serve", "--daemon", "--pi-executable="}, want: "Pi executable must not be empty"},
		{name: "foreground Codex", args: []string{"agent", "serve", "--codex-executable="}, want: "Codex executable must not be empty"},
		{name: "daemon Codex", args: []string{"agent", "serve", "--daemon", "--codex-executable="}, want: "Codex executable must not be empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := validDependencies()
			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs(test.args)
			require.EqualError(t, root.ExecuteContext(context.Background()), test.want)
		})
	}
}

func TestSelectedProviderExecutableResolverRejectsMissingExplicitSelection(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resolver := selectedProviderExecutableResolver{codex: "missing-codex-selection"}

	_, err := resolver.LookPath(launchagent.ProviderCodex)
	require.Error(t, err)
	require.NotErrorIs(t, err, exec.ErrNotFound)
	require.ErrorContains(t, err, "missing-codex-selection")

	_, err = resolver.LookPath(launchagent.ProviderPi)
	require.ErrorIs(t, err, exec.ErrNotFound)
}

func TestAgentDaemonRejectsForegroundSettingsAndNilManager(t *testing.T) {
	for _, flag := range []string{"--port", "--provider-idle-timeout", "--shutdown-timeout"} {
		t.Run(flag, func(t *testing.T) {
			deps := validDependencies()
			deps.NewLaunchAgentManager = func() (launchagent.Manager, error) {
				return nil, errors.New("manager must not be constructed")
			}
			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs([]string{"agent", "serve", "--daemon", flag, "1s"})
			err = root.ExecuteContext(context.Background())
			require.EqualError(t, err, flag+" cannot be used with --daemon")
		})
	}

	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return nil, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "daemon", "status"})
	require.EqualError(t, root.ExecuteContext(context.Background()), "launch agent manager factory returned nil")
}

func TestAgentDaemonUnsupportedErrorKeepsExactGuidance(t *testing.T) {
	manager := &fakeLaunchAgentManager{err: launchagent.ErrUnsupported}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"agent", "daemon", "status"}, deps)
	require.Equal(t, exitInternal, code)
	require.Empty(t, stdout.String())
	require.Equal(t, "Error: "+launchagent.ErrUnsupported.Error()+"\n", stderr.String())
}

func TestAgentDaemonOperationsDoNotLoadConfiguration(t *testing.T) {
	deps := validDependencies()
	deps.LoadConfig = func(string) (generalconfig.Config, error) {
		return generalconfig.Config{}, errors.New("configuration must not be loaded")
	}
	manager := &fakeLaunchAgentManager{}
	deps.NewLaunchAgentManager = func() (launchagent.Manager, error) { return manager, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	for _, args := range [][]string{{"agent", "daemon", "status"}, {"agent", "daemon", "restart"}, {"agent", "serve", "--daemon"}} {
		root.SetArgs(args)
		require.NoError(t, root.ExecuteContext(context.Background()), args)
	}
}

func mustWorkingDirectory(t *testing.T) string {
	t.Helper()
	workingDirectory, err := filepath.Abs(".")
	require.NoError(t, err)
	return workingDirectory
}
