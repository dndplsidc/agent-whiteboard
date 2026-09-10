package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareCatalogRootSyncsNewDirectoryEdges(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.Chmod(home, 0o755))
			parent := filepath.Join(home, ".agent-whiteboard")
			root := filepath.Join(parent, "catalog")
			syncFailure := errors.New("directory sync failed")
			var synced []string
			err := prepareCatalogRoot(root, func(path string) error {
				assertPrivatePath(t, path, 0o700)
				synced = append(synced, path)
				if path == parent {
					require.NoDirExists(t, root, "parent must be durable before creating catalog")
				}
				if len(synced) == failAt {
					return syncFailure
				}
				return syncParentDirectory(path)
			})
			if failAt == 0 {
				require.NoError(t, err)
				require.Equal(t, []string{parent, root}, synced)
			} else {
				require.ErrorIs(t, err, syncFailure)
				require.Len(t, synced, failAt)
			}
			assertPrivatePath(t, home, 0o755)
			// A failed sync leaves the directory present. A retry must still
			// establish both edges before reporting readiness.
			synced = nil
			require.NoError(t, prepareCatalogRoot(root, func(path string) error {
				synced = append(synced, path)
				return syncParentDirectory(path)
			}))
			require.Equal(t, []string{parent, root}, synced)
		})
	}
}

func TestPrepareCatalogRootPreservesExistingParentPermissions(t *testing.T) {
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o755))
	root := filepath.Join(parent, "catalog")
	var synced []string
	require.NoError(t, prepareCatalogRoot(root, func(path string) error {
		synced = append(synced, path)
		return syncParentDirectory(path)
	}))
	require.Equal(t, []string{parent, root}, synced)
	assertPrivatePath(t, parent, 0o755)
}
