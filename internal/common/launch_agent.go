package common

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
)

const (
	LaunchAgentLabel                      = "com.agent-whiteboard.local-agent"
	LaunchAgentControlExecutable          = "/bin/launchctl"
	LaunchAgentProviderPi                 = "pi"
	LaunchAgentProviderCodex              = "codex"
	LaunchAgentPiExecutableEnvironment    = "AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE"
	LaunchAgentCodexExecutableEnvironment = "AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE"
	LaunchAgentPathEnvironment            = "PATH"
	installedNotRunningGuidance           = "LaunchAgent is installed but not running"
	notInstalledGuidance                  = "LaunchAgent is not installed"
)

var (
	LaunchAgentForegroundGuidance     = unsupportedGuidance(runtime.GOOS)
	ErrLaunchAgentUnsupported         = errors.New(LaunchAgentForegroundGuidance)
	ErrLaunchAgentInstalledNotRunning = errors.New(installedNotRunningGuidance)
	ErrLaunchAgentNotInstalled        = errors.New(notInstalledGuidance)
	// ErrLaunchAgentNotLoaded lets injected runners report launchctl's ordinary missing-service result.
	ErrLaunchAgentNotLoaded = errors.New("LaunchAgent is not loaded")
)

func unsupportedGuidance(goos string) string {
	return "managed agent daemon is unsupported on " + goos + "; run 'agent-whiteboard agent serve' in the foreground"
}

// LaunchAgentProviderDescriptor describes a registered provider executable. Install only
// accepts the package's fixed provider names and executable names.
type LaunchAgentProviderDescriptor interface {
	ProviderName() string
	ExecutableName() string
}

// LaunchAgentExecutableResolver resolves an executable name using the current process
// environment. Implementations must return exec.ErrNotFound when it is absent.
type LaunchAgentExecutableResolver interface {
	LookPath(string) (string, error)
}

type pathResolver struct{}

func (pathResolver) LookPath(name string) (string, error) { return exec.LookPath(name) }

// LaunchAgentConfig contains the durable inputs recorded in the LaunchAgent. Paths must
// be absolute and clean. ConfigPath may name an absent final file. EnvironmentPath is the
// installing process's runtime PATH; plist generation sanitizes and supplements it.
type LaunchAgentConfig struct {
	Executable         string
	ConfigPath         string
	Providers          []LaunchAgentProviderDescriptor
	ExecutableResolver LaunchAgentExecutableResolver
	EnvironmentPath    string
}

// LaunchAgentStatus intentionally excludes launch arguments, paths, environment, and raw
// launchctl output.
type LaunchAgentStatus struct {
	Installed bool
	Loaded    bool
	Running   bool
	PID       int
}

// LaunchAgentRunner executes one process-control command. LaunchAgentManager implementations pass an
// absolute command and a complete argument list; no shell is used.
type LaunchAgentRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// LaunchAgentExecRunner runs commands directly without a shell and returns combined output.
type LaunchAgentExecRunner struct{}

func (LaunchAgentExecRunner) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, command, args...).CombinedOutput()
}

// LaunchAgentManager owns the per-user managed-agent lifecycle.
type LaunchAgentManager interface {
	Install(context.Context, LaunchAgentConfig) error
	Status(context.Context) (LaunchAgentStatus, error)
	Restart(context.Context) error
	Stop(context.Context) error
	Uninstall(context.Context) error
}

type servicePaths struct {
	Home      string
	Plist     string
	StdoutLog string
	StderrLog string
}
