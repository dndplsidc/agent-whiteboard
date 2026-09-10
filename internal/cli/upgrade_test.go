package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	generalconfig "github.com/dndplsidc/agent-whiteboard/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUpgradeInstallsLatestIntoCurrentExecutableDirectory(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent-whiteboard")
	var gotCommand string
	var gotArguments, gotEnvironment []string
	deps := validDependencies()
	var installerOutput bytes.Buffer
	deps.Stderr = &installerOutput
	deps.ExecutablePath = func() (string, error) { return executable, nil }
	deps.Environ = func() []string { return []string{"PATH=/usr/bin", "GOBIN=/wrong", "HOME=/home/test"} }
	deps.LookPath = func(name string) (string, error) {
		require.Equal(t, "go", name)
		return "/usr/bin/go", nil
	}
	deps.RunCommand = func(_ context.Context, command string, arguments, environment []string, stdout, stderr io.Writer) error {
		gotCommand = command
		gotArguments = append([]string(nil), arguments...)
		gotEnvironment = append([]string(nil), environment...)
		require.Equal(t, deps.Stderr, stdout)
		require.Equal(t, deps.Stderr, stderr)
		return nil
	}
	deps.LoadConfig = func(string) (generalconfig.Config, error) {
		return generalconfig.Config{}, errors.New("configuration must not be loaded")
	}
	var stdout bytes.Buffer
	deps.Stdout = &stdout

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	require.Equal(t, "/usr/bin/go", gotCommand)
	require.Equal(t, []string{"install", "github.com/dndplsidc/agent-whiteboard/cmd/agent-whiteboard@latest"}, gotArguments)
	require.ElementsMatch(t, []string{"PATH=/usr/bin", "HOME=/home/test", "GOBIN=" + directory}, gotEnvironment)
	require.Equal(t, "Agent Whiteboard upgraded successfully.\n", stdout.String())
}

func TestUpgradeJSONKeepsInstallerOutputOutOfMachineStreams(t *testing.T) {
	directory := t.TempDir()
	deps := validDependencies()
	deps.ExecutablePath = func() (string, error) { return filepath.Join(directory, "agent-whiteboard"), nil }
	deps.Environ = func() []string { return nil }
	deps.LookPath = func(string) (string, error) { return "/usr/bin/go", nil }
	deps.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stdout, "download chatter\n")
		_, _ = io.WriteString(stderr, "build chatter\n")
		return nil
	}
	var stdout, stderr bytes.Buffer
	deps.Stdout, deps.Stderr = &stdout, &stderr

	code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"--json", "upgrade"}, deps)

	require.Equal(t, exitSuccess, code)
	require.JSONEq(t, `{"schema_version":1}`, stdout.String())
	require.Empty(t, stderr.String())
}

func TestUpgradeRejectsNonstandardExecutableName(t *testing.T) {
	deps := validDependencies()
	deps.ExecutablePath = func() (string, error) { return filepath.Join(t.TempDir(), "awb"), nil }
	deps.Environ = func() []string { return nil }
	deps.LookPath = func(string) (string, error) { return "/usr/bin/go", nil }
	deps.RunCommand = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		t.Fatal("installer must not run")
		return nil
	}

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, `cannot upgrade executable named "awb"; expected "agent-whiteboard"`)
}

func TestUpgradeReportsMissingGo(t *testing.T) {
	deps := validDependencies()
	deps.ExecutablePath = func() (string, error) { return filepath.Join(t.TempDir(), "agent-whiteboard"), nil }
	deps.Environ = func() []string { return nil }
	deps.LookPath = func(string) (string, error) { return "", errors.New("not found") }

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, "cannot upgrade Agent Whiteboard: Go is not available on PATH")
}

func TestUpgradePreservesInstallerFailureAndOutput(t *testing.T) {
	directory := t.TempDir()
	deps := validDependencies()
	deps.ExecutablePath = func() (string, error) { return filepath.Join(directory, "agent-whiteboard"), nil }
	deps.Environ = func() []string { return nil }
	deps.LookPath = func(string) (string, error) { return "/usr/bin/go", nil }
	deps.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "permission denied")
		return errors.New("exit status 1")
	}
	var stdout, stderr bytes.Buffer
	deps.Stdout, deps.Stderr = &stdout, &stderr

	code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"--json", "upgrade"}, deps)

	require.Equal(t, exitInternal, code)
	require.Empty(t, stdout.String())
	require.JSONEq(t, `{"schema_version":1,"error":{"code":"internal_error","message":"upgrade Agent Whiteboard: permission denied"}}`, stderr.String())
}
