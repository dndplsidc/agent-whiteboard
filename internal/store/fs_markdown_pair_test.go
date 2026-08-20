package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	imageDomain "github.com/edocsss/agent-whiteboard/internal/image"
	whiteboardDomain "github.com/edocsss/agent-whiteboard/internal/whiteboard"
	"github.com/stretchr/testify/require"
)

func TestFSMarkdownSchema2PairRoundTripsExactBytes(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := markdownRecord([]byte("# source\x00\n"), []byte("context\x00\n"))

	require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
	got, err := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, record, got)

	resourceDir := filepath.Join(root, "whiteboards", testID)
	stored := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))
	require.Equal(t, float64(2), stored["schema_version"])
	sourceName := stored["content_filename"].(string)
	contextName := stored["context_filename"].(string)
	require.Regexp(t, `^source-[a-f0-9]{32}\.md$`, sourceName)
	require.Regexp(t, `^context-[a-f0-9]{32}\.md$`, contextName)
	require.Equal(t, generationToken(t, sourceName, "source-"), generationToken(t, contextName, "context-"))
	require.Equal(t, record.Source, readFile(t, filepath.Join(resourceDir, sourceName)))
	require.Equal(t, record.Context, readFile(t, filepath.Join(resourceDir, contextName)))
}

func TestFSLegacyMarkdownReadAndFirstUpdatePublishesPair(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	legacy := markdownRecord([]byte("legacy source"), nil)
	writeLegacyMarkdown(t, root, legacy)

	got, err := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, legacy.Source, got.Source)
	require.Empty(t, got.Context)

	replacement := markdownRecord([]byte("new source"), []byte("new context"))
	replacement.UpdatedAt = time.Unix(20, 0).UTC()
	require.NoError(t, fs.Whiteboards().Replace(context.Background(), replacement))
	got, err = fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, replacement.Source, got.Source)
	require.Equal(t, replacement.Context, got.Context)
	require.Equal(t, legacy.CreatedAt, got.CreatedAt)

	stored := decodeMetadata(t, filepath.Join(root, "whiteboards", testID, metadataFilename))
	require.Equal(t, float64(2), stored["schema_version"])
	require.NotEmpty(t, stored["context_filename"])
}

func TestFSRejectsMetadataThatDoesNotReferenceOneMarkdownGeneration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing context", mutate: func(stored map[string]any) { delete(stored, "context_filename") }},
		{name: "different generation", mutate: func(stored map[string]any) {
			stored["context_filename"] = "context-11111111111111111111111111111111.md"
		}},
		{name: "legacy schema with context", mutate: func(stored map[string]any) { stored["schema_version"] = float64(1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			require.NoError(t, fs.Whiteboards().Create(context.Background(), markdownRecord([]byte("source"), []byte("context"))))
			metadataPath := filepath.Join(root, "whiteboards", testID, metadataFilename)
			stored := decodeMetadata(t, metadataPath)
			tt.mutate(stored)
			encoded, err := json.Marshal(stored)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(metadataPath, encoded, filePermissions))

			_, err = fs.Whiteboards().Get(context.Background(), testID)
			assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
		})
	}
}

func TestFSMarkdownReplaceValidatesBothCurrentArtifacts(t *testing.T) {
	for _, missingField := range []string{"content_filename", "context_filename"} {
		t.Run(missingField, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			original := markdownRecord([]byte("old source"), []byte("old context"))
			require.NoError(t, fs.Whiteboards().Create(context.Background(), original))
			resourceDir := filepath.Join(root, "whiteboards", testID)
			before := readFile(t, filepath.Join(resourceDir, metadataFilename))
			stored := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))
			require.NoError(t, os.Remove(filepath.Join(resourceDir, stored[missingField].(string))))

			err := fs.Whiteboards().Replace(context.Background(), markdownRecord([]byte("new source"), []byte("new context")))
			assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
			require.Equal(t, before, readFile(t, filepath.Join(resourceDir, metadataFilename)))
		})
	}
}

