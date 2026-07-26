package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrustedOriginEditsPreserveSparseYAMLCommentsOrderAndIdempotence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := `# configuration comment
version: 1
server:
  # keep this setting
  port: 9000
agent:
  port: 8568
  trusted_origins:
    - https://first.example # first origin
`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o640))

	require.NoError(t, AddTrustedOrigin(path, "https://SECOND.example:443"))
	loaded, err := Load(path)
	require.NoError(t, err)
	origins, set := loaded.Agent().TrustedOrigins()
	require.True(t, set)
	require.Equal(t, []string{"https://first.example", "https://second.example"}, origins)
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(contents), "# configuration comment")
	require.Contains(t, string(contents), "# keep this setting")
	require.Contains(t, string(contents), "# first origin")
	port, set := loaded.Server().Port()
	require.True(t, set)
	require.Equal(t, 9000, port)

	beforeIdempotent := append([]byte(nil), contents...)
	require.NoError(t, AddTrustedOrigin(path, "https://second.example"))
	contents, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeIdempotent, contents)

	require.NoError(t, RemoveTrustedOrigin(path, "https://first.example"))
	require.Equal(t, []string{"https://second.example"}, mustTrustedOrigins(t, path))
	contents, err = os.ReadFile(path)
	require.NoError(t, err)
	beforeIdempotent = append([]byte(nil), contents...)
	require.NoError(t, RemoveTrustedOrigin(path, "https://first.example"))
	contents, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeIdempotent, contents)
}

func TestTrustedOriginDefaultPathCreationAndExplicitMissingPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origins, err := ListTrustedOrigins("")
	require.NoError(t, err)
	require.Empty(t, origins)
	require.NoDirExists(t, filepath.Join(home, ".agent-whiteboard"))

	require.NoError(t, AddTrustedOrigin("", "https://example.test"))
	path := filepath.Join(home, ".agent-whiteboard", "config.yaml")
	require.Equal(t, []string{"https://example.test"}, mustTrustedOrigins(t, path))
	assertPermissions(t, filepath.Dir(path), 0o700)
	assertPermissions(t, path, 0o600)
	assertPermissions(t, path+".lock", 0o600)

	explicit := filepath.Join(t.TempDir(), "missing.yaml")
	for name, operation := range map[string]func() error{
		"add":    func() error { return AddTrustedOrigin(explicit, "https://example.test") },
		"remove": func() error { return RemoveTrustedOrigin(explicit, "https://example.test") },
		"list": func() error {
			_, listErr := ListTrustedOrigins(explicit)
			return listErr
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, operation(), os.ErrNotExist)
			require.NoFileExists(t, explicit)
			require.NoFileExists(t, explicit+".lock")
		})
	}
}

func TestTrustedOriginOperationsRejectUnsafeTargetsAndInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.yaml")
	require.NoError(t, os.WriteFile(realPath, []byte("version: 1\n"), 0o600))
	symlinkPath := filepath.Join(dir, "link.yaml")
	require.NoError(t, os.Symlink(realPath, symlinkPath))
	directoryPath := filepath.Join(dir, "directory.yaml")
	require.NoError(t, os.Mkdir(directoryPath, 0o700))
	invalidPath := filepath.Join(dir, "invalid.yaml")
	invalid := []byte("version: 1\nunknown: true\n")
	require.NoError(t, os.WriteFile(invalidPath, invalid, 0o600))
	worldWritablePath := filepath.Join(dir, "world-writable.yaml")
	require.NoError(t, os.WriteFile(worldWritablePath, []byte("version: 1\n"), 0o600))
	require.NoError(t, os.Chmod(worldWritablePath, 0o666))

	for _, path := range []string{symlinkPath, directoryPath} {
		for _, operation := range []func(string) error{
			func(target string) error { return AddTrustedOrigin(target, "https://example.test") },
			func(target string) error { _, err := ListTrustedOrigins(target); return err },
		} {
			require.ErrorContains(t, operation(path), "regular file")
		}
	}

	for _, operation := range []func() error{
		func() error { return AddTrustedOrigin(worldWritablePath, "https://example.test") },
		func() error { _, err := ListTrustedOrigins(worldWritablePath); return err },
	} {
		require.ErrorContains(t, operation(), "must not be writable by group or others")
	}

	require.Error(t, AddTrustedOrigin(realPath, "http://example.test"))
	require.Equal(t, "version: 1\n", string(mustReadFile(t, realPath)))
	require.ErrorContains(t, AddTrustedOrigin(invalidPath, "https://example.test"), "unknown field")
	require.Equal(t, invalid, mustReadFile(t, invalidPath))
}

