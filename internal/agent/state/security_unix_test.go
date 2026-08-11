//go:build darwin || linux

package state

import (
	"errors"
	"os"
	"os/exec"
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

func TestStoreLockCoordinatesAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	require.NoError(t, err)
	defer store.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestStoreLockHelper$")
	command.Env = append(os.Environ(), "AGENTSTATE_LOCK_HELPER_HOME="+home)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestStoreLockHelper(t *testing.T) {
	home := os.Getenv("AGENTSTATE_LOCK_HELPER_HOME")
	if home == "" {
		return
	}
	store, err := Open(home)
	if store != nil {
		_ = store.Close()
	}
	require.ErrorIs(t, err, ErrLocked)
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

	store.ops.publish = func(_, _ string, _ *fileIdentity, _ fileIdentity) error { return errors.New("rename failed") }
	outcome, err := store.Create(testIdentity(), session, now)
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	entries, readErr := os.ReadDir(filepath.Join(store.Root(), "conversations"))
	require.NoError(t, readErr)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".mapping.tmp-"))
	}

	store.ops = defaultFileOps(store.conversations, store.workspaces)
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
	realPublish := store.conversations.publish
	store.ops.publish = func(oldName, newName string, original *fileIdentity, replacement fileIdentity) error {
		require.NoError(t, realPublish(oldName, newName, original, replacement))
		return errors.New("post-rename failure")
	}
	outcome, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.Error(t, err)
	require.Equal(t, CommitApplied, outcome)
	_, err = store.Load(testIdentity())
	require.NoError(t, err)
}

func TestAtomicMappingOperationsPreserveUnexpectedSwappedTargets(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	unexpected := []byte(`{"unexpected":true}`)

	t.Run("create", func(t *testing.T) {
		store := openTestStore(t)
		key, err := ConversationKey(testIdentity())
		require.NoError(t, err)
		path := filepath.Join(store.Root(), "conversations", key+".json")
		realPublish := store.conversations.publish
		store.ops.publish = func(oldName, newName string, original *fileIdentity, replacement fileIdentity) error {
			require.NoError(t, os.WriteFile(path, unexpected, 0o600))
			return realPublish(oldName, newName, original, replacement)
		}
		outcome, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
		require.Error(t, err)
		require.Equal(t, CommitNotApplied, outcome)
		actual, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, unexpected, actual)
	})

	for _, operation := range []string{"update", "remove"} {
		t.Run(operation, func(t *testing.T) {
			store := openTestStore(t)
			_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
			require.NoError(t, err)
			key, err := ConversationKey(testIdentity())
			require.NoError(t, err)
			path := filepath.Join(store.Root(), "conversations", key+".json")
			saved := path + ".saved"
			swap := func() {
				require.NoError(t, os.Rename(path, saved))
				require.NoError(t, os.WriteFile(path, unexpected, 0o600))
			}
			var outcome CommitOutcome
			if operation == "update" {
				realPublish := store.conversations.publish
				store.ops.publish = func(oldName, newName string, original *fileIdentity, replacement fileIdentity) error {
					swap()
					return realPublish(oldName, newName, original, replacement)
				}
				outcome, err = store.Update(testIdentity(), func(mapping *Mapping) error {
					mapping.Current.ModelLabel = "changed"
					return nil
				})
			} else {
				realRemove := store.conversations.removeExpected
				store.ops.removeExpected = func(name string, expected fileIdentity) error {
					swap()
					return realRemove(name, expected)
				}
				outcome, err = store.Remove(testIdentity())
			}
			require.Error(t, err)
			require.Equal(t, CommitNotApplied, outcome)
			actual, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, unexpected, actual)
			_, statErr := os.Stat(saved)
			require.NoError(t, statErr, "the original target also remains preserved")
		})
	}
}

func TestReturnedPathsRejectRootAndChildSwaps(t *testing.T) {
	t.Run("workspace child", func(t *testing.T) {
		store := openTestStore(t)
		workspace := filepath.Join(store.Root(), "workspaces", testID)
		moved := workspace + ".moved"
		store.ops.beforePathReturn = func() {
			require.NoError(t, os.Rename(workspace, moved))
			require.NoError(t, os.Mkdir(workspace, 0o700))
		}
		returned, err := store.EnsureWorkspace(testID)
		require.Error(t, err)
		require.Empty(t, returned)
		_, err = os.Stat(workspace)
		require.NoError(t, err)
	})

	t.Run("provider child", func(t *testing.T) {
		store := openTestStore(t)
		path := filepath.Join(store.Root(), "providers", "pi")
		moved := path + ".moved"
		store.ops.beforePathReturn = func() {
			require.NoError(t, os.Rename(path, moved))
			require.NoError(t, os.Mkdir(path, 0o700))
		}
		returned, err := store.EnsureProviderDirectory("pi")
		require.Error(t, err)
		require.Empty(t, returned)
	})

	t.Run("state root", func(t *testing.T) {
		store := openTestStore(t)
		moved := store.Root() + ".moved"
		store.ops.beforePathReturn = func() { require.NoError(t, os.Rename(store.Root(), moved)) }
		returned, err := store.EnsureWorkspace(testID)
		require.Error(t, err)
		require.Empty(t, returned)
	})
}

