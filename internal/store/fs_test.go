package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	imageDomain "github.com/dndplsidc/agent-whiteboard/internal/image"
	whiteboardDomain "github.com/dndplsidc/agent-whiteboard/internal/whiteboard"
	"github.com/stretchr/testify/require"
)

const (
	testID  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testID2 = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

func TestFSCreateHonorsCanceledLifecycleBeforeInitialization(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fs, err := NewFS(Config{Root: root, CleanupInterval: time.Hour, Clock: common.SystemClock{}, Context: ctx})
	require.Nil(t, fs)
	require.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(filepath.Join(root, "whiteboards"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestFSCreateRejectsConfiguredRootSymlink(t *testing.T) {
	target := t.TempDir()
	configuredRoot := filepath.Join(t.TempDir(), "storage-link")
	require.NoError(t, os.Symlink(target, configuredRoot))

	fs, err := NewFS(Config{Root: configuredRoot, CleanupInterval: time.Hour, Clock: common.SystemClock{}, Context: context.Background()})
	require.Nil(t, fs)
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, target)
	_, statErr := os.Stat(filepath.Join(target, "whiteboards"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestFSCreateRejectsPreexistingCategorySymlink(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "real-whiteboards"), 0o700))
	require.NoError(t, os.Symlink("real-whiteboards", filepath.Join(root, "whiteboards")))

	fs, err := NewFS(Config{Root: root, CleanupInterval: time.Hour, Clock: common.SystemClock{}, Context: context.Background()})
	require.Nil(t, fs)
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
}

func TestPublishGenerationDoesNotClobberAndCanRetry(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	require.NoError(t, root.WriteFile("temp", []byte("new"), 0o600))
	require.NoError(t, root.WriteFile("content-00000000000000000000000000000000", []byte("committed"), 0o600))

	err = publishGeneration(root, "temp", "content-00000000000000000000000000000000")
	require.ErrorIs(t, err, os.ErrExist)
	committed, readErr := root.ReadFile("content-00000000000000000000000000000000")
	require.NoError(t, readErr)
	require.Equal(t, []byte("committed"), committed)
	require.Equal(t, []byte("new"), mustReadRootFile(t, root, "temp"))

	require.NoError(t, publishGeneration(root, "temp", "content-11111111111111111111111111111111"))
	require.Equal(t, []byte("new"), mustReadRootFile(t, root, "content-11111111111111111111111111111111"))
	_, err = root.Lstat("temp")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestFSCreateInitializesLayoutAndRoundTripsWhiteboard(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	created := time.Unix(1_700_000_000, 123_456_789).UTC()
	updated := time.Unix(1_700_000_001, 987_654_321).UTC()
	expires := time.Unix(1_800_000_000, 222_333_444).UTC()
	record := whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("# exact\x00bytes\n"), Context: []byte("exact context\x00bytes\n"),
		CreatedAt: created, UpdatedAt: updated, ExpiresAt: &expires,
	}

	require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
	got, err := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, record, got)

	for _, dir := range []string{root, filepath.Join(root, "whiteboards"), filepath.Join(root, "images"), filepath.Join(root, ".readiness"), filepath.Join(root, "whiteboards", testID)} {
		assertPermissions(t, dir, 0o700)
	}
	resourceDir := filepath.Join(root, "whiteboards", testID)
	entries, err := os.ReadDir(resourceDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	for _, entry := range entries {
		assertPermissions(t, filepath.Join(resourceDir, entry.Name()), 0o600)
	}

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(readFile(t, filepath.Join(resourceDir, "metadata.json")), &metadata))
	require.Equal(t, float64(2), metadata["schema_version"])
	require.Equal(t, "markdown", metadata["kind"])
	sourceGeneration := metadata["content_filename"].(string)
	contextGeneration := metadata["context_filename"].(string)
	require.Regexp(t, regexp.MustCompile(`^source-[a-f0-9]{32}\.md$`), sourceGeneration)
	require.Regexp(t, regexp.MustCompile(`^context-[a-f0-9]{32}\.md$`), contextGeneration)
	require.Equal(t, generationToken(t, sourceGeneration, "source-"), generationToken(t, contextGeneration, "context-"))
	require.Equal(t, record.Source, readFile(t, filepath.Join(resourceDir, sourceGeneration)))
	require.Equal(t, record.Context, readFile(t, filepath.Join(resourceDir, contextGeneration)))
	assertJSONTime(t, metadata["created_at"], created)
	assertJSONTime(t, metadata["updated_at"], updated)
	assertJSONTime(t, metadata["expires_at"], expires)
}

func TestFSCreateImageUsesExtensionlessInternalGeneration(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := imageDomain.Image{
		ID: testID, Extension: ".png", MediaType: "image/png", Content: []byte("not interpreted by storage"),
		CreatedAt: time.Unix(10, 11).UTC(), UpdatedAt: time.Unix(12, 13).UTC(),
	}

	require.NoError(t, fs.Images().Create(context.Background(), record))
	got, err := fs.Images().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, record, got)

	resourceDir := filepath.Join(root, "images", testID)
	entries, err := os.ReadDir(resourceDir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, entry := range entries {
		if entry.Name() != "metadata.json" {
			require.Regexp(t, regexp.MustCompile(`^content-[a-f0-9]{32}$`), entry.Name())
			require.NotContains(t, entry.Name(), record.Extension)
		}
	}
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(readFile(t, filepath.Join(resourceDir, "metadata.json")), &metadata))
	require.Equal(t, "image", metadata["kind"])
	require.Equal(t, ".png", metadata["extension"])
	require.Equal(t, "image/png", metadata["media_type"])
	require.Nil(t, metadata["expires_at"])
}

func TestFSCreateCollisionClassification(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := whiteboardDomain.Whiteboard{ID: testID, Kind: whiteboardDomain.KindHTML, Source: []byte("<!doctype html>"), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), record))

	err := fs.Whiteboards().Create(context.Background(), record)
	require.ErrorIs(t, err, common.ErrIDCollision)
	require.False(t, common.HasCode(err, common.CodeStorageUnavailable))

	linkIDDir := filepath.Join(root, "whiteboards", testID2)
	require.NoError(t, os.Symlink(filepath.Join(root, "images"), linkIDDir))
	err = fs.Whiteboards().Create(context.Background(), whiteboardDomain.Whiteboard{ID: testID2, Kind: whiteboardDomain.KindHTML})
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	require.NotErrorIs(t, err, common.ErrIDCollision)
}

