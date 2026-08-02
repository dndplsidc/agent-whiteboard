package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrustCommandsUseGlobalConfigBeforeOrAfterSubcommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	deps := validDependencies()

	for _, args := range [][]string{
		{"--config", path, "agent", "trust", "add", "https://FIRST.example:443"},
		{"agent", "trust", "add", "https://second.example", "--config", path},
		{"agent", "--config", path, "trust", "add", "https://third.example"},
	} {
		root, err := NewRoot(deps)
		require.NoError(t, err)
		root.SetArgs(args)
		require.NoError(t, root.ExecuteContext(context.Background()))
	}

	var stdout bytes.Buffer
	deps.Stdout = &stdout
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "trust", "list", "--config", path})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, "https://first.example\nhttps://second.example\nhttps://third.example\n", stdout.String())
}

func TestTrustCommandsJSONContracts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	deps := validDependencies()

	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "add", args: []string{"agent", "trust", "add", "https://one.example", "--config", path, "--json"}, want: "{\"schema_version\":1}\n"},
		{name: "list", args: []string{"--json", "agent", "trust", "list", "--config", path}, want: "{\"schema_version\":1,\"origins\":[\"https://one.example\"]}\n"},
		{name: "remove", args: []string{"agent", "trust", "remove", "https://one.example", "--json", "--config", path}, want: "{\"schema_version\":1}\n"},
		{name: "empty list", args: []string{"agent", "trust", "list", "--json", "--config", path}, want: "{\"schema_version\":1,\"origins\":[]}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			deps.Stdout = &stdout
			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs(test.args)
			require.NoError(t, root.ExecuteContext(context.Background()))
			require.Equal(t, test.want, stdout.String())
		})
	}
}

func TestTrustHumanMutationsAreSilentAndRemoveIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	deps := validDependencies()
	var stdout bytes.Buffer
	deps.Stdout = &stdout

	for _, args := range [][]string{
		{"agent", "trust", "add", "https://one.example", "--config", path},
		{"agent", "trust", "remove", "https://one.example", "--config", path},
		{"agent", "trust", "remove", "https://one.example", "--config", path},
	} {
		root, err := NewRoot(deps)
		require.NoError(t, err)
		root.SetArgs(args)
		require.NoError(t, root.ExecuteContext(context.Background()))
	}
	require.Empty(t, stdout.String())
}

func TestTrustCommandErrorsAreActionableLocalUsageErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, test := range []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "invalid origin", args: []string{"agent", "trust", "add", "http://example.test", "--config", missing}, wantStderr: "Error: trusted origin must be an exact HTTPS origin\n"},
		{name: "explicit missing", args: []string{"agent", "trust", "list", "--config", missing}, wantStderr: "Error: configuration file does not exist\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), test.args, validDependencies())
			require.Equal(t, exitUsage, code)
			require.Empty(t, stdout.String())
			require.Equal(t, test.wantStderr, stderr.String())
		})
	}
}

func TestTrustListMissingDefaultIsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var stdout bytes.Buffer
	deps := validDependencies()
	deps.Stdout = &stdout
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"agent", "trust", "list", "--json"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Equal(t, "{\"schema_version\":1,\"origins\":[]}\n", stdout.String())
}
