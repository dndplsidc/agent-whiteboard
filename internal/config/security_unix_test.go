//go:build darwin || linux

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenConfigurationRejectsFinalSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := filepath.Join(dir, "original.yaml")
	victim := filepath.Join(dir, "victim.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	require.NoError(t, os.WriteFile(victim, []byte("version: 1\nagent:\n  port: 9999\n"), 0o600))

	file, err := openConfigurationWithHook(path, func() {
		require.NoError(t, os.Rename(path, backup))
		require.NoError(t, os.Symlink(victim, path))
	})
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, err)
}

func TestTrustedOriginListUsesOpenedDirectoryAfterParentPathSwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "config-dir")
	moved := filepath.Join(root, "opened-dir")
	path := filepath.Join(parent, "config.yaml")
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.NoError(t, os.WriteFile(path, []byte("version: 1\nagent:\n  trusted_origins:\n    - https://opened.example\n"), 0o600))

	ops := defaultEditFileOps()
	ops.afterDirectoryOpen = func() {
		require.NoError(t, os.Rename(parent, moved))
		require.NoError(t, os.Mkdir(parent, 0o700))
		require.NoError(t, os.WriteFile(path, []byte("version: 1\nagent:\n  trusted_origins:\n    - https://replacement.example\n"), 0o600))
	}
	origins, err := listTrustedOrigins(path, ops)
	require.NoError(t, err)
	require.Equal(t, []string{"https://opened.example"}, origins)
}

func TestTrustedOriginListRejectsFinalSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := filepath.Join(dir, "original.yaml")
	victim := filepath.Join(dir, "victim.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	require.NoError(t, os.WriteFile(victim, []byte("version: 1\nagent:\n  trusted_origins:\n    - https://victim.example\n"), 0o600))

	ops := defaultEditFileOps()
	ops.beforeTargetOpen = func() {
		require.NoError(t, os.Rename(path, backup))
		require.NoError(t, os.Symlink(victim, path))
	}
	_, err := listTrustedOrigins(path, ops)
	require.Error(t, err)
}

func TestTrustedOriginEditUsesOpenedDirectoryAfterParentPathSwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "config-dir")
	moved := filepath.Join(root, "opened-dir")
	path := filepath.Join(parent, "config.yaml")
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))

	ops := defaultEditFileOps()
	ops.afterDirectoryOpen = func() {
		require.NoError(t, os.Rename(parent, moved))
		require.NoError(t, os.Mkdir(parent, 0o700))
		require.NoError(t, os.WriteFile(path, []byte("version: 1\nagent:\n  trusted_origins:\n    - https://replacement.example\n"), 0o600))
	}
	require.NoError(t, editTrustedOrigin(path, "https://added.example", true, ops))
	require.Equal(t, []string{"https://added.example"}, mustTrustedOrigins(t, filepath.Join(moved, "config.yaml")))
	require.Equal(t, []string{"https://replacement.example"}, mustTrustedOrigins(t, path))
	require.FileExists(t, filepath.Join(moved, "config.yaml.lock"))
	require.NoFileExists(t, path+".lock")
}

func TestTrustedOriginEditRejectsTargetSwapBeforeCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	backup := filepath.Join(dir, "original.yaml")
	victim := filepath.Join(dir, "victim.yaml")
	victimContents := []byte("version: 1\nagent:\n  trusted_origins:\n    - https://victim.example\n")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	require.NoError(t, os.WriteFile(victim, victimContents, 0o600))

	ops := defaultEditFileOps()
	ops.beforeTargetCheck = func() {
		require.NoError(t, os.Rename(path, backup))
		require.NoError(t, os.Symlink(victim, path))
	}
	err := editTrustedOrigin(path, "https://added.example", true, ops)
	require.ErrorContains(t, err, "changed while editing")
	require.Equal(t, victimContents, mustReadFile(t, victim))
	require.Equal(t, "version: 1\n", string(mustReadFile(t, backup)))
}

func TestTrustedOriginOperationsRejectUntrustedWritableParent(t *testing.T) {
	for name, operation := range map[string]func(string) error{
		"list": func(path string) error {
			_, err := ListTrustedOrigins(path)
			return err
		},
		"edit": func(path string) error {
			return AddTrustedOrigin(path, "https://example.test")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
			require.NoError(t, os.Chmod(dir, 0o777))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			require.ErrorContains(t, operation(path), "must not be writable by group or others")
		})
	}
}