func TestFSMarkdownCreateFailuresBeforeMetadataPublicationRemoveIncompleteResource(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*FS, error)
	}{
		{
			name: "source publication",
			inject: func(fs *FS, injected error) {
				fs.publishArtifact = func(*os.Root, string, string) error { return injected }
			},
		},
		{
			name: "context publication",
			inject: func(fs *FS, injected error) {
				calls := 0
				fs.publishArtifact = func(root *os.Root, temp, final string) error {
					calls++
					if calls == 2 {
						return injected
					}
					return publishGeneration(root, temp, final)
				}
			},
		},
		{
			name: "pre-metadata directory sync",
			inject: func(fs *FS, injected error) {
				fs.directorySync = func(root *os.Root) error {
					if root != fs.categories["whiteboards"] {
						return injected
					}
					return syncDirectory(root)
				}
			},
		},
		{
			name: "metadata rename",
			inject: func(fs *FS, injected error) {
				fs.renameArtifact = func(*os.Root, string, string) error { return injected }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			injected := errors.New("injected " + tt.name + " failure")
			tt.inject(fs, injected)

			err := fs.Whiteboards().Create(context.Background(), markdownRecord([]byte("source"), []byte("context")))
			require.ErrorIs(t, err, injected)
			_, statErr := os.Stat(filepath.Join(root, "whiteboards", testID))
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestFSMarkdownPairFailuresBeforeMetadataPublicationPreserveOldPair(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*FS, context.CancelFunc, error)
	}{
		{
			name: "source publication",
			inject: func(fs *FS, _ context.CancelFunc, injected error) {
				fs.publishArtifact = func(*os.Root, string, string) error { return injected }
			},
		},
		{
			name: "context publication",
			inject: func(fs *FS, _ context.CancelFunc, injected error) {
				calls := 0
				fs.publishArtifact = func(root *os.Root, temp, final string) error {
					calls++
					if calls == 2 {
						return injected
					}
					return publishGeneration(root, temp, final)
				}
			},
		},
		{
			name: "cancellation between publications",
			inject: func(fs *FS, cancel context.CancelFunc, _ error) {
				fs.publishArtifact = func(root *os.Root, temp, final string) error {
					err := publishGeneration(root, temp, final)
					cancel()
					return err
				}
			},
		},
		{
			name: "pre-metadata directory sync",
			inject: func(fs *FS, _ context.CancelFunc, injected error) {
				fs.directorySync = func(*os.Root) error { return injected }
			},
		},
		{
			name: "metadata rename",
			inject: func(fs *FS, _ context.CancelFunc, injected error) {
				fs.renameArtifact = func(*os.Root, string, string) error { return injected }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			old := markdownRecord([]byte("old source"), []byte("old context"))
			require.NoError(t, fs.Whiteboards().Create(context.Background(), old))
			resourceDir := filepath.Join(root, "whiteboards", testID)
			beforeMetadata := readFile(t, filepath.Join(resourceDir, metadataFilename))
			beforeNames := readDirNames(t, resourceDir)
			injected := errors.New("injected " + tt.name + " failure")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tt.inject(fs, cancel, injected)

			err := fs.Whiteboards().Replace(ctx, markdownRecord([]byte("new source"), []byte("new context")))
			if tt.name == "cancellation between publications" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, injected)
			}
			fs.publishArtifact = publishGeneration
			fs.renameArtifact = renameArtifact
			fs.directorySync = syncDirectory
			got, getErr := fs.Whiteboards().Get(context.Background(), testID)
			require.NoError(t, getErr)
			require.Equal(t, old.Source, got.Source)
			require.Equal(t, old.Context, got.Context)
			require.Equal(t, beforeMetadata, readFile(t, filepath.Join(resourceDir, metadataFilename)))
			require.ElementsMatch(t, beforeNames, readDirNames(t, resourceDir))
		})
	}
}