func TestFSGetMissingAndInvalidIDs(t *testing.T) {
	fs := newTestFS(t, t.TempDir())
	_, err := fs.Whiteboards().Get(context.Background(), testID)
	assertCodeWithoutRoot(t, err, common.CodeNotFound, "")
	_, err = fs.Images().Get(context.Background(), "../outside")
	require.True(t, common.HasCode(err, common.CodeInvalidRequest))
	require.Equal(t, "invalid resource id", err.Error())
}

func TestFSPathSafetyRejectsManagedSymlinks(t *testing.T) {
	t.Run("resource directory", func(t *testing.T) {
		root := t.TempDir()
		fs := newTestFS(t, root)
		outside := t.TempDir()
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "images", testID)))
		_, err := fs.Images().Get(context.Background(), testID)
		assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	})

	t.Run("metadata", func(t *testing.T) {
		root := t.TempDir()
		fs := newTestFS(t, root)
		dir := filepath.Join(root, "whiteboards", testID)
		require.NoError(t, os.Mkdir(dir, 0o700))
		outside := filepath.Join(t.TempDir(), "metadata.json")
		require.NoError(t, os.WriteFile(outside, []byte(`{}`), 0o600))
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, "metadata.json")))
		_, err := fs.Whiteboards().Get(context.Background(), testID)
		assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	})

	t.Run("generation", func(t *testing.T) {
		root := t.TempDir()
		fs := newTestFS(t, root)
		record := whiteboardDomain.Whiteboard{ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("safe"), CreatedAt: time.Now(), UpdatedAt: time.Now()}
		require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
		dir := filepath.Join(root, "whiteboards", testID)
		metadata := decodeMetadata(t, filepath.Join(dir, "metadata.json"))
		generation := metadata["content_filename"].(string)
		require.NoError(t, os.Remove(filepath.Join(dir, generation)))
		outside := filepath.Join(t.TempDir(), "outside")
		require.NoError(t, os.WriteFile(outside, []byte("escaped"), 0o600))
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, generation)))
		_, err := fs.Whiteboards().Get(context.Background(), testID)
		assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
		linkInfo, statErr := os.Lstat(filepath.Join(dir, generation))
		require.NoError(t, statErr)
		require.NotZero(t, linkInfo.Mode()&os.ModeSymlink)
		require.Equal(t, []byte("escaped"), readFile(t, outside))
	})
}

func TestFSGetValidatesMetadataAndCommittedFilename(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "schema", mutate: func(m map[string]any) { m["schema_version"] = 99 }},
		{name: "kind", mutate: func(m map[string]any) { m["kind"] = "image" }},
		{name: "timestamp nanoseconds", mutate: func(m map[string]any) { m["created_at"] = map[string]any{"seconds": 1, "nanoseconds": 1_000_000_000} }},
		{name: "filename traversal", mutate: func(m map[string]any) { m["content_filename"] = "../../outside.md" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			record := whiteboardDomain.Whiteboard{ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("safe"), CreatedAt: time.Now(), UpdatedAt: time.Now()}
			require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
			path := filepath.Join(root, "whiteboards", testID, "metadata.json")
			metadata := decodeMetadata(t, path)
			tt.mutate(metadata)
			encoded, err := json.Marshal(metadata)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, encoded, 0o600))
			_, err = fs.Whiteboards().Get(context.Background(), testID)
			assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
		})
	}
}

