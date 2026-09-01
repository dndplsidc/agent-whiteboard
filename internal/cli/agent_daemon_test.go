package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/app"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	generalconfig "github.com/dndplsidc/agent-whiteboard/internal/config"
	"github.com/stretchr/testify/require"
)

type fakeLaunchAgentManager struct {
	installConfig common.LaunchAgentConfig
	installCtx    context.Context
	status        common.LaunchAgentStatus
	statusCtx     context.Context
	operation     string
	operationCtx  context.Context
	err           error
}

func (manager *fakeLaunchAgentManager) Install(ctx context.Context, config common.LaunchAgentConfig) error {
	manager.installCtx = ctx
	manager.installConfig = config
	return manager.err
}
func (manager *fakeLaunchAgentManager) Status(ctx context.Context) (common.LaunchAgentStatus, error) {
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
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
	deps.ExecutablePath = func() (string, error) { return "bin/agent-whiteboard", nil }
	deps.Getenv = mapGetenv(map[string]string{
		common.LaunchAgentPiExecutableEnvironment:     "/env/pi",
		common.LaunchAgentCodexExecutableEnvironment:  "/env/codex",
		common.LaunchAgentCursorExecutableEnvironment: "/env/cursor-agent",
		common.LaunchAgentPathEnvironment:             "/activated/nvm/bin:/usr/bin:/bin",
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
		"--pi-executable", "/flag/pi", "--codex-executable", "/flag/codex", "--cursor-executable", "/flag/cursor-agent",
	})
	require.NoError(t, root.ExecuteContext(ctx))

	require.Equal(t, filepath.Join(mustWorkingDirectory(t), "bin", "agent-whiteboard"), manager.installConfig.Executable)
	require.Equal(t, filepath.Join(mustWorkingDirectory(t), "relative", "config.yaml"), manager.installConfig.ConfigPath)
	require.Equal(t, "/activated/nvm/bin:/usr/bin:/bin", manager.installConfig.EnvironmentPath)
	require.Len(t, manager.installConfig.Providers, 3)
	require.Equal(t, common.LaunchAgentProviderPi, manager.installConfig.Providers[0].ProviderName())
	require.Equal(t, common.LaunchAgentProviderPi, manager.installConfig.Providers[0].ExecutableName())
	require.Equal(t, common.LaunchAgentProviderCodex, manager.installConfig.Providers[1].ProviderName())
	require.Equal(t, common.LaunchAgentProviderCodex, manager.installConfig.Providers[1].ExecutableName())
	require.Equal(t, common.LaunchAgentProviderCursor, manager.installConfig.Providers[2].ProviderName())
	require.Equal(t, common.LaunchAgentCursorExecutableName, manager.installConfig.Providers[2].ExecutableName())
	require.NotNil(t, manager.installConfig.ExecutableResolver)
	resolved, err := manager.installConfig.ExecutableResolver.LookPath(common.LaunchAgentProviderPi)
	require.NoError(t, err)
	require.Equal(t, "/flag/pi", resolved)
	resolved, err = manager.installConfig.ExecutableResolver.LookPath(common.LaunchAgentProviderCodex)
	require.NoError(t, err)
	require.Equal(t, "/flag/codex", resolved)
	resolved, err = manager.installConfig.ExecutableResolver.LookPath(common.LaunchAgentCursorExecutableName)
	require.NoError(t, err)
	require.Equal(t, "/flag/cursor-agent", resolved)
	require.Same(t, ctx, manager.installCtx)
	require.Zero(t, foregroundCalls)
}

func TestAgentDaemonInstallJSONSuccess(t *testing.T) {
	manager := &fakeLaunchAgentManager{}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
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
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
	deps.ExecutablePath = func() (string, error) { return "/agent-whiteboard", nil }
	deps.Getenv = mapGetenv(map[string]string{
		common.LaunchAgentPiExecutableEnvironment:     "/env/pi",
		common.LaunchAgentCodexExecutableEnvironment:  "/env/codex",
		common.LaunchAgentCursorExecutableEnvironment: "/env/cursor-agent",
	})
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "serve", "--daemon"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, filepath.Join(home, ".agent-whiteboard", "config.yaml"), manager.installConfig.ConfigPath)
	resolved, err := manager.installConfig.ExecutableResolver.LookPath(common.LaunchAgentProviderPi)
	require.NoError(t, err)
	require.Equal(t, "/env/pi", resolved)
	resolved, err = manager.installConfig.ExecutableResolver.LookPath(common.LaunchAgentProviderCodex)
	require.NoError(t, err)
	require.Equal(t, "/env/codex", resolved)
	resolved, err = manager.installConfig.ExecutableResolver.LookPath(common.LaunchAgentCursorExecutableName)
	require.NoError(t, err)
	require.Equal(t, "/env/cursor-agent", resolved)
}