func TestTrustedOriginAtomicCommitFailureSemantics(t *testing.T) {
	t.Run("pre-commit sync failure leaves old file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		old := []byte("version: 1\nagent:\n  trusted_origins:\n    - https://old.example\n")
		require.NoError(t, os.WriteFile(path, old, 0o600))
		injected := errors.New("injected file sync failure")
		ops := defaultEditFileOps()
		ops.syncFile = func(*os.File) error { return injected }

		err := editTrustedOrigin(path, "https://new.example", true, ops)
		require.ErrorIs(t, err, injected)
		require.Equal(t, old, mustReadFile(t, path))
		matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".config.yaml.tmp-*"))
		require.NoError(t, globErr)
		require.Empty(t, matches)
	})

	t.Run("rename failure observes same-directory owner-only temp and leaves old file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		old := []byte("version: 1\n")
		require.NoError(t, os.WriteFile(path, old, 0o600))
		injected := errors.New("injected rename failure")
		ops := defaultEditFileOps()
		ops.rename = func(_ *configDirectory, oldName, newName string) error {
			require.Equal(t, filepath.Base(path), newName)
			assertPermissions(t, filepath.Join(filepath.Dir(path), oldName), 0o600)
			return injected
		}

		err := editTrustedOrigin(path, "https://new.example", true, ops)
		require.ErrorIs(t, err, injected)
		require.Equal(t, old, mustReadFile(t, path))
	})

	t.Run("post-rename sync failure reports uncertain committed success", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
		injected := errors.New("injected directory sync failure")
		ops := defaultEditFileOps()
		ops.syncDir = func(*configDirectory) error { return injected }

		err := editTrustedOrigin(path, "https://new.example", true, ops)
		var uncertain *CommitUncertainError
		require.ErrorAs(t, err, &uncertain)
		require.ErrorIs(t, err, injected)
		require.Equal(t, []string{"https://new.example"}, mustTrustedOrigins(t, path))
	})
}

func TestTrustedOriginConcurrentEditorsDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\nagent:\n  trusted_origins:\n    - https://initial.example\n"), 0o600))

	const editors = 32
	var wait sync.WaitGroup
	errorsByEditor := make(chan error, editors)
	for index := 0; index < editors; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errorsByEditor <- AddTrustedOrigin(path, fmt.Sprintf("https://host-%02d.example", index))
		}(index)
	}
	wait.Wait()
	close(errorsByEditor)
	for err := range errorsByEditor {
		require.NoError(t, err)
	}

	origins := mustTrustedOrigins(t, path)
	require.Len(t, origins, editors+1)
	require.Equal(t, "https://initial.example", origins[0])
	got := append([]string(nil), origins[1:]...)
	sort.Strings(got)
	want := make([]string, editors)
	for index := range want {
		want[index] = fmt.Sprintf("https://host-%02d.example", index)
	}
	require.Equal(t, want, got)
}

func TestTrustedOriginEditRejectsLockSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	require.NoError(t, os.Symlink(path, path+".lock"))

	err := AddTrustedOrigin(path, "https://example.test")
	require.Error(t, err)
	require.Equal(t, "version: 1\n", string(mustReadFile(t, path)))
}

func mustTrustedOrigins(t *testing.T, selectedPath string) []string {
	t.Helper()
	origins, err := ListTrustedOrigins(selectedPath)
	require.NoError(t, err)
	return origins
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return contents
}

func assertPermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm(), strings.TrimSpace(path))
}