func TestFSReplaceCommitsNewGenerationAndDeleteRemovesResource(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	old := imageDomain.Image{ID: testID, Extension: ".png", MediaType: "image/png", Content: []byte("old"), CreatedAt: time.Unix(1, 2), UpdatedAt: time.Unix(3, 4)}
	require.NoError(t, fs.Images().Create(context.Background(), old))
	dir := filepath.Join(root, "images", testID)
	oldGeneration := decodeMetadata(t, filepath.Join(dir, "metadata.json"))["content_filename"].(string)
	replacement := imageDomain.Image{ID: testID, Extension: ".jpg", MediaType: "image/jpeg", Content: []byte("new"), CreatedAt: time.Unix(99, 100), UpdatedAt: time.Unix(5, 6)}

	require.NoError(t, fs.Images().Replace(context.Background(), replacement))
	got, err := fs.Images().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, replacement.ID, got.ID)
	require.Equal(t, replacement.Extension, got.Extension)
	require.Equal(t, replacement.MediaType, got.MediaType)
	require.Equal(t, replacement.Content, got.Content)
	require.True(t, old.CreatedAt.Equal(got.CreatedAt))
	require.True(t, replacement.UpdatedAt.Equal(got.UpdatedAt))
	require.Nil(t, got.ExpiresAt)
	newGeneration := decodeMetadata(t, filepath.Join(dir, "metadata.json"))["content_filename"].(string)
	require.NotEqual(t, oldGeneration, newGeneration)
	_, err = os.Stat(filepath.Join(dir, oldGeneration))
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, fs.Images().Delete(context.Background(), testID))
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
	err = fs.Images().Delete(context.Background(), testID)
	assertCodeWithoutRoot(t, err, common.CodeNotFound, root)
}

func TestFSDirectoryEntriesAreSyncedInCrashConsistentOrder(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	original := imageDomain.Image{
		ID: testID, Extension: ".png", MediaType: "image/png", Content: []byte("old"),
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	replacement := imageDomain.Image{
		ID: testID, Extension: ".jpg", MediaType: "image/jpeg", Content: []byte("new"),
		CreatedAt: time.Unix(2, 0), UpdatedAt: time.Unix(2, 0),
	}

	var events []string
	oldGeneration := ""
	fs.directorySync = func(directory *os.Root) error {
		category := fs.categories["images"]
		if directory == category {
			if _, err := category.Lstat(testID); err == nil {
				events = append(events, "resource-directory-created")
			} else if errors.Is(err, os.ErrNotExist) {
				events = append(events, "resource-directory-removed")
			} else {
				return err
			}
			return nil
		}

		stored, err := readMetadataForTest(directory)
		if errors.Is(err, os.ErrNotExist) {
			events = append(events, "generation-published")
			return nil
		}
		if err != nil {
			return err
		}
		if oldGeneration == "" {
			events = append(events, "metadata-published")
		} else if stored.ContentFilename == oldGeneration {
			events = append(events, "generation-published")
		} else if _, err := directory.Lstat(oldGeneration); errors.Is(err, os.ErrNotExist) {
			events = append(events, "old-generation-removed")
		} else if err == nil {
			events = append(events, "metadata-published")
		} else {
			return err
		}
		return nil
	}

	require.NoError(t, fs.Images().Create(context.Background(), original))
	require.Equal(t, []string{
		"resource-directory-created",
		"generation-published",
		"metadata-published",
	}, events)
	oldGeneration = decodeMetadata(t, filepath.Join(root, "images", testID, metadataFilename))["content_filename"].(string)

	events = nil
	require.NoError(t, fs.Images().Replace(context.Background(), replacement))
	require.Equal(t, []string{
		"generation-published",
		"metadata-published",
		"old-generation-removed",
	}, events)

	events = nil
	require.NoError(t, fs.Images().Delete(context.Background(), testID))
	require.Equal(t, []string{"resource-directory-removed"}, events)
}

func TestFSReplaceMetadataDirectorySyncFailurePreservesPublishedRecord(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	require.NoError(t, fs.Whiteboards().Create(context.Background(), whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("old"),
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}))
	syncFailure := errors.New("injected metadata directory sync failure")
	resourceSyncs := 0
	fs.directorySync = func(directory *os.Root) error {
		if directory == fs.categories["whiteboards"] {
			return nil
		}
		resourceSyncs++
		if resourceSyncs == 2 {
			return syncFailure
		}
		return nil
	}

	err := fs.Whiteboards().Replace(context.Background(), whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindHTML, Source: []byte("replaced"),
		CreatedAt: time.Unix(2, 0), UpdatedAt: time.Unix(2, 0),
	})
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	require.ErrorIs(t, err, syncFailure)
	fs.directorySync = func(*os.Root) error { return nil }

	got, err := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, []byte("replaced"), got.Source)
	stored := decodeMetadata(t, filepath.Join(root, "whiteboards", testID, metadataFilename))
	require.FileExists(t, filepath.Join(root, "whiteboards", testID, stored["content_filename"].(string)))
}

