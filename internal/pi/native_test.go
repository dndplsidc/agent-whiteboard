//go:build unix

package pi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

type fixedIDs struct {
	value string
	err   error
}

func (ids fixedIDs) NewID() (string, error) { return ids.value, ids.err }

type fixedClock struct{ value time.Time }

func (clock fixedClock) Now() time.Time { return clock.value }

const nativeTestRef = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"

func newTestNativeManager(t *testing.T) (*nativeManager, string) {
	t.Helper()
	temporaryRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(temporaryRoot, "pi")
	require.NoError(t, os.Mkdir(root, 0o700))
	temporaryWorkspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workspace := filepath.Join(temporaryWorkspace, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	manager, err := newNativeManager(root, fixedIDs{value: nativeTestRef}, fixedClock{value: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	require.NoError(t, err)
	return manager, workspace
}

func writeNativeHeader(t *testing.T, allocation nativeAllocation, sessionID string) {
	t.Helper()
	header := map[string]any{"type": "session", "version": 3, "id": sessionID, "timestamp": "2026-01-02T03:04:05Z", "cwd": allocation.Workspace}
	encoded, err := json.Marshal(header)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(allocation.path, append(encoded, '\n'), 0o600))
}

func TestNativeAllocationFinalizeInspectAndDelete(t *testing.T) {
	manager, workspace := newTestNativeManager(t)
	allocation, err := manager.allocate(workspace)
	require.NoError(t, err)
	info, err := os.Lstat(allocation.path)
	require.NoError(t, err)
	markerInfo, err := os.Lstat(allocation.markerPath)
	require.NoError(t, err)
	require.Zero(t, info.Size())
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Equal(t, identityOf(info), identityOf(markerInfo))

	writeNativeHeader(t, allocation, "pi-session")
	state := startupState{SessionID: "pi-session", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "p", ModelID: "m", Model: "p/m", ContextWindow: 10, MaxTokens: 5}
	metadata, err := manager.finalizeAllocation(allocation, state)
	require.NoError(t, err)
	require.NoError(t, metadata.Validate())
	metadataInfo, err := os.Lstat(manager.metadataPath(allocation.Ref))
	require.NoError(t, err)
	require.True(t, metadataInfo.Mode().IsRegular())
	require.Equal(t, os.FileMode(0o600), metadataInfo.Mode().Perm())
	temporaryMetadata, err := filepath.Glob(filepath.Join(manager.sessions, ".metadata-*"))
	require.NoError(t, err)
	require.Empty(t, temporaryMetadata)
	inspected, err := manager.inspect(allocation.Ref)
	require.NoError(t, err)
	require.Equal(t, metadata, inspected)
	require.NoError(t, manager.delete(allocation.Ref))
	require.NoError(t, manager.delete(allocation.Ref))
	require.NoFileExists(t, allocation.path)
	require.NoFileExists(t, allocation.markerPath)
}

func TestNativeAllocationRejectsZeroClockBeforeCreatingFile(t *testing.T) {
	temporaryRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(temporaryRoot, "pi")
	require.NoError(t, os.Mkdir(root, 0o700))
	workspace := filepath.Join(temporaryRoot, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	manager, err := newNativeManager(root, fixedIDs{value: nativeTestRef}, fixedClock{})
	require.NoError(t, err)
	_, err = manager.allocate(workspace)
	require.Error(t, err)
	require.NoFileExists(t, manager.sessionPath(mustNativeRef(t, nativeTestRef)))
}

func TestNativeFinalizationIsIdempotentAndNeverReplacesMetadata(t *testing.T) {
	manager, workspace := newTestNativeManager(t)
	allocation, err := manager.allocate(workspace)
	require.NoError(t, err)
	writeNativeHeader(t, allocation, "pi-session")
	state := startupState{SessionID: "pi-session", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "p", ModelID: "m", Model: "p/m", ContextWindow: 10, MaxTokens: 5}
	first, err := manager.finalizeAllocation(allocation, state)
	require.NoError(t, err)
	second, err := manager.finalizeAllocation(allocation, state)
	require.NoError(t, err)
	require.Equal(t, first, second)
	contents, err := os.ReadFile(manager.metadataPath(allocation.Ref))
	require.NoError(t, err)
	state.Model, state.ModelProvider = "foreign/m", "foreign"
	_, err = manager.finalizeAllocation(allocation, state)
	require.Error(t, err)
	after, err := os.ReadFile(manager.metadataPath(allocation.Ref))
	require.NoError(t, err)
	require.Equal(t, contents, after)
}

func TestNativeConcurrentExactFinalizationAndAmbiguousSyncAreRecovered(t *testing.T) {
	t.Run("concurrent", func(t *testing.T) {
		for range 20 {
			manager, workspace := newTestNativeManager(t)
			allocation, err := manager.allocate(workspace)
			require.NoError(t, err)
			writeNativeHeader(t, allocation, "pi-session")
			state := startupState{SessionID: "pi-session", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "p", ModelID: "m", Model: "p/m", ContextWindow: 10, MaxTokens: 5}
			results := make(chan error, 2)
			var start sync.WaitGroup
			start.Add(1)
			for range 2 {
				go func() {
					start.Wait()
					_, finalizeErr := manager.finalizeAllocation(allocation, state)
					results <- finalizeErr
				}()
			}
			start.Done()
			require.NoError(t, <-results)
			require.NoError(t, <-results)
		}
	})

	t.Run("post-link sync", func(t *testing.T) {
		manager, workspace := newTestNativeManager(t)
		allocation, err := manager.allocate(workspace)
		require.NoError(t, err)
		writeNativeHeader(t, allocation, "pi-session")
		calls := 0
		manager.syncDir = func(path string) error {
			calls++
			if calls == 1 {
				return errors.New("injected sync failure")
			}
			return syncDirectory(path)
		}
		state := startupState{SessionID: "pi-session", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "p", ModelID: "m", Model: "p/m", ContextWindow: 10, MaxTokens: 5}
		_, err = manager.finalizeAllocation(allocation, state)
		require.NoError(t, err)
		require.GreaterOrEqual(t, calls, 2)
	})
}

func TestNativeAllocationCollisionRollbackAndReplacementFailClosed(t *testing.T) {
	manager, workspace := newTestNativeManager(t)
	allocation, err := manager.allocate(workspace)
	require.NoError(t, err)
	_, err = manager.allocate(workspace)
	require.Error(t, err)

	require.NoError(t, os.Remove(allocation.path))
	require.NoError(t, os.WriteFile(allocation.path, nil, 0o600))
	require.Error(t, manager.rollbackAllocation(allocation))

	require.NoError(t, os.Remove(allocation.path))
	allocation, err = manager.allocate(workspace)
	require.NoError(t, err)
	require.NoError(t, manager.rollbackAllocation(allocation))
	require.NoFileExists(t, allocation.path)
	require.NoFileExists(t, allocation.markerPath)
}

func TestNativeSessionHeaderRejectsDuplicateIdentityAndWrongVersion(t *testing.T) {
	manager, workspace := newTestNativeManager(t)
	allocation, err := manager.allocate(workspace)
	require.NoError(t, err)
	for name, line := range map[string]string{
		"duplicate id":  `{"type":"session","version":3,"id":"pi-session","id":"foreign","timestamp":"2026-01-02T03:04:05Z","cwd":"` + workspace + `"}` + "\n",
		"wrong version": `{"type":"session","version":4,"id":"pi-session","timestamp":"2026-01-02T03:04:05Z","cwd":"` + workspace + `"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(allocation.path, []byte(line), 0o600))
			require.Error(t, validateSessionFile(allocation.path, "pi-session", workspace))
		})
	}
}

func TestNativeRejectsInsecurePathsHeadersAndMetadata(t *testing.T) {
	manager, workspace := newTestNativeManager(t)
	allocation, err := manager.allocate(workspace)
	require.NoError(t, err)
	writeNativeHeader(t, allocation, "wrong")
	state := startupState{SessionID: "expected", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "p", ModelID: "m", Model: "p/m", ContextWindow: 10, MaxTokens: 5}
	_, err = manager.finalizeAllocation(allocation, state)
	require.Error(t, err)

	require.NoError(t, os.Remove(allocation.path))
	require.NoError(t, os.Symlink(workspace, allocation.path))
	_, err = manager.inspect(allocation.Ref)
	assertProviderCode(t, err, provider.ErrorNativeSessionMissing)

	root := filepath.Join(t.TempDir(), "bad")
	require.NoError(t, os.Mkdir(root, 0o755))
	_, err = newNativeManager(root, fixedIDs{value: nativeTestRef}, fixedClock{value: time.Now().UTC()})
	require.Error(t, err)
}

func TestNativeDeleteRetriesMetadataOnlyRemainder(t *testing.T) {
	manager, workspace := newTestNativeManager(t)
	allocation, err := manager.allocate(workspace)
	require.NoError(t, err)
	writeNativeHeader(t, allocation, "pi-session")
	state := startupState{SessionID: "pi-session", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "p", ModelID: "m", Model: "p/m", ContextWindow: 10, MaxTokens: 5}
	_, err = manager.finalizeAllocation(allocation, state)
	require.NoError(t, err)
	require.NoError(t, os.Remove(allocation.path))
	require.NoError(t, manager.delete(allocation.Ref))
}

func mustNativeRef(t *testing.T, value string) provider.NativeSessionRef {
	t.Helper()
	ref, err := provider.NewNativeSessionRef(value)
	require.NoError(t, err)
	return ref
}

func TestNativeInspectFailsClosedOnSessionAndMetadataSubstitution(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *nativeManager, nativeAllocation)
	}{
		{name: "session mode", mutate: func(t *testing.T, _ *nativeManager, allocation nativeAllocation) {
			require.NoError(t, os.Chmod(allocation.path, 0o644))
		}},
		{name: "session header", mutate: func(t *testing.T, _ *nativeManager, allocation nativeAllocation) {
			writeNativeHeader(t, allocation, "substituted")
		}},
		{name: "session nonregular", mutate: func(t *testing.T, _ *nativeManager, allocation nativeAllocation) {
			require.NoError(t, os.Remove(allocation.path))
			require.NoError(t, os.Mkdir(allocation.path, 0o700))
		}},
		{name: "session symlink", mutate: func(t *testing.T, _ *nativeManager, allocation nativeAllocation) {
			replacement := allocation.path + ".replacement"
			require.NoError(t, os.Rename(allocation.path, replacement))
			require.NoError(t, os.Symlink(replacement, allocation.path))
		}},
		{name: "metadata mode", mutate: func(t *testing.T, manager *nativeManager, allocation nativeAllocation) {
			require.NoError(t, os.Chmod(manager.metadataPath(allocation.Ref), 0o644))
		}},
		{name: "metadata symlink", mutate: func(t *testing.T, manager *nativeManager, allocation nativeAllocation) {
			path := manager.metadataPath(allocation.Ref)
			replacement := path + ".replacement"
			require.NoError(t, os.Rename(path, replacement))
			require.NoError(t, os.Symlink(replacement, path))
		}},
		{name: "metadata ref", mutate: func(t *testing.T, manager *nativeManager, allocation nativeAllocation) {
			path := manager.metadataPath(allocation.Ref)
			contents, err := os.ReadFile(path)
			require.NoError(t, err)
			var metadata map[string]any
			require.NoError(t, json.Unmarshal(contents, &metadata))
			metadata["ref"] = "eHl6eHl6eHl6eHl6eHl6eHl6eHl6eHl6"
			contents, err = json.Marshal(metadata)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, contents, 0o600))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace := newTestNativeManager(t)
			allocation, err := manager.allocate(workspace)
			require.NoError(t, err)
			writeNativeHeader(t, allocation, "pi-session")
			state := startupState{SessionID: "pi-session", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "p", ModelID: "m", Model: "p/m", ContextWindow: 10, MaxTokens: 5}
			_, err = manager.finalizeAllocation(allocation, state)
			require.NoError(t, err)
			test.mutate(t, manager, allocation)
			_, err = manager.inspect(allocation.Ref)
			assertProviderCode(t, err, provider.ErrorNativeSessionMissing)
		})
	}
}