func TestWorkspaceTombstonePreservesSwappedTarget(t *testing.T) {
	store := openTestStore(t)
	workspace, err := store.EnsureWorkspace(testID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "original"), []byte("safe"), 0o600))
	moved := workspace + ".moved"
	store.ops.beforeWorkspaceTombstone = func() {
		require.NoError(t, os.Rename(workspace, moved))
		require.NoError(t, os.Mkdir(workspace, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "unexpected"), []byte("keep"), 0o600))
	}
	require.Error(t, store.RemoveWorkspace(testID))
	actual, err := os.ReadFile(filepath.Join(workspace, "unexpected"))
	require.NoError(t, err)
	require.Equal(t, []byte("keep"), actual)
	actual, err = os.ReadFile(filepath.Join(moved, "original"))
	require.NoError(t, err)
	require.Equal(t, []byte("safe"), actual)
}

func TestRootSwapHooksCannotMutateDetachedState(t *testing.T) {
	t.Run("mapping publish", func(t *testing.T) {
		store := openTestStore(t)
		root, moved := store.Root(), store.Root()+".moved"
		realPublish := store.conversations.publish
		store.ops.publish = func(oldName, newName string, original *fileIdentity, replacement fileIdentity) error {
			require.NoError(t, os.Rename(root, moved))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "conversations"), 0o700))
			return realPublish(oldName, newName, original, replacement)
		}
		now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
		outcome, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
		require.Error(t, err)
		require.Equal(t, CommitNotApplied, outcome)
		key, keyErr := ConversationKey(testIdentity())
		require.NoError(t, keyErr)
		_, err = os.Stat(filepath.Join(moved, "conversations", mappingName(key)))
		require.ErrorIs(t, err, os.ErrNotExist, "the detached conversation directory must not be published through")
	})

	t.Run("workspace tombstone", func(t *testing.T) {
		store := openTestStore(t)
		workspace, err := store.EnsureWorkspace(testID)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "original"), []byte("safe"), 0o600))
		root, moved := store.Root(), store.Root()+".moved"
		store.ops.beforeWorkspaceTombstone = func() {
			require.NoError(t, os.Rename(root, moved))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "workspaces"), 0o700))
		}
		require.Error(t, store.RemoveWorkspace(testID))
		actual, readErr := os.ReadFile(filepath.Join(moved, "workspaces", testID, "original"))
		require.NoError(t, readErr)
		require.Equal(t, []byte("safe"), actual)
		_, err = os.Stat(filepath.Join(moved, "workspaces", workspaceTombstoneName(testID)))
		require.ErrorIs(t, err, os.ErrNotExist, "the detached workspace must not be tombstoned")
	})
}