func TestFSSingleArtifactAppliedMetadataRenameErrorKeepsReplacementReadable(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	original := imageDomain.Image{
		ID: testID, Extension: ".png", MediaType: "image/png", Content: []byte("old"),
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	require.NoError(t, fs.Images().Create(context.Background(), original))
	injected := errors.New("injected applied metadata rename failure")
	fs.renameArtifact = func(root *os.Root, oldName, newName string) error {
		if err := renameArtifact(root, oldName, newName); err != nil {
			return err
		}
		return injected
	}
	replacement := imageDomain.Image{
		ID: testID, Extension: ".jpg", MediaType: "image/jpeg", Content: []byte("new"),
		CreatedAt: time.Unix(2, 0), UpdatedAt: time.Unix(2, 0),
	}

	err := fs.Images().Replace(context.Background(), replacement)
	require.ErrorIs(t, err, injected)
	fs.renameArtifact = renameArtifact
	got, getErr := fs.Images().Get(context.Background(), testID)
	require.NoError(t, getErr)
	require.Equal(t, replacement.Content, got.Content)
	require.Equal(t, replacement.Extension, got.Extension)
	require.Equal(t, replacement.MediaType, got.MediaType)
}

func TestFSCreatePostCommitErrorRollsBackVerifiedResource(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	injected := errors.New("injected post-commit directory sync failure")
	resourceSyncs := 0
	fs.directorySync = func(directory *os.Root) error {
		if directory != fs.categories["whiteboards"] {
			resourceSyncs++
			if resourceSyncs == 2 {
				return injected
			}
		}
		return syncDirectory(directory)
	}

	err := fs.Whiteboards().Create(context.Background(), markdownRecord([]byte("source"), []byte("context")))
	require.ErrorIs(t, err, injected)
	var uncertain whiteboardDomain.UncertainCreateError
	require.False(t, errors.As(err, &uncertain))
	_, statErr := os.Stat(filepath.Join(root, "whiteboards", testID))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestFSCreateAppliedMetadataRenameErrorRollsBackVerifiedResource(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	injected := errors.New("injected applied metadata rename failure")
	fs.renameArtifact = func(root *os.Root, oldName, newName string) error {
		if err := renameArtifact(root, oldName, newName); err != nil {
			return err
		}
		return injected
	}

	err := fs.Whiteboards().Create(context.Background(), markdownRecord([]byte("source"), []byte("context")))
	require.ErrorIs(t, err, injected)
	var uncertain whiteboardDomain.UncertainCreateError
	require.False(t, errors.As(err, &uncertain))
	_, statErr := os.Stat(filepath.Join(root, "whiteboards", testID))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestFSCreateRollbackFailureReportsPossiblyLiveResource(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	commitFailure := errors.New("injected post-commit directory sync failure")
	rollbackFailure := errors.New("injected rollback failure")
	resourceSyncs := 0
	fs.directorySync = func(directory *os.Root) error {
		if directory != fs.categories["whiteboards"] {
			resourceSyncs++
			if resourceSyncs == 2 {
				return commitFailure
			}
		}
		return syncDirectory(directory)
	}
	fs.removeResource = func(*os.Root, string) error { return rollbackFailure }
	record := markdownRecord([]byte("source"), []byte("context"))

	err := fs.Whiteboards().Create(context.Background(), record)
	require.ErrorIs(t, err, commitFailure)
	require.ErrorIs(t, err, rollbackFailure)
	var uncertain whiteboardDomain.UncertainCreateError
	require.ErrorAs(t, err, &uncertain)
	require.True(t, uncertain.ResourceMayExist())

	fs.directorySync = syncDirectory
	got, getErr := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, getErr)
	require.Equal(t, record, got)
}

func TestImageServiceRollsBackUncertainFSCreate(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	ids := &sequenceIDGenerator{ids: []string{testID, testID2}}
	service, err := imageDomain.NewService(
		fs.Images(),
		common.SystemClock{},
		ids,
		0,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	require.NoError(t, err)

	syncFailure := errors.New("injected uncertain image commit")
	resourceSyncs := 0
	failed := false
	fs.directorySync = func(directory *os.Root) error {
		if directory != fs.categories["images"] {
			resourceSyncs++
			if !failed && resourceSyncs == 4 {
				failed = true
				return syncFailure
			}
		}
		return syncDirectory(directory)
	}

	content := encodedStoreTestPNG(t)
	result, err := service.CreateImages(context.Background(), imageDomain.CreateInput{Images: []imageDomain.Upload{
		{Content: content},
		{Content: content},
	}})
	require.Nil(t, result)
	require.ErrorIs(t, err, syncFailure)
	require.True(t, common.HasCode(err, common.CodeStorageUnavailable))
	require.True(t, failed)
	for _, id := range []string{testID, testID2} {
		_, statErr := os.Stat(filepath.Join(root, "images", id))
		require.ErrorIs(t, statErr, os.ErrNotExist, id)
	}
}

func TestFSConcurrentReadersObserveCompleteReplacementRecords(t *testing.T) {
	fs := newTestFS(t, t.TempDir())
	old := whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("old-markdown"),
		CreatedAt: time.Unix(10, 11).UTC(), UpdatedAt: time.Unix(12, 13).UTC(),
	}
	replacement := whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindHTML, Source: []byte("<p>new-html</p>"),
		CreatedAt: time.Unix(99, 100).UTC(), UpdatedAt: time.Unix(14, 15).UTC(),
	}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), old))
	key := resourceLockKey("whiteboards", testID)
	releaseBarrier, err := fs.locks.lock(context.Background(), key)
	require.NoError(t, err)
	defer func() {
		if releaseBarrier != nil {
			releaseBarrier()
		}
	}()

	const readers = 50
	const writers = 20
	start := make(chan struct{})
	errs := make(chan error, readers+writers)
	var workers sync.WaitGroup
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range writers {
				got, err := fs.Whiteboards().Get(context.Background(), testID)
				if err != nil {
					errs <- err
					return
				}
				oldComplete := got.Kind == old.Kind && string(got.Source) == string(old.Source) && got.UpdatedAt.Equal(old.UpdatedAt)
				newComplete := got.Kind == replacement.Kind && string(got.Source) == string(replacement.Source) && got.UpdatedAt.Equal(replacement.UpdatedAt)
				if (!oldComplete && !newComplete) || !got.CreatedAt.Equal(old.CreatedAt) {
					errs <- fmt.Errorf("observed incomplete record: %#v", got)
					return
				}
			}
		}()
	}
	for range writers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if err := fs.Whiteboards().Replace(context.Background(), replacement); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	waitFor(t, 2*time.Second, func() bool { return lockRefs(&fs.locks, key) == readers+writers+1 })
	releaseBarrier()
	releaseBarrier = nil
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, replacement.Kind, got.Kind)
	require.Equal(t, replacement.Source, got.Source)
	require.True(t, old.CreatedAt.Equal(got.CreatedAt))
}