func TestAgentDaemonInstallWithoutOverrideUsesOrdinaryResolver(t *testing.T) {
	manager := &fakeLaunchAgentManager{}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
	deps.ExecutablePath = func() (string, error) { return "/agent-whiteboard", nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "serve", "--daemon"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Nil(t, manager.installConfig.ExecutableResolver)
	require.Len(t, manager.installConfig.Providers, 3)
}

func TestAgentDaemonStatusRedactsLaunchAgentDetails(t *testing.T) {
	manager := &fakeLaunchAgentManager{status: common.LaunchAgentStatus{Installed: true, Loaded: true, Running: true, PID: 1234}}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
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
	for name, status := range map[string]common.LaunchAgentStatus{
		"running without PID":    {Installed: true, Loaded: true, Running: true},
		"running while unloaded": {Installed: true, Running: true, PID: 12},
		"stopped with PID":       {Installed: true, Loaded: true, PID: 12},
	} {
		t.Run(name, func(t *testing.T) {
			manager := &fakeLaunchAgentManager{status: status}
			deps := validDependencies()
			deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
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
			deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
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
		{name: "foreground Cursor", args: []string{"agent", "serve", "--cursor-executable="}, want: "Cursor executable must not be empty"},
		{name: "daemon Cursor", args: []string{"agent", "serve", "--daemon", "--cursor-executable="}, want: "Cursor executable must not be empty"},
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

func TestSelectedProviderExecutableResolverDefaultsCursorAgentWithoutGenericDiscovery(t *testing.T) {
	directory := t.TempDir()
	generic := filepath.Join(directory, "agent")
	require.NoError(t, os.WriteFile(generic, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", directory)
	resolver := selectedProviderExecutableResolver{}

	_, err := resolver.LookPath(common.LaunchAgentCursorExecutableName)
	require.ErrorIs(t, err, exec.ErrNotFound)
	cursorAgent := filepath.Join(directory, common.LaunchAgentCursorExecutableName)
	require.NoError(t, os.WriteFile(cursorAgent, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	resolved, err := resolver.LookPath(common.LaunchAgentCursorExecutableName)
	require.NoError(t, err)
	require.Equal(t, cursorAgent, resolved)
}

func TestSelectedProviderExecutableResolverMatchesCommonDescriptorCallPath(t *testing.T) {
	home := t.TempDir()
	agent := filepath.Join(home, "agent-whiteboard")
	cursorAgent := filepath.Join(home, "explicit-agent")
	require.NoError(t, os.WriteFile(agent, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	require.NoError(t, os.WriteFile(cursorAgent, []byte("#!/bin/sh\nexit 0\n"), 0o700))

	plist, err := common.GenerateLaunchAgentPlist(common.LaunchAgentConfig{
		Executable: agent,
		ConfigPath: filepath.Join(home, "config.yaml"),
		Providers: []common.LaunchAgentProviderDescriptor{
			cursorProviderDescriptor{},
		},
		ExecutableResolver: selectedProviderExecutableResolver{cursor: cursorAgent},
	}, home)
	require.NoError(t, err)
	require.Contains(t, string(plist), "<key>"+common.LaunchAgentCursorExecutableEnvironment+"</key>")
	resolvedCursorAgent, err := filepath.EvalSymlinks(cursorAgent)
	require.NoError(t, err)
	require.Contains(t, string(plist), "<string>"+resolvedCursorAgent+"</string>")
}

func TestSelectedProviderExecutableResolverRejectsMissingExplicitSelection(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resolver := selectedProviderExecutableResolver{codex: "missing-codex-selection", cursor: "missing-cursor-selection"}

	_, err := resolver.LookPath(common.LaunchAgentProviderCodex)
	require.Error(t, err)
	require.NotErrorIs(t, err, exec.ErrNotFound)
	require.ErrorContains(t, err, "missing-codex-selection")

	_, err = resolver.LookPath(common.LaunchAgentCursorExecutableName)
	require.Error(t, err)
	require.NotErrorIs(t, err, exec.ErrNotFound)
	require.ErrorContains(t, err, "missing-cursor-selection")

	_, err = resolver.LookPath(common.LaunchAgentProviderPi)
	require.ErrorIs(t, err, exec.ErrNotFound)
}

func TestAgentDaemonRejectsForegroundSettingsAndNilManager(t *testing.T) {
	for _, flag := range []string{"--port", "--provider-idle-timeout", "--shutdown-timeout"} {
		t.Run(flag, func(t *testing.T) {
			deps := validDependencies()
			deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) {
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
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return nil, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "daemon", "status"})
	require.EqualError(t, root.ExecuteContext(context.Background()), "launch agent manager factory returned nil")
}

func TestAgentDaemonUnsupportedErrorKeepsExactGuidance(t *testing.T) {
	manager := &fakeLaunchAgentManager{err: common.ErrLaunchAgentUnsupported}
	deps := validDependencies()
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"agent", "daemon", "status"}, deps)
	require.Equal(t, exitInternal, code)
	require.Empty(t, stdout.String())
	require.Equal(t, "Error: "+common.ErrLaunchAgentUnsupported.Error()+"\n", stderr.String())
}

func TestAgentDaemonOperationsDoNotLoadConfiguration(t *testing.T) {
	deps := validDependencies()
	deps.LoadConfig = func(string) (generalconfig.Config, error) {
		return generalconfig.Config{}, errors.New("configuration must not be loaded")
	}
	manager := &fakeLaunchAgentManager{}
	deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) { return manager, nil }
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