func TestFSMarkdownAppliedPublicationErrorsDoNotLeakGenerations(t *testing.T) {
	for _, failedCall := range []int{1, 2} {
		name := "source"
		if failedCall == 2 {
			name = "context"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			original := markdownRecord([]byte("old source"), []byte("old context"))
			require.NoError(t, fs.Whiteboards().Create(context.Background(), original))
			resourceDir := filepath.Join(root, "whiteboards", testID)
			beforeNames := readDirNames(t, resourceDir)
			injected := errors.New("injected applied publication failure")
			calls := 0
			fs.publishArtifact = func(root *os.Root, temp, final string) error {
				calls++
				if err := publishGeneration(root, temp, final); err != nil {
					return err
				}
				if calls == failedCall {
					return injected
				}
				return nil
			}

			err := fs.Whiteboards().Replace(context.Background(), markdownRecord([]byte("new source"), []byte("new context")))
			require.ErrorIs(t, err, injected)
			fs.publishArtifact = publishGeneration
			got, getErr := fs.Whiteboards().Get(context.Background(), testID)
			require.NoError(t, getErr)
			require.Equal(t, original.Source, got.Source)
			require.Equal(t, original.Context, got.Context)
			require.ElementsMatch(t, beforeNames, readDirNames(t, resourceDir))
		})
	}
}

func TestFSMarkdownAppliedMetadataRenameErrorKeepsPublishedPairReadable(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	original := markdownRecord([]byte("old source"), []byte("old context"))
	require.NoError(t, fs.Whiteboards().Create(context.Background(), original))
	injected := errors.New("injected applied metadata rename failure")
	fs.renameArtifact = func(root *os.Root, oldName, newName string) error {
		if err := renameArtifact(root, oldName, newName); err != nil {
			return err
		}
		return injected
	}
	replacement := markdownRecord([]byte("new source"), []byte("new context"))

	err := fs.Whiteboards().Replace(context.Background(), replacement)
	require.ErrorIs(t, err, injected)
	fs.renameArtifact = renameArtifact
	got, getErr := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, getErr)
	require.Equal(t, replacement.Source, got.Source)
	require.Equal(t, replacement.Context, got.Context)
	resourceDir := filepath.Join(root, "whiteboards", testID)
	stored := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))
	require.FileExists(t, filepath.Join(resourceDir, stored["content_filename"].(string)))
	require.FileExists(t, filepath.Join(resourceDir, stored["context_filename"].(string)))

	fs.sweep(fs.ctx)
	require.ElementsMatch(t, []string{
		metadataFilename,
		stored["content_filename"].(string),
		stored["context_filename"].(string),
	}, readDirNames(t, resourceDir))
}

func TestFSMarkdownUncertainMetadataRenamePreservesThenCleansOrphans(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	original := markdownRecord([]byte("old source"), []byte("old context"))
	require.NoError(t, fs.Whiteboards().Create(context.Background(), original))
	resourceDir := filepath.Join(root, "whiteboards", testID)
	beforeNames := readDirNames(t, resourceDir)
	injected := errors.New("injected uncertain metadata rename failure")
	fs.renameArtifact = func(*os.Root, string, string) error { return injected }
	fs.inspectMetadataPublication = func(*os.Root, []byte) publicationState { return publicationUncertain }

	err := fs.Whiteboards().Replace(context.Background(), markdownRecord([]byte("new source"), []byte("new context")))
	require.ErrorIs(t, err, injected)
	got, getErr := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, getErr)
	require.Equal(t, original.Source, got.Source)
	require.Equal(t, original.Context, got.Context)
	require.Len(t, readDirNames(t, resourceDir), len(beforeNames)+2)

	fs.renameArtifact = renameArtifact
	fs.inspectMetadataPublication = inspectMetadataPublication
	fs.sweep(fs.ctx)
	require.ElementsMatch(t, beforeNames, readDirNames(t, resourceDir))
}