func TestWorkspaceRemovalFailuresRemainRetryable(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*Store)
	}{
		{name: "close", inject: func(store *Store) {
			realClose := store.ops.closeWorkspace
			store.ops.closeWorkspace = func(directory *secureDirectory) error {
				require.NoError(t, realClose(directory))
				return errors.New("injected close failure")
			}
		}},
		{name: "final unlink", inject: func(store *Store) {
			store.ops.unlinkWorkspace = func(*secureDirectory, string) error { return errors.New("injected unlink failure") }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			workspace, err := store.EnsureWorkspace(testID)
			require.NoError(t, err)
			test.inject(store)
			require.Error(t, store.RemoveWorkspace(testID))
			info, statErr := os.Stat(workspace)
			require.NoError(t, statErr, "failed removal must restore the canonical workspace")
			require.True(t, info.IsDir())
			store.ops = defaultFileOps(store.conversations, store.workspaces)
			require.NoError(t, store.RemoveWorkspace(testID))
			_, err = os.Stat(workspace)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestWorkspacePostUnlinkErrorIsNeverSuppressed(t *testing.T) {
	store := openTestStore(t)
	workspace, err := store.EnsureWorkspace(testID)
	require.NoError(t, err)
	realUnlink := store.ops.unlinkWorkspace
	postUnlinkErr := errors.New("injected post-unlink failure")
	store.ops.unlinkWorkspace = func(parent *secureDirectory, name string) error {
		require.NoError(t, realUnlink(parent, name))
		return postUnlinkErr
	}

	err = store.RemoveWorkspace(testID)
	require.ErrorIs(t, err, postUnlinkErr)
	_, statErr := os.Stat(workspace)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestWorkspaceAbsentRetryFsyncsParentBeforeSuccess(t *testing.T) {
	store := openTestStore(t)
	workspace, err := store.EnsureWorkspace(testID)
	require.NoError(t, err)
	realSync := store.ops.syncWorkspaces
	syncErr := errors.New("injected workspace sync failure")
	syncs := 0
	store.ops.syncWorkspaces = func() error {
		syncs++
		if syncs == 1 {
			return syncErr
		}
		return realSync()
	}

	require.ErrorIs(t, store.RemoveWorkspace(testID), syncErr)
	_, statErr := os.Stat(workspace)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	require.NoError(t, store.RemoveWorkspace(testID))
	require.Equal(t, 2, syncs, "retry must fsync the workspaces parent before accepting absence")
}

func TestWorkspaceUnlinkRestoreConflictRetainsDeterministicTombstone(t *testing.T) {
	store := openTestStore(t)
	workspace, err := store.EnsureWorkspace(testID)
	require.NoError(t, err)
	store.ops.unlinkWorkspace = func(_ *secureDirectory, _ string) error {
		require.NoError(t, os.Mkdir(workspace, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "substitution"), []byte("keep"), 0o600))
		return errors.New("injected unlink failure")
	}
	require.Error(t, store.RemoveWorkspace(testID))
	tombstone := filepath.Join(store.Root(), "workspaces", workspaceTombstoneName(testID))
	info, err := os.Stat(tombstone)
	require.NoError(t, err, "the failed workspace must retain a name derived from its conversation ID")
	require.True(t, info.IsDir())
	actual, err := os.ReadFile(filepath.Join(workspace, "substitution"))
	require.NoError(t, err)
	require.Equal(t, []byte("keep"), actual)

	store.ops = defaultFileOps(store.conversations, store.workspaces)
	require.Error(t, store.RemoveWorkspace(testID), "a canonical substitution must block tombstone cleanup")
	require.NoError(t, os.RemoveAll(workspace))
	require.NoError(t, store.RemoveWorkspace(testID), "retry must discover and remove the deterministic tombstone")
	_, err = os.Stat(tombstone)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMappingTombstoneCleanupPreservesCanonicalSubstitution(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	key, err := ConversationKey(testIdentity())
	require.NoError(t, err)
	name := mappingName(key)
	expected, err := store.conversations.targetIdentity(name)
	require.NoError(t, err)
	canonical := filepath.Join(store.Root(), "conversations", name)
	replacement := []byte(`{"substitution":true}`)
	err = store.conversations.removeExpectedWithUnlink(name, expected, func(_ *secureDirectory, _ string) error {
		require.NoError(t, os.WriteFile(canonical, replacement, 0o600))
		return errors.New("injected unlink failure")
	})
	require.Error(t, err)
	tombstone := filepath.Join(store.Root(), "conversations", mappingTombstoneName(name))
	_, err = os.Stat(tombstone)
	require.NoError(t, err)

	require.NoError(t, cleanTemporaryMappings(store.conversations))
	actual, err := os.ReadFile(canonical)
	require.NoError(t, err)
	require.Equal(t, replacement, actual, "cleanup must not delete the canonical substitution")
	_, err = os.Stat(tombstone)
	require.ErrorIs(t, err, os.ErrNotExist)
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

func TestCanonicalConversationDirectoryReplacementFailsClosed(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	path := filepath.Join(store.Root(), "conversations")
	moved := path + ".moved"
	require.NoError(t, os.Rename(path, moved))
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err = store.Load(testIdentity())
	require.Error(t, err)
	key, keyErr := ConversationKey(testIdentity())
	require.NoError(t, keyErr)
	_, err = os.Stat(filepath.Join(path, key+".json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAppliedAfterRemoveErrorSyncsDirectory(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	realRemove := store.conversations.removeExpected
	store.ops.removeExpected = func(name string, expected fileIdentity) error {
		require.NoError(t, realRemove(name, expected))
		return errors.New("post-remove failure")
	}
	realSync := store.conversations.sync
	syncs := 0
	store.ops.syncDir = func() error {
		syncs++
		return realSync()
	}
	outcome, err := store.Remove(testIdentity())
	require.Error(t, err)
	require.Equal(t, CommitApplied, outcome)
	require.Equal(t, 1, syncs)
}

func TestAppliedAfterErrorWithFailedSyncIsUncertain(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	realPublish := store.conversations.publish
	store.ops.publish = func(oldName, newName string, original *fileIdentity, replacement fileIdentity) error {
		require.NoError(t, realPublish(oldName, newName, original, replacement))
		return errors.New("post-publish failure")
	}
	store.ops.syncDir = func() error { return errors.New("sync failure") }
	outcome, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.Error(t, err)
	require.Equal(t, CommitUncertain, outcome)
}

func TestRootReplacementMakesExistingStoreFailClosed(t *testing.T) {
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
	require.Error(t, err)
	key, keyErr := ConversationKey(testIdentity())
	require.NoError(t, keyErr)
	_, err = os.Stat(filepath.Join(moved, "conversations", key+".json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(original, "conversations", key+".json"))
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = Open(home)
	require.ErrorIs(t, err, ErrLocked, "the process-local canonical-home guard prevents a split singleton")

	require.NoError(t, os.RemoveAll(original))
	require.NoError(t, os.Rename(moved, original))
	_, err = store.Load(testIdentity())
	require.Error(t, err, "a store remains poisoned after detecting canonical replacement")
}
