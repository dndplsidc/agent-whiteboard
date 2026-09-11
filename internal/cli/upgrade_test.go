package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	generalconfig "github.com/dndplsidc/agent-whiteboard/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUpgradeResolvesLatestDirectlyAndAtomicallyInstallsExactVersion(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, upgradeExecutableName)
	require.NoError(t, os.WriteFile(executable, []byte("old binary"), 0o755))
	deps := validDependencies()
	var stderr, stdout bytes.Buffer
	deps.Stdout, deps.Stderr = &stdout, &stderr
	deps.ExecutablePath = func() (string, error) { return executable, nil }
	deps.Environ = func() []string {
		return []string{"PATH=/usr/bin", "GOPROXY=https://stale.example", "GOBIN=/wrong", "HOME=/home/test"}
	}
	deps.LookPath = func(name string) (string, error) {
		require.Equal(t, "go", name)
		return "/usr/bin/go", nil
	}
	deps.ReadBuildVersion = func(path string) (string, error) {
		if path == executable {
			return "v0.2.1", nil
		}
		return "v0.2.4", nil
	}
	var stagingDirectory string
	call := 0
	deps.RunCommand = func(_ context.Context, command string, arguments, environment []string, commandStdout, commandStderr io.Writer) error {
		call++
		require.Equal(t, "/usr/bin/go", command)
		switch call {
		case 1:
			require.Equal(t, []string{"list", "-m", "-versions", "-json", upgradeModule + "@latest"}, arguments)
			require.Equal(t, "direct", environmentValue(environment, "GOPROXY"))
			_, _ = io.WriteString(commandStdout, upgradeResolution("v0.2.4", "v0.2.1", "v0.2.4"))
		case 2:
			require.Equal(t, []string{"install", upgradePackage + "@v0.2.4"}, arguments)
			require.Equal(t, "https://stale.example", environmentValue(environment, "GOPROXY"))
			stagingDirectory = environmentValue(environment, "GOBIN")
			require.Equal(t, directory, filepath.Dir(stagingDirectory))
			require.Contains(t, filepath.Base(stagingDirectory), ".agent-whiteboard-upgrade-")
			require.Same(t, deps.Stderr, commandStdout)
			require.Same(t, deps.Stderr, commandStderr)
			require.NoError(t, os.WriteFile(filepath.Join(stagingDirectory, upgradeExecutableName), []byte("new binary"), 0o755))
		default:
			t.Fatalf("unexpected command call %d", call)
		}
		return nil
	}
	deps.LoadConfig = func(string) (generalconfig.Config, error) {
		return generalconfig.Config{}, errors.New("configuration must not be loaded")
	}

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	require.Equal(t, 2, call)
	installed, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(installed))
	require.NoDirExists(t, stagingDirectory)
	require.Equal(t, "Agent Whiteboard upgraded to v0.2.4.\n", stdout.String())
	require.Empty(t, stderr.String())
}

func TestUpgradeAlreadyCurrentDoesNotReplaceExecutable(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, upgradeExecutableName)
	require.NoError(t, os.WriteFile(executable, []byte("current binary"), 0o755))
	deps := upgradeTestDependencies(t, executable, "v0.2.4")
	var stdout bytes.Buffer
	deps.Stdout = &stdout
	deps.RunCommand = func(_ context.Context, _ string, arguments, _ []string, commandStdout, _ io.Writer) error {
		require.Equal(t, "list", arguments[0])
		_, _ = io.WriteString(commandStdout, upgradeResolution("v0.2.4", "v0.2.3", "v0.2.4"))
		return nil
	}

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	contents, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "current binary", string(contents))
	require.Equal(t, "Agent Whiteboard is already up to date at v0.2.4.\n", stdout.String())
}

func TestUpgradeAlreadyCurrentJSONKeepsResolverOutputOutOfMachineStreams(t *testing.T) {
	executable := filepath.Join(t.TempDir(), upgradeExecutableName)
	deps := upgradeTestDependencies(t, executable, "v0.2.4")
	deps.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, commandStdout, commandStderr io.Writer) error {
		_, _ = io.WriteString(commandStdout, upgradeResolution("v0.2.4", "v0.2.3", "v0.2.4"))
		_, _ = io.WriteString(commandStderr, "resolver chatter")
		return nil
	}
	var stdout, stderr bytes.Buffer
	deps.Stdout, deps.Stderr = &stdout, &stderr

	code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"--json", "upgrade"}, deps)

	require.Equal(t, exitSuccess, code)
	require.JSONEq(t, `{"schema_version":1}`, stdout.String())
	require.Empty(t, stderr.String())
}

func TestUpgradeRefusesDowngradeBeforeInstalling(t *testing.T) {
	executable := filepath.Join(t.TempDir(), upgradeExecutableName)
	deps := upgradeTestDependencies(t, executable, "v0.2.4")
	call := 0
	deps.RunCommand = func(_ context.Context, _ string, arguments, _ []string, commandStdout, _ io.Writer) error {
		call++
		require.Equal(t, "list", arguments[0])
		_, _ = io.WriteString(commandStdout, upgradeResolution("v0.2.3", "v0.2.3", "v0.2.4"))
		return nil
	}

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, "refusing to downgrade Agent Whiteboard from v0.2.4 to v0.2.3")
	require.Equal(t, 1, call)
}

func TestUpgradeRefusesUnknownCurrentRelease(t *testing.T) {
	executable := filepath.Join(t.TempDir(), upgradeExecutableName)
	deps := upgradeTestDependencies(t, executable, "v0.2.2")
	deps.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, commandStdout, _ io.Writer) error {
		_, _ = io.WriteString(commandStdout, upgradeResolution("v0.2.4", "v0.2.3", "v0.2.4"))
		return nil
	}

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, "cannot verify current Agent Whiteboard version v0.2.2 against available releases")
}