func TestFSUnrelatedResourceLocksDoNotBlockEachOther(t *testing.T) {
	fs := newTestFS(t, t.TempDir())
	first := whiteboardDomain.Whiteboard{ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("first"), CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}
	second := whiteboardDomain.Whiteboard{ID: testID2, Kind: whiteboardDomain.KindMarkdown, Source: []byte("second"), CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), first))
	require.NoError(t, fs.Whiteboards().Create(context.Background(), second))

	release, err := fs.locks.lock(context.Background(), resourceLockKey("whiteboards", testID))
	require.NoError(t, err)
	defer release()
	second.Source = []byte("updated independently")
	second.UpdatedAt = time.Unix(2, 0)
	done := make(chan error, 1)
	go func() { done <- fs.Whiteboards().Replace(context.Background(), second) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("replacement of unrelated ID blocked on held resource lock")
	}
}

func TestLockSetCanceledWaiterReleasesReferenceAfterAcquiring(t *testing.T) {
	locks := lockSet{entries: make(map[string]*lockEntry)}
	releaseHeld, err := locks.lock(context.Background(), "whiteboards/"+testID)
	require.NoError(t, err)
	waitCtx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		release, err := locks.lock(waitCtx, "whiteboards/"+testID)
		if err == nil {
			release()
		}
		waitDone <- err
	}()
	waitFor(t, 2*time.Second, func() bool { return lockRefs(&locks, "whiteboards/"+testID) == 2 })
	cancel()
	select {
	case err := <-waitDone:
		t.Fatalf("standard mutex waiter returned before acquiring the lock: %v", err)
	default:
	}
	releaseHeld()
	select {
	case err := <-waitDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter did not return after acquiring the lock")
	}
	require.Zero(t, lockEntryCount(&locks))
}

func TestLockSetReadAndWriteEntriesAreRemovedAtZeroReferences(t *testing.T) {
	locks := lockSet{entries: make(map[string]*lockEntry)}
	releaseRead, err := locks.rlock(context.Background(), "images/"+testID)
	require.NoError(t, err)
	releaseWrite, err := locks.lock(context.Background(), "whiteboards/"+testID)
	require.NoError(t, err)
	require.Equal(t, 2, lockEntryCount(&locks))
	releaseRead()
	require.Equal(t, 1, lockEntryCount(&locks))
	releaseWrite()
	require.Zero(t, lockEntryCount(&locks))
}

