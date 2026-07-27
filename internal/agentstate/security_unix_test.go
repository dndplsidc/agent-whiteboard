//go:build darwin || linux

package agentstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestStateLayoutUsesOwnerOnlyModes(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	key, err := ConversationKey(testIdentity())
	require.NoError(t, err)
	for _, path := range []string{store.Root(), filepath.Join(store.Root(), "conversations"), filepath.Join(store.Root(), "workspaces"), filepath.Join(store.Root(), "providers")} {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		require.True(t, info.IsDir())
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	for _, path := range []string{filepath.Join(store.Root(), "broker.lock"), filepath.Join(store.Root(), "conversations", key+".json")} {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		require.True(t, info.Mode().IsRegular())
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestStoreHoldsSingletonLockForLifetime(t *testing.T) {
	home := t.TempDir()
	first, err := Open(home)
	require.NoError(t, err)
	_, err = Open(home)
	require.ErrorIs(t, err, ErrLocked)
	require.NoError(t, first.Close())
	second, err := Open(home)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

func TestStoreRejectsUnsafeExistingStatePaths(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, home string){
		"state symlink": func(t *testing.T, home string) {
			require.NoError(t, os.Mkdir(filepath.Join(home, ".agent-whiteboard"), 0o700))
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(home, ".agent-whiteboard", "state")))
		},
		"accessible state": func(t *testing.T, home string) {
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".agent-whiteboard", "state"), 0o700))
			require.NoError(t, os.Chmod(filepath.Join(home, ".agent-whiteboard", "state"), 0o750))
		},
		"mapping symlink": func(t *testing.T, home string) {
			store, err := Open(home)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			key, err := ConversationKey(testIdentity())
			require.NoError(t, err)
			require.NoError(t, os.Symlink("missing", filepath.Join(home, ".agent-whiteboard", "state", "conversations", key+".json")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			prepare(t, home)
			store, err := Open(home)
			if store != nil {
				defer store.Close()
			}
			if name == "mapping symlink" {
				_, err = store.Load(testIdentity())
			}
			require.Error(t, err)
		})
	}
}

func TestLoadRejectsHardLinkedAndLooseMapping(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{name: "hard link", mutate: func(path string) error { return os.Link(path, path+".link") }},
		{name: "loose permissions", mutate: func(path string) error { return os.Chmod(path, 0o640) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
			_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
			require.NoError(t, err)
			key, err := ConversationKey(testIdentity())
			require.NoError(t, err)
			path := filepath.Join(store.Root(), "conversations", key+".json")
			require.NoError(t, test.mutate(path))
			_, err = store.Load(testIdentity())
			require.Error(t, err)
		})
	}
}

func TestStoreCleansSafeAbandonedTemporaryMappings(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	require.NoError(t, err)
	conversations := filepath.Join(store.Root(), "conversations")
	require.NoError(t, store.Close())
	temporary := filepath.Join(conversations, ".mapping.tmp-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.WriteFile(temporary, []byte("{}"), 0o600))
	store, err = Open(home)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	_, err = os.Stat(temporary)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestOwnershipValidationRejectsAnotherOwner(t *testing.T) {
	stat := unix.Stat_t{Mode: unix.S_IFREG | 0o600, Nlink: 1, Uid: uint32(os.Geteuid() + 1)}
	require.Error(t, validateRegularStat(&stat, 0o600))
}

func TestCommitOutcomesAndTemporaryCleanup(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	session := testSession(t, testID, "sessions/current", now)

	store.ops.rename = func(_, _ string) error { return errors.New("rename failed") }
	outcome, err := store.Create(testIdentity(), session, now)
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	entries, readErr := os.ReadDir(filepath.Join(store.Root(), "conversations"))
	require.NoError(t, readErr)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".mapping.tmp-"))
	}

	store.ops = defaultFileOps(store.conversations)
	_, err = store.Create(testIdentity(), session, now)
	require.NoError(t, err)
	store.ops.syncDir = func() error { return errors.New("sync failed") }
	outcome, err = store.Update(testIdentity(), func(mapping *Mapping) error {
		mapping.Current.ModelLabel = "new-model"
		return nil
	})
	require.Error(t, err)
	require.Equal(t, CommitUncertain, outcome)
	loaded, loadErr := store.Load(testIdentity())
	require.NoError(t, loadErr)
	require.Equal(t, "new-model", loaded.Current.ModelLabel)
}

func TestRenameErrorReportsAppliedWhenExpectedMappingWasPublished(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	realRename := store.conversations.rename
	store.ops.rename = func(oldName, newName string) error {
		require.NoError(t, realRename(oldName, newName))
		return errors.New("post-rename failure")
	}
	outcome, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.Error(t, err)
	require.Equal(t, CommitApplied, outcome)
	_, err = store.Load(testIdentity())
	require.NoError(t, err)
}

func TestWorkspaceRemovalIsRecursiveButRejectsUnsafeEntries(t *testing.T) {
	store := openTestStore(t)
	workspace, err := store.EnsureWorkspace(testID)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(workspace, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "nested", "state"), []byte("x"), 0o600))
	require.NoError(t, store.RemoveWorkspace(testID))

	workspace, err = store.EnsureWorkspace(testID)
	require.NoError(t, err)
	require.NoError(t, os.Symlink("missing", filepath.Join(workspace, "unsafe")))
	require.Error(t, store.RemoveWorkspace(testID))
	_, err = os.Lstat(workspace)
	require.NoError(t, err, "failed cleanup remains discoverable for retry")
}

func TestOperationsRemainBoundToOpenedStateDirectory(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	require.NoError(t, err)
	defer store.Close()
	original := store.Root()
	moved := original + ".moved"
	require.NoError(t, os.Rename(original, moved))
	require.NoError(t, os.MkdirAll(original, 0o700))

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err = store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	key, err := ConversationKey(testIdentity())
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(moved, "conversations", key+".json"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(original, "conversations", key+".json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
