package launchagent

import (
	"context"
	"errors"
	"os/exec"
)

const (
	Label                       = "com.agent-whiteboard.local-agent"
	LaunchctlExecutable         = "/bin/launchctl"
	ProviderPi                  = "pi"
	PiExecutableEnvironment     = "AGENT_WHITEBOARD_PI_EXECUTABLE"
	ForegroundGuidance          = "managed agent services are unsupported on this platform; run `agent-whiteboard agent serve` in the foreground"
	installedNotRunningGuidance = "LaunchAgent is installed but not running"
	notInstalledGuidance        = "LaunchAgent is not installed"
)

var (
	ErrUnsupported         = errors.New(ForegroundGuidance)
	ErrInstalledNotRunning = errors.New(installedNotRunningGuidance)
	ErrNotInstalled        = errors.New(notInstalledGuidance)
	// ErrNotLoaded lets injected runners report launchctl's ordinary missing-service result.
	ErrNotLoaded = errors.New("LaunchAgent is not loaded")
)

// Config contains the durable inputs recorded in the LaunchAgent. Paths must
// be absolute. Install resolves symlinks and records only real paths.
type Config struct {
	Executable          string
	ConfigPath          string
	ProviderExecutables map[string]string
}

// Status intentionally excludes launch arguments, paths, environment, and raw
// launchctl output.
type Status struct {
	Installed bool
	Loaded    bool
	Running   bool
	PID       int
}

// Runner executes one process-control command. Manager implementations pass an
// absolute command and a complete argument list; no shell is used.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// ExecRunner runs commands directly without a shell and returns combined output.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, command, args...).CombinedOutput()
}

// Manager owns the per-user managed-agent lifecycle.
type Manager interface {
	Install(context.Context, Config) error
	Status(context.Context) (Status, error)
	Restart(context.Context) error
	Stop(context.Context) error
	Uninstall(context.Context) error
}

type servicePaths struct {
	Plist     string
	StdoutLog string
	StderrLog string
}