func TestFSLazilyDeletesExpiredResourcesAsNotFound(t *testing.T) {
	tests := []struct {
		name string
		run  func(*FS) error
	}{
		{name: "get", run: func(fs *FS) error {
			_, err := fs.Whiteboards().Get(context.Background(), testID)
			return err
		}},
		{name: "replace", run: func(fs *FS) error {
			return fs.Whiteboards().Replace(context.Background(), whiteboardDomain.Whiteboard{
				ID: testID, Kind: whiteboardDomain.KindHTML, Source: []byte("replacement"),
				CreatedAt: time.Unix(20, 0), UpdatedAt: time.Unix(21, 0),
			})
		}},
		{name: "delete", run: func(fs *FS) error {
			return fs.Whiteboards().Delete(context.Background(), testID)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0).UTC()
			clock := &testClock{now: now}
			root := t.TempDir()
			fs := newTestFSWithClock(t, root, clock, time.Hour)
			record := whiteboardDomain.Whiteboard{
				ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("expired"),
				CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute), ExpiresAt: &now,
			}
			require.NoError(t, fs.Whiteboards().Create(context.Background(), record))

			err := tt.run(fs)
			assertCodeWithoutRoot(t, err, common.CodeNotFound, root)
			_, statErr := os.Stat(filepath.Join(root, "whiteboards", testID))
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestFSExpiredGetObservesFreshReplacementQueuedDuringLockTransition(t *testing.T) {
	expires := time.Unix(1_700_000_000, 0).UTC()
	// Get first observes expiration. A clock correction then lets the already
	// queued writer replace the old record before Get acquires its write lock.
	clock := &transitionClock{
		times:         []time.Time{expires, expires.Add(-time.Second), expires.Add(-time.Second)},
		firstObserved: make(chan struct{}),
		resumeFirst:   make(chan struct{}),
	}
	fs := newTestFSWithClock(t, t.TempDir(), clock, time.Hour)
	original := whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("expired snapshot"),
		CreatedAt: expires.Add(-time.Hour), UpdatedAt: expires.Add(-time.Minute), ExpiresAt: &expires,
	}
	replacementExpires := expires.Add(time.Hour)
	replacement := whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindHTML, Source: []byte("fresh replacement"),
		CreatedAt: expires.Add(-2 * time.Hour), UpdatedAt: expires, ExpiresAt: &replacementExpires,
	}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), original))

	type getResult struct {
		record whiteboardDomain.Whiteboard
		err    error
	}
	getDone := make(chan getResult, 1)
	go func() {
		record, err := fs.Whiteboards().Get(context.Background(), testID)
		getDone <- getResult{record: record, err: err}
	}()
	<-clock.firstObserved

	replaceDone := make(chan error, 1)
	go func() { replaceDone <- fs.Whiteboards().Replace(context.Background(), replacement) }()
	key := resourceLockKey("whiteboards", testID)
	waitFor(t, 2*time.Second, func() bool {
		return lockRefs(&fs.locks, key) == 2 && writeLockPending(&fs.locks, key)
	})
	close(clock.resumeFirst)

	require.NoError(t, <-replaceDone)
	result := <-getDone
	require.NoError(t, result.err)
	require.Equal(t, replacement.Kind, result.record.Kind)
	require.Equal(t, replacement.Source, result.record.Source)
	require.True(t, original.CreatedAt.Equal(result.record.CreatedAt))
	require.True(t, replacement.UpdatedAt.Equal(result.record.UpdatedAt))
	require.NotNil(t, result.record.ExpiresAt)
	require.True(t, replacementExpires.Equal(*result.record.ExpiresAt))
}

func TestFSPeriodicCleanupDeletesExpiredResources(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	clock := &testClock{now: start}
	root := t.TempDir()
	fs := newTestFSWithClock(t, root, clock, 5*time.Millisecond)
	expires := start.Add(time.Second)
	record := imageDomain.Image{
		ID: testID, Extension: ".png", MediaType: "image/png", Content: []byte("expires periodically"),
		CreatedAt: start, UpdatedAt: start, ExpiresAt: &expires,
	}
	require.NoError(t, fs.Images().Create(context.Background(), record))
	clock.Set(expires)

	resourceDir := filepath.Join(root, "images", testID)
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(resourceDir)
		return errors.Is(err, os.ErrNotExist)
	})
	_, err := fs.Images().Get(context.Background(), testID)
	assertCodeWithoutRoot(t, err, common.CodeNotFound, root)
}

func TestFSCleanupSharesReadLockAndPreservesReferencedGeneration(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("referenced"),
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
	resourceDir := filepath.Join(root, "whiteboards", testID)
	referenced := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))["content_filename"].(string)
	orphan := "source-11111111111111111111111111111111.md"
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, orphan), []byte("orphan"), filePermissions))

	key := resourceLockKey("whiteboards", testID)
	releaseRead, err := fs.locks.rlock(context.Background(), key)
	require.NoError(t, err)
	sweepDone := make(chan struct{})
	go func() {
		fs.sweep(fs.ctx)
		close(sweepDone)
	}()
	waitFor(t, 2*time.Second, func() bool { return lockRefs(&fs.locks, key) == 2 })
	require.Equal(t, record.Source, readFile(t, filepath.Join(resourceDir, referenced)))
	select {
	case <-sweepDone:
		t.Fatal("cleanup completed while the resource read lock was held")
	default:
	}
	releaseRead()
	select {
	case <-sweepDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not resume after the read lock was released")
	}
	_, err = os.Stat(filepath.Join(resourceDir, orphan))
	require.ErrorIs(t, err, os.ErrNotExist)
	got, err := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, record.Source, got.Source)
}