func TestFSMarkdownLinkedPublicationErrorIsCleaned(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	original := markdownRecord([]byte("old source"), []byte("old context"))
	require.NoError(t, fs.Whiteboards().Create(context.Background(), original))
	resourceDir := filepath.Join(root, "whiteboards", testID)
	beforeNames := readDirNames(t, resourceDir)
	injected := errors.New("injected uncertain publication failure")
	fs.publishArtifact = func(root *os.Root, temp, final string) error {
		if err := root.Link(temp, final); err != nil {
			return err
		}
		return injected
	}

	err := fs.Whiteboards().Replace(context.Background(), markdownRecord([]byte("new source"), []byte("new context")))
	require.ErrorIs(t, err, injected)
	fs.publishArtifact = publishGeneration
	got, getErr := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, getErr)
	require.Equal(t, original.Source, got.Source)
	require.Equal(t, original.Context, got.Context)

	fs.sweep(fs.ctx)
	require.ElementsMatch(t, beforeNames, readDirNames(t, resourceDir))
}

func TestFSMarkdownPairPostCommitFailuresNeverRollBack(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*FS, map[string]any, error)
	}{
		{
			name: "post-commit directory sync",
			inject: func(fs *FS, _ map[string]any, injected error) {
				calls := 0
				fs.directorySync = func(root *os.Root) error {
					calls++
					if calls == 2 {
						return injected
					}
					return syncDirectory(root)
				}
			},
		},
		{
			name: "old source cleanup",
			inject: func(fs *FS, old map[string]any, injected error) {
				fs.removeArtifact = func(root *os.Root, name string) error {
					if name == old["content_filename"].(string) {
						return injected
					}
					return root.Remove(name)
				}
			},
		},
		{
			name: "old context cleanup",
			inject: func(fs *FS, old map[string]any, injected error) {
				fs.removeArtifact = func(root *os.Root, name string) error {
					if name == old["context_filename"].(string) {
						return injected
					}
					return root.Remove(name)
				}
			},
		},
		{
			name: "old pair cleanup directory sync",
			inject: func(fs *FS, _ map[string]any, injected error) {
				calls := 0
				fs.directorySync = func(root *os.Root) error {
					calls++
					if calls == 3 {
						return injected
					}
					return syncDirectory(root)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fs := newTestFS(t, root)
			old := markdownRecord([]byte("old source"), []byte("old context"))
			require.NoError(t, fs.Whiteboards().Create(context.Background(), old))
			resourceDir := filepath.Join(root, "whiteboards", testID)
			oldMetadata := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))
			injected := errors.New("injected " + tt.name + " failure")
			tt.inject(fs, oldMetadata, injected)
			replacement := markdownRecord([]byte("new source"), []byte("new context"))

			err := fs.Whiteboards().Replace(context.Background(), replacement)
			require.ErrorIs(t, err, injected)
			fs.removeArtifact = removeArtifact
			fs.directorySync = syncDirectory
			got, getErr := fs.Whiteboards().Get(context.Background(), testID)
			require.NoError(t, getErr)
			require.Equal(t, replacement.Source, got.Source)
			require.Equal(t, replacement.Context, got.Context)
			stored := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))
			require.Equal(t, float64(2), stored["schema_version"])
			require.Equal(t, generationToken(t, stored["content_filename"].(string), "source-"), generationToken(t, stored["context_filename"].(string), "context-"))
		})
	}
}