func TestUpgradeVerificationFailurePreservesExistingExecutable(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, upgradeExecutableName)
	require.NoError(t, os.WriteFile(executable, []byte("old binary"), 0o755))
	deps := upgradeTestDependencies(t, executable, "v0.2.3")
	var stagingDirectory string
	call := 0
	deps.ReadBuildVersion = func(path string) (string, error) {
		if path == executable {
			return "v0.2.3", nil
		}
		return "v0.2.2", nil
	}
	deps.RunCommand = func(_ context.Context, _ string, arguments, environment []string, commandStdout, _ io.Writer) error {
		call++
		if arguments[0] == "list" {
			_, _ = io.WriteString(commandStdout, upgradeResolution("v0.2.4", "v0.2.3", "v0.2.4"))
			return nil
		}
		stagingDirectory = environmentValue(environment, "GOBIN")
		return os.WriteFile(filepath.Join(stagingDirectory, upgradeExecutableName), []byte("wrong binary"), 0o755)
	}

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, "downloaded Agent Whiteboard version v0.2.2; expected v0.2.4")
	require.Equal(t, 2, call)
	contents, readErr := os.ReadFile(executable)
	require.NoError(t, readErr)
	require.Equal(t, "old binary", string(contents))
	require.NoDirExists(t, stagingDirectory)
}

func TestUpgradeInstallerFailurePreservesExistingExecutableAndReportsJSON(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, upgradeExecutableName)
	require.NoError(t, os.WriteFile(executable, []byte("old binary"), 0o755))
	deps := upgradeTestDependencies(t, executable, "v0.2.3")
	deps.RunCommand = func(_ context.Context, _ string, arguments, _ []string, commandStdout, commandStderr io.Writer) error {
		if arguments[0] == "list" {
			_, _ = io.WriteString(commandStdout, upgradeResolution("v0.2.4", "v0.2.3", "v0.2.4"))
			return nil
		}
		_, _ = io.WriteString(commandStderr, "permission denied")
		return errors.New("exit status 1")
	}
	var stdout, stderr bytes.Buffer
	deps.Stdout, deps.Stderr = &stdout, &stderr

	code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"--json", "upgrade"}, deps)

	require.Equal(t, exitInternal, code)
	require.Empty(t, stdout.String())
	require.JSONEq(t, `{"schema_version":1,"error":{"code":"internal_error","message":"upgrade Agent Whiteboard: permission denied"}}`, stderr.String())
	contents, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "old binary", string(contents))
}

func TestUpgradeResolverFailureIsActionableAndPreservesCancellation(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  error
		output string
		want   string
		wantIs error
	}{
		{name: "diagnostic", cause: errors.New("exit status 1"), output: "direct lookup failed", want: "resolve latest Agent Whiteboard release: direct lookup failed"},
		{name: "canceled", cause: context.Canceled, want: "context canceled", wantIs: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), upgradeExecutableName)
			deps := upgradeTestDependencies(t, executable, "v0.2.3")
			deps.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, _, commandStderr io.Writer) error {
				_, _ = io.WriteString(commandStderr, test.output)
				return test.cause
			}

			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs([]string{"upgrade"})
			err = root.ExecuteContext(context.Background())

			require.EqualError(t, err, test.want)
			if test.wantIs != nil {
				require.ErrorIs(t, err, test.wantIs)
			}
		})
	}
}

func TestUpgradeRejectsMalformedResolution(t *testing.T) {
	executable := filepath.Join(t.TempDir(), upgradeExecutableName)
	deps := upgradeTestDependencies(t, executable, "v0.2.3")
	deps.RunCommand = func(_ context.Context, _ string, _ []string, _ []string, commandStdout, _ io.Writer) error {
		_, _ = io.WriteString(commandStdout, `{"Path":"wrong.example/module","Version":"v0.2.4","Versions":["v0.2.3","v0.2.4"]}`)
		return nil
	}

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, "resolve latest Agent Whiteboard release: unexpected module metadata")
}

func TestUpgradeRejectsNonstandardExecutableName(t *testing.T) {
	deps := validDependencies()
	deps.ExecutablePath = func() (string, error) { return filepath.Join(t.TempDir(), "awb"), nil }

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, `cannot upgrade executable named "awb"; expected "agent-whiteboard"`)
}

func TestUpgradeReportsMissingGo(t *testing.T) {
	deps := validDependencies()
	deps.ExecutablePath = func() (string, error) { return filepath.Join(t.TempDir(), upgradeExecutableName), nil }
	deps.LookPath = func(string) (string, error) { return "", errors.New("not found") }

	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"upgrade"})
	err = root.ExecuteContext(context.Background())

	require.EqualError(t, err, "cannot upgrade Agent Whiteboard: Go is not available on PATH")
}

func upgradeTestDependencies(t *testing.T, executable, currentVersion string) Dependencies {
	t.Helper()
	deps := validDependencies()
	deps.ExecutablePath = func() (string, error) { return executable, nil }
	deps.Environ = func() []string { return []string{"PATH=/usr/bin", "GOPROXY=https://stale.example"} }
	deps.LookPath = func(string) (string, error) { return "/usr/bin/go", nil }
	deps.ReadBuildVersion = func(string) (string, error) { return currentVersion, nil }
	return deps
}

func upgradeResolution(version string, versions ...string) string {
	result := `{"Path":"` + upgradeModule + `","Version":"` + version + `","Query":"latest","Versions":[`
	for index, available := range versions {
		if index > 0 {
			result += ","
		}
		result += `"` + available + `"`
	}
	return result + `]}`
}

func environmentValue(environment []string, key string) string {
	prefix := key + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