func TestFSCleanupRemovesUnreferencedGenerationsAndTemps(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := imageDomain.Image{
		ID: testID, Extension: ".png", MediaType: "image/png", Content: []byte("referenced"),
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	require.NoError(t, fs.Images().Create(context.Background(), record))
	resourceDir := filepath.Join(root, "images", testID)
	referenced := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))["content_filename"].(string)
	orphans := []string{
		"content-11111111111111111111111111111111",
		".content-temp-22222222222222222222222222222222",
		".metadata-temp-33333333333333333333333333333333",
	}
	for _, name := range orphans {
		require.NoError(t, os.WriteFile(filepath.Join(resourceDir, name), []byte("orphan"), filePermissions))
	}
	unknownName := "operator-notes.txt"
	unknownContent := []byte("unknown live-resource artifact")
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, unknownName), unknownContent, filePermissions))

	fs.sweep(fs.ctx)
	for _, name := range orphans {
		_, err := os.Stat(filepath.Join(resourceDir, name))
		require.ErrorIs(t, err, os.ErrNotExist, name)
	}
	require.Equal(t, record.Content, readFile(t, filepath.Join(resourceDir, referenced)))
	require.FileExists(t, filepath.Join(resourceDir, metadataFilename))
	require.Equal(t, unknownContent, readFile(t, filepath.Join(resourceDir, unknownName)))
}

func TestFSCleanupReclaimsOnlyRecognizedMetadataLessResources(t *testing.T) {
	tests := []struct {
		name       string
		artifacts  map[string][]byte
		wantExists bool
	}{
		{
			name: "recognized crash artifacts",
			artifacts: map[string][]byte{
				"content-11111111111111111111111111111111":        []byte("generation"),
				".metadata-temp-22222222222222222222222222222222": []byte("metadata temp"),
			},
		},
		{
			name: "unknown artifact",
			artifacts: map[string][]byte{
				"content-11111111111111111111111111111111": []byte("generation"),
				"operator-notes.txt":                       []byte("preserve exactly"),
			},
			wantExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			resourceDir := filepath.Join(root, "images", testID)
			require.NoError(t, os.Mkdir(resourceDir, directoryPermissions))
			for name, content := range tt.artifacts {
				require.NoError(t, os.WriteFile(filepath.Join(resourceDir, name), content, filePermissions))
			}

			fs.sweep(fs.ctx)
			_, err := os.Stat(resourceDir)
			if !tt.wantExists {
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			for name, content := range tt.artifacts {
				require.Equal(t, content, readFile(t, filepath.Join(resourceDir, name)))
			}
		})
	}
}

func TestFSCloseWaitsForActiveOperationAndRejectsNewOperations(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: []byte("held"),
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
	key := resourceLockKey("whiteboards", testID)
	releaseHeld, err := fs.locks.lock(context.Background(), key)
	require.NoError(t, err)
	operationDone := make(chan error, 1)
	go func() {
		_, err := fs.Whiteboards().Get(context.Background(), testID)
		operationDone <- err
	}()
	waitFor(t, 2*time.Second, func() bool { return lockRefs(&fs.locks, key) == 2 })

	closeDone := make(chan error, 1)
	go func() { closeDone <- fs.Close() }()
	waitFor(t, 2*time.Second, func() bool { return fsClosing(fs) })
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before active operation completed: %v", err)
	default:
	}

	_, err = fs.Whiteboards().Get(context.Background(), "invalid")
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	err = fs.Whiteboards().Create(context.Background(), whiteboardDomain.Whiteboard{})
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	err = fs.Whiteboards().Replace(context.Background(), whiteboardDomain.Whiteboard{})
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	err = fs.Whiteboards().Delete(context.Background(), "invalid")
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
	err = fs.Whiteboards().Ready(context.Background())
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)

	releaseHeld()
	select {
	case <-operationDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked active operation did not return after lock release")
	}
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after active operation completed")
	}
}