func TestFSCleanupPreservesLiveMarkdownPairAndUnknownFiles(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := markdownRecord([]byte("live source"), []byte("live context"))
	require.NoError(t, fs.Whiteboards().Create(context.Background(), record))
	resourceDir := filepath.Join(root, "whiteboards", testID)
	stored := decodeMetadata(t, filepath.Join(resourceDir, metadataFilename))
	orphans := []string{
		"source-11111111111111111111111111111111.md",
		"context-11111111111111111111111111111111.md",
		".content-temp-22222222222222222222222222222222",
		".context-temp-33333333333333333333333333333333",
		".metadata-temp-44444444444444444444444444444444",
	}
	for _, name := range orphans {
		require.NoError(t, os.WriteFile(filepath.Join(resourceDir, name), []byte("orphan"), filePermissions))
	}
	unknown := filepath.Join(resourceDir, "operator-notes.txt")
	require.NoError(t, os.WriteFile(unknown, []byte("preserve"), filePermissions))

	fs.sweep(fs.ctx)

	for _, name := range orphans {
		_, err := os.Stat(filepath.Join(resourceDir, name))
		require.ErrorIs(t, err, os.ErrNotExist, name)
	}
	require.Equal(t, record.Source, readFile(t, filepath.Join(resourceDir, stored["content_filename"].(string))))
	require.Equal(t, record.Context, readFile(t, filepath.Join(resourceDir, stored["context_filename"].(string))))
	require.Equal(t, []byte("preserve"), readFile(t, unknown))
}

func TestFSConcurrentReadersObserveCompleteMarkdownPairs(t *testing.T) {
	fs := newTestFS(t, t.TempDir())
	old := markdownRecord([]byte("old source"), []byte("old context"))
	newRecord := markdownRecord([]byte("new source"), []byte("new context"))
	newRecord.UpdatedAt = time.Unix(30, 0).UTC()
	require.NoError(t, fs.Whiteboards().Create(context.Background(), old))

	const readers = 32
	start := make(chan struct{})
	ready := make(chan struct{}, readers)
	stop := make(chan struct{})
	errs := make(chan error, readers)
	var workers sync.WaitGroup
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			firstRead := true
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := fs.Whiteboards().Get(context.Background(), testID)
				if err != nil {
					errs <- err
					return
				}
				if firstRead {
					ready <- struct{}{}
					firstRead = false
				}
				oldPair := string(got.Source) == string(old.Source) && string(got.Context) == string(old.Context)
				newPair := string(got.Source) == string(newRecord.Source) && string(got.Context) == string(newRecord.Context)
				if !oldPair && !newPair {
					errs <- errors.New("observed mixed Markdown generation")
					return
				}
			}
		}()
	}
	close(start)
	for range readers {
		<-ready
	}
	require.NoError(t, fs.Whiteboards().Replace(context.Background(), newRecord))
	close(stop)
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestFSHTMLUsesSchema2PairAndImageRemainsSchema1(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	html := htmlRecord([]byte("<!doctype html><html><head></head><body>exact</body></html>"), []byte("creator notes\x00\n"))
	image := imageDomain.Image{ID: testID2, Extension: ".png", MediaType: "image/png", Content: []byte("image"), CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0)}
	require.NoError(t, fs.Whiteboards().Create(context.Background(), html))
	require.NoError(t, fs.Images().Create(context.Background(), image))

	htmlDir := filepath.Join(root, "whiteboards", testID)
	storedHTML := decodeMetadata(t, filepath.Join(htmlDir, metadataFilename))
	require.Equal(t, float64(2), storedHTML["schema_version"])
	sourceName := storedHTML["content_filename"].(string)
	contextName := storedHTML["context_filename"].(string)
	require.Regexp(t, `^source-[a-f0-9]{32}\.html$`, sourceName)
	require.Regexp(t, `^context-[a-f0-9]{32}\.md$`, contextName)
	require.Equal(t, generationToken(t, sourceName, "source-"), generationToken(t, contextName, "context-"))
	require.Equal(t, html.Source, readFile(t, filepath.Join(htmlDir, sourceName)))
	require.Equal(t, html.Context, readFile(t, filepath.Join(htmlDir, contextName)))

	imageDir := filepath.Join(root, "images", testID2)
	storedImage := decodeMetadata(t, filepath.Join(imageDir, metadataFilename))
	require.Equal(t, float64(1), storedImage["schema_version"])
	require.NotEmpty(t, storedImage["content_filename"])
	require.NotContains(t, storedImage, "context_filename")
	require.Len(t, readDirNames(t, imageDir), 2)
}