func TestFSConcurrentRepeatedCloseWaitsForOneCompletion(t *testing.T) {
	fs := newTestFS(t, t.TempDir())
	record := imageDomain.Image{
		ID: testID, Extension: ".png", MediaType: "image/png", Content: []byte("held while closing"),
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	require.NoError(t, fs.Images().Create(context.Background(), record))
	key := resourceLockKey("images", testID)
	releaseHeld, err := fs.locks.lock(context.Background(), key)
	require.NoError(t, err)
	operationDone := make(chan struct{})
	go func() {
		_, _ = fs.Images().Get(context.Background(), testID)
		close(operationDone)
	}()
	waitFor(t, 2*time.Second, func() bool { return lockRefs(&fs.locks, key) == 2 })

	const callers = 32
	start := make(chan struct{})
	results := make(chan error, callers)
	var callersDone sync.WaitGroup
	for caller := range callers {
		callersDone.Add(1)
		go func(caller int) {
			defer callersDone.Done()
			<-start
			if caller%2 == 0 {
				results <- fs.Whiteboards().Close()
				return
			}
			results <- fs.Images().Close()
		}(caller)
	}
	close(start)
	waitFor(t, 2*time.Second, func() bool { return fsClosing(fs) })
	select {
	case err := <-results:
		releaseHeld()
		t.Fatalf("concurrent Close returned before the shared completion point: %v", err)
	default:
	}
	releaseHeld()
	select {
	case <-operationDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active operation did not release concurrent Close callers")
	}
	callersDone.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.NoError(t, fs.Close())
}

func TestFSContextCancellationPreservesCommittedGeneration(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := whiteboardDomain.Whiteboard{ID: testID, Kind: whiteboardDomain.KindHTML, Source: []byte("old"), Context: []byte("old context"), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
	dir := filepath.Join(root, "whiteboards", testID)
	before := decodeMetadata(t, filepath.Join(dir, "metadata.json"))["content_filename"]
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	replacement := record
	replacement.Source = []byte("new")
	replacement.Context = []byte("new context")

	require.ErrorIs(t, fs.Whiteboards().Replace(canceled, replacement), context.Canceled)
	after := decodeMetadata(t, filepath.Join(dir, "metadata.json"))["content_filename"]
	require.Equal(t, before, after)
	got, err := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, record.ID, got.ID)
	require.Equal(t, record.Kind, got.Kind)
	require.Equal(t, record.Source, got.Source)
	require.Equal(t, record.Context, got.Context)
	require.True(t, record.CreatedAt.Equal(got.CreatedAt))
	require.True(t, record.UpdatedAt.Equal(got.UpdatedAt))
	require.Nil(t, got.ExpiresAt)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	require.ErrorIs(t, fs.Whiteboards().Delete(canceled, testID), context.Canceled)
	require.ErrorIs(t, fs.Whiteboards().Ready(canceled), context.Canceled)
	_, err = fs.Whiteboards().Get(canceled, testID)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFSReadinessAndSharedIdempotentClose(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	require.NoError(t, fs.Whiteboards().Ready(context.Background()))
	require.Empty(t, readDirNames(t, filepath.Join(root, ".readiness")))
	require.NoError(t, fs.Images().Close())
	require.NoError(t, fs.Whiteboards().Close())
	err := fs.Images().Ready(context.Background())
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
}

func newTestFS(t *testing.T, root string) *FS {
	t.Helper()
	return newTestFSWithClock(t, root, common.SystemClock{}, time.Hour)
}

func newTestFSWithClock(t *testing.T, root string, clock common.Clock, cleanupInterval time.Duration) *FS {
	t.Helper()
	fs, err := NewFS(Config{Root: root, CleanupInterval: cleanupInterval, Clock: clock, Context: context.Background()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fs.Close()) })
	return fs
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

type transitionClock struct {
	mu            sync.Mutex
	times         []time.Time
	firstObserved chan struct{}
	resumeFirst   chan struct{}
	calls         int
}

type sequenceIDGenerator struct {
	ids   []string
	index int
}

func (generator *sequenceIDGenerator) NewID() (string, error) {
	if generator.index >= len(generator.ids) {
		return "", errors.New("test ID sequence exhausted")
	}
	id := generator.ids[generator.index]
	generator.index++
	return id, nil
}

func (clock *transitionClock) Now() time.Time {
	clock.mu.Lock()
	if len(clock.times) == 0 {
		clock.mu.Unlock()
		panic("transition clock exhausted")
	}
	now := clock.times[0]
	clock.times = clock.times[1:]
	call := clock.calls
	clock.calls++
	clock.mu.Unlock()
	if call == 0 {
		close(clock.firstObserved)
		<-clock.resumeFirst
	}
	return now
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Set(now time.Time) {
	clock.mu.Lock()
	clock.now = now
	clock.mu.Unlock()
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition was not satisfied before deadline")
		case <-ticker.C:
		}
	}
}

func lockRefs(locks *lockSet, key string) int {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if entry := locks.entries[key]; entry != nil {
		return entry.refs
	}
	return 0
}

func lockEntryCount(locks *lockSet) int {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	return len(locks.entries)
}

func writeLockPending(locks *lockSet, key string) bool {
	locks.mu.Lock()
	entry := locks.entries[key]
	locks.mu.Unlock()
	if entry == nil {
		return false
	}
	if entry.mu.TryRLock() {
		entry.mu.RUnlock()
		return false
	}
	return true
}

func fsClosing(fs *FS) bool {
	fs.lifecycleMu.Lock()
	defer fs.lifecycleMu.Unlock()
	return fs.closing
}

func mustReadRootFile(t *testing.T, root *os.Root, name string) []byte {
	t.Helper()
	content, err := root.ReadFile(name)
	require.NoError(t, err)
	return content
}

func readMetadataForTest(root *os.Root) (metadata, error) {
	encoded, err := root.ReadFile(metadataFilename)
	if err != nil {
		return metadata{}, err
	}
	var stored metadata
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return metadata{}, err
	}
	return stored, nil
}

func assertPermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm(), path)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}

func encodedStoreTestPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	return encoded.Bytes()
}

func decodeMetadata(t *testing.T, path string) map[string]any {
	t.Helper()
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(readFile(t, path), &metadata))
	return metadata
}

func assertJSONTime(t *testing.T, value any, want time.Time) {
	t.Helper()
	object, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(want.Unix()), object["seconds"])
	require.Equal(t, float64(want.Nanosecond()), object["nanoseconds"])
}

func assertCodeWithoutRoot(t *testing.T, err error, code common.ErrorCode, root string) {
	t.Helper()
	require.Error(t, err)
	require.True(t, common.HasCode(err, code), "error: %v", err)
	if root != "" {
		require.NotContains(t, err.Error(), root)
	}
}

func readDirNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