func TestFSLegacySchema1HTMLIsRejected(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	record := htmlRecord([]byte("<!doctype html><html><head></head><body>legacy</body></html>"), nil)
	resourceDir := filepath.Join(root, "whiteboards", record.ID)
	require.NoError(t, os.Mkdir(resourceDir, directoryPermissions))
	generation := "source-00000000000000000000000000000000.html"
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, generation), record.Source, filePermissions))
	stored := metadata{SchemaVersion: 1, Kind: string(whiteboardDomain.KindHTML), CreatedAt: fromTime(record.CreatedAt), UpdatedAt: fromTime(record.UpdatedAt), ContentFilename: generation}
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, metadataFilename), encoded, filePermissions))

	_, err = fs.Whiteboards().Get(context.Background(), record.ID)
	assertCodeWithoutRoot(t, err, common.CodeStorageUnavailable, root)
}

func TestFSHTMLPairFailureBeforeMetadataPreservesOldPair(t *testing.T) {
	root := t.TempDir()
	fs := newTestFS(t, root)
	old := htmlRecord([]byte("<!doctype html><html><head></head><body>old</body></html>"), []byte("old notes"))
	require.NoError(t, fs.Whiteboards().Create(context.Background(), old))
	resourceDir := filepath.Join(root, "whiteboards", testID)
	beforeMetadata := readFile(t, filepath.Join(resourceDir, metadataFilename))

	injected := errors.New("context publication failed")
	calls := 0
	fs.publishArtifact = func(root *os.Root, temp, final string) error {
		calls++
		if calls == 2 {
			return injected
		}
		return publishGeneration(root, temp, final)
	}
	newRecord := htmlRecord([]byte("<!doctype html><html><head></head><body>new</body></html>"), []byte("new notes"))
	err := fs.Whiteboards().Replace(context.Background(), newRecord)
	require.ErrorIs(t, err, injected)
	require.Equal(t, beforeMetadata, readFile(t, filepath.Join(resourceDir, metadataFilename)))
	got, getErr := fs.Whiteboards().Get(context.Background(), testID)
	require.NoError(t, getErr)
	require.Equal(t, old.Source, got.Source)
	require.Equal(t, old.Context, got.Context)
}

func markdownRecord(source, creatorContext []byte) whiteboardDomain.Whiteboard {
	return whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindMarkdown, Source: source, Context: creatorContext,
		CreatedAt: time.Unix(10, 0).UTC(), UpdatedAt: time.Unix(11, 0).UTC(),
	}
}

func htmlRecord(source, creatorContext []byte) whiteboardDomain.Whiteboard {
	return whiteboardDomain.Whiteboard{
		ID: testID, Kind: whiteboardDomain.KindHTML, Source: source, Context: creatorContext,
		CreatedAt: time.Unix(10, 0).UTC(), UpdatedAt: time.Unix(11, 0).UTC(),
	}
}

func writeLegacyMarkdown(t *testing.T, root string, record whiteboardDomain.Whiteboard) {
	t.Helper()
	resourceDir := filepath.Join(root, "whiteboards", record.ID)
	require.NoError(t, os.Mkdir(resourceDir, directoryPermissions))
	generation := "source-00000000000000000000000000000000.md"
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, generation), record.Source, filePermissions))
	stored := metadata{
		SchemaVersion:   1,
		Kind:            string(whiteboardDomain.KindMarkdown),
		CreatedAt:       fromTime(record.CreatedAt),
		UpdatedAt:       fromTime(record.UpdatedAt),
		ExpiresAt:       fromTimePtr(record.ExpiresAt),
		ContentFilename: generation,
	}
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, metadataFilename), encoded, filePermissions))
}

func generationToken(t *testing.T, name, prefix string) string {
	t.Helper()
	require.GreaterOrEqual(t, len(name), len(prefix)+32)
	require.Equal(t, prefix, name[:len(prefix)])
	return name[len(prefix) : len(prefix)+32]
}
