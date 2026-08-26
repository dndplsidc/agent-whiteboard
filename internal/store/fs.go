package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	imageDomain "github.com/dndplsidc/agent-whiteboard/internal/image"
	whiteboardDomain "github.com/dndplsidc/agent-whiteboard/internal/whiteboard"
)

const (
	legacyMetadataSchemaVersion = 1
	pairedMetadataSchemaVersion = 2
	metadataFilename            = "metadata.json"
	directoryPermissions        = 0o700
	filePermissions             = 0o600
	randomNameAttempts          = 10
)

var (
	whiteboardGenerationPattern = regexp.MustCompile(`^source-[a-f0-9]{32}\.(md|html)$`)
	pairedSourcePattern         = regexp.MustCompile(`^source-([a-f0-9]{32})\.(md|html)$`)
	creatorContextPattern       = regexp.MustCompile(`^context-([a-f0-9]{32})\.md$`)
	imageGenerationPattern      = regexp.MustCompile(`^content-[a-f0-9]{32}$`)
	temporaryArtifactPattern    = regexp.MustCompile(`^\.(content|context|metadata)-temp-[a-f0-9]{32}$`)
)

type Config struct {
	Root            string
	CleanupInterval time.Duration
	Clock           common.Clock
	Context         context.Context
}

type FS struct {
	rootPath                   string
	rootHandle                 *os.Root
	categories                 map[string]*os.Root
	clock                      common.Clock
	ctx                        context.Context
	cancel                     context.CancelFunc
	interval                   time.Duration
	locks                      lockSet
	directorySync              func(*os.Root) error
	publishArtifact            func(*os.Root, string, string) error
	renameArtifact             func(*os.Root, string, string) error
	removeArtifact             func(*os.Root, string) error
	removeResource             func(*os.Root, string) error
	inspectMetadataPublication func(*os.Root, []byte) publicationState

	lifecycleMu sync.Mutex
	closing     bool
	active      sync.WaitGroup
	cleanup     sync.WaitGroup
	closeDone   chan struct{}
	closeErr    error
}

type lockEntry struct {
	mu   sync.RWMutex
	refs int
}

type lockSet struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type whiteboardView struct{ fs *FS }
type imageView struct{ fs *FS }

var _ whiteboardDomain.Store = (*whiteboardView)(nil)
var _ imageDomain.Store = (*imageView)(nil)

type storedTime struct {
	Seconds     int64 `json:"seconds"`
	Nanoseconds int32 `json:"nanoseconds"`
}

type metadata struct {
	SchemaVersion   int         `json:"schema_version"`
	Kind            string      `json:"kind"`
	CreatedAt       storedTime  `json:"created_at"`
	UpdatedAt       storedTime  `json:"updated_at"`
	ExpiresAt       *storedTime `json:"expires_at"`
	ContentFilename string      `json:"content_filename"`
	ContextFilename string      `json:"context_filename,omitempty"`
	Extension       string      `json:"extension"`
	MediaType       string      `json:"media_type"`
}

func NewFS(config Config) (*FS, error) {
	if config.Root == "" {
		return nil, invalidRequest("storage root is required")
	}
	if err := config.Context.Err(); err != nil {
		return nil, err
	}
	if common.IsNil(config.Clock) {
		return nil, invalidRequest("storage clock is required")
	}

	absoluteRoot, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, storageUnavailable(err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(absoluteRoot, directoryPermissions); err != nil {
			return nil, storageUnavailable(err)
		}
		rootInfo, err = os.Lstat(absoluteRoot)
	}
	if err != nil || !realDirectory(rootInfo) {
		return nil, storageUnavailable(err)
	}
	rootHandle, err := openVerifiedFilesystemRoot(absoluteRoot, rootInfo)
	if err != nil {
		return nil, storageUnavailable(err)
	}
	if err := chmodRootDirectory(rootHandle); err != nil {
		_ = rootHandle.Close()
		return nil, storageUnavailable(err)
	}

	ctx, cancel := context.WithCancel(config.Context)
	fs := &FS{
		rootPath: absoluteRoot, rootHandle: rootHandle, categories: make(map[string]*os.Root, 3),
		clock: config.Clock, ctx: ctx, cancel: cancel, interval: config.CleanupInterval,
		locks: lockSet{entries: make(map[string]*lockEntry)}, directorySync: syncDirectory,
		publishArtifact: publishGeneration, renameArtifact: renameArtifact, removeArtifact: removeArtifact,
		removeResource:             func(root *os.Root, name string) error { return root.RemoveAll(name) },
		inspectMetadataPublication: inspectMetadataPublication,
		closeDone:                  make(chan struct{}),
	}
	for _, name := range []string{"whiteboards", "images", ".readiness"} {
		if err := ctx.Err(); err != nil {
			fs.closeHandles()
			cancel()
			return nil, err
		}
		if _, err := fs.containedPath(name); err != nil {
			fs.closeHandles()
			cancel()
			return nil, storageUnavailable(err)
		}
		category, err := ensureManagedRoot(rootHandle, name)
		if err != nil {
			fs.closeHandles()
			cancel()
			return nil, storageUnavailable(err)
		}
		fs.categories[name] = category
	}
	fs.cleanup.Add(1)
	go fs.cleanupLoop()
	return fs, nil
}

func (fs *FS) Whiteboards() whiteboardDomain.Store { return &whiteboardView{fs: fs} }
func (fs *FS) Images() imageDomain.Store           { return &imageView{fs: fs} }

func (view *whiteboardView) Create(ctx context.Context, record whiteboardDomain.Whiteboard) error {
	if err := view.fs.beginOperation(ctx); err != nil {
		return err
	}
	defer view.fs.active.Done()
	if err := validateWhiteboardRecord(record); err != nil {
		return err
	}
	return view.fs.create(ctx, "whiteboards", record.ID, record.Source, record.Context, whiteboardMetadata(record))
}

func (view *whiteboardView) Get(ctx context.Context, id string) (whiteboardDomain.Whiteboard, error) {
	if err := view.fs.beginOperation(ctx); err != nil {
		return whiteboardDomain.Whiteboard{}, err
	}
	defer view.fs.active.Done()
	if err := common.ValidateID(id); err != nil {
		return whiteboardDomain.Whiteboard{}, err
	}
	content, creatorContext, stored, err := view.fs.get(ctx, "whiteboards", id, "whiteboard")
	if err != nil {
		return whiteboardDomain.Whiteboard{}, err
	}
	return whiteboardDomain.Whiteboard{
		ID: id, Kind: whiteboardDomain.Kind(stored.Kind), Source: content, Context: creatorContext,
		CreatedAt: stored.CreatedAt.time(), UpdatedAt: stored.UpdatedAt.time(), ExpiresAt: stored.ExpiresAt.timePtr(),
	}, nil
}

func (view *whiteboardView) Replace(ctx context.Context, record whiteboardDomain.Whiteboard) error {
	if err := view.fs.beginOperation(ctx); err != nil {
		return err
	}
	defer view.fs.active.Done()
	if err := validateWhiteboardRecord(record); err != nil {
		return err
	}
	return view.fs.replace(ctx, "whiteboards", record.ID, record.Source, record.Context, whiteboardMetadata(record), "whiteboard")
}

func (view *whiteboardView) Delete(ctx context.Context, id string) error {
	if err := view.fs.beginOperation(ctx); err != nil {
		return err
	}
	defer view.fs.active.Done()
	if err := common.ValidateID(id); err != nil {
		return err
	}
	return view.fs.delete(ctx, "whiteboards", id, "whiteboard")
}

func (view *whiteboardView) Ready(ctx context.Context) error { return view.fs.Ready(ctx) }
func (view *whiteboardView) Close() error                    { return view.fs.Close() }

func (view *imageView) Create(ctx context.Context, record imageDomain.Image) error {
	if err := view.fs.beginOperation(ctx); err != nil {
		return err
	}
	defer view.fs.active.Done()
	if err := validateImageRecord(record); err != nil {
		return err
	}
	return view.fs.create(ctx, "images", record.ID, record.Content, nil, imageMetadata(record))
}

func (view *imageView) Get(ctx context.Context, id string) (imageDomain.Image, error) {
	if err := view.fs.beginOperation(ctx); err != nil {
		return imageDomain.Image{}, err
	}
	defer view.fs.active.Done()
	if err := common.ValidateID(id); err != nil {
		return imageDomain.Image{}, err
	}
	content, _, stored, err := view.fs.get(ctx, "images", id, "image")
	if err != nil {
		return imageDomain.Image{}, err
	}
	return imageDomain.Image{
		ID: id, Extension: stored.Extension, MediaType: stored.MediaType, Content: content,
		CreatedAt: stored.CreatedAt.time(), UpdatedAt: stored.UpdatedAt.time(), ExpiresAt: stored.ExpiresAt.timePtr(),
	}, nil
}

func (view *imageView) Replace(ctx context.Context, record imageDomain.Image) error {
	if err := view.fs.beginOperation(ctx); err != nil {
		return err
	}
	defer view.fs.active.Done()
	if err := validateImageRecord(record); err != nil {
		return err
	}
	return view.fs.replace(ctx, "images", record.ID, record.Content, nil, imageMetadata(record), "image")
}

func (view *imageView) Delete(ctx context.Context, id string) error {
	if err := view.fs.beginOperation(ctx); err != nil {
		return err
	}
	defer view.fs.active.Done()
	if err := common.ValidateID(id); err != nil {
		return err
	}
	return view.fs.delete(ctx, "images", id, "image")
}

func (view *imageView) Ready(ctx context.Context) error { return view.fs.Ready(ctx) }
func (view *imageView) Close() error                    { return view.fs.Close() }

func (fs *FS) create(ctx context.Context, namespace, id string, content, creatorContext []byte, stored metadata) (resultErr error) {
	release, err := fs.locks.lock(ctx, resourceLockKey(namespace, id))
	if err != nil {
		return err
	}
	defer release()
	category, err := fs.category(namespace)
	if err != nil {
		return err
	}
	if _, err := fs.resourcePath(namespace, id); err != nil {
		return err
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return err
	}
	if err := category.Mkdir(id, directoryPermissions); err != nil {
		info, statErr := category.Lstat(id)
		if errors.Is(err, os.ErrExist) && statErr == nil && realDirectory(info) {
			return common.ErrIDCollision
		}
		return storageUnavailable(err)
	}

	var resource *os.Root
	defer func() {
		if resource != nil {
			_ = resource.Close()
		}
		if resultErr == nil {
			return
		}
		if rollbackErr := fs.rollbackCreatedResource(category, id); rollbackErr != nil {
			resultErr = &uncertainCreateError{operation: resultErr, rollback: rollbackErr}
		}
	}()

	if err := fs.directorySync(category); err != nil {
		return storageUnavailable(err)
	}
	resource, err = openVerifiedNestedRoot(category, id)
	if err != nil {
		return storageUnavailable(err)
	}
	if err := chmodRootDirectory(resource); err != nil {
		return storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return err
	}
	_, err = fs.commit(ctx, resource, content, creatorContext, &stored, nil)
	return err
}

func (fs *FS) rollbackCreatedResource(category *os.Root, id string) error {
	removeErr := fs.removeResource(category, id)
	_, statErr := category.Lstat(id)
	if errors.Is(statErr, os.ErrNotExist) {
		return fs.directorySync(category)
	}
	if statErr == nil {
		statErr = errors.New("created resource remains after rollback")
	}
	return errors.Join(removeErr, statErr)
}

type uncertainCreateError struct {
	operation error
	rollback  error
}

func (err *uncertainCreateError) Error() string          { return err.operation.Error() }
func (err *uncertainCreateError) Unwrap() []error        { return []error{err.operation, err.rollback} }
func (err *uncertainCreateError) ResourceMayExist() bool { return true }

func (fs *FS) get(ctx context.Context, namespace, id, expectedKind string) ([]byte, []byte, metadata, error) {
	key := resourceLockKey(namespace, id)
	release, err := fs.locks.rlock(ctx, key)
	if err != nil {
		return nil, nil, metadata{}, err
	}
	resource, err := fs.openResource(namespace, id)
	if err != nil {
		release()
		return nil, nil, metadata{}, err
	}
	stored, err := fs.loadMetadata(ctx, resource, expectedKind)
	if err != nil {
		_ = resource.Close()
		release()
		return nil, nil, metadata{}, err
	}
	if fs.isExpired(stored) {
		_ = resource.Close()
		release()
		return fs.getAfterExpiration(ctx, namespace, id, expectedKind, key)
	}
	content, creatorContext, err := fs.readStoredContent(namespace, id, resource, stored)
	closeErr := resource.Close()
	release()
	if err != nil {
		return nil, nil, metadata{}, err
	}
	if closeErr != nil {
		return nil, nil, metadata{}, storageUnavailable(closeErr)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return nil, nil, metadata{}, err
	}
	return content, creatorContext, stored, nil
}

func (fs *FS) getAfterExpiration(ctx context.Context, namespace, id, expectedKind, key string) ([]byte, []byte, metadata, error) {
	release, err := fs.locks.lock(ctx, key)
	if err != nil {
		return nil, nil, metadata{}, err
	}
	defer release()
	resource, err := fs.openResource(namespace, id)
	if err != nil {
		return nil, nil, metadata{}, err
	}
	stored, err := fs.loadMetadata(ctx, resource, expectedKind)
	if err != nil {
		_ = resource.Close()
		return nil, nil, metadata{}, err
	}
	if fs.isExpired(stored) {
		if err := fs.removeOpenedResource(ctx, namespace, id, resource); err != nil {
			return nil, nil, metadata{}, err
		}
		return nil, nil, metadata{}, notFound()
	}
	content, creatorContext, err := fs.readStoredContent(namespace, id, resource, stored)
	closeErr := resource.Close()
	if err != nil {
		return nil, nil, metadata{}, err
	}
	if closeErr != nil {
		return nil, nil, metadata{}, storageUnavailable(closeErr)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return nil, nil, metadata{}, err
	}
	return content, creatorContext, stored, nil
}

func (fs *FS) readStoredContent(namespace, id string, resource *os.Root, stored metadata) ([]byte, []byte, error) {
	if err := fs.validateResourceFilename(namespace, id, stored.ContentFilename); err != nil {
		return nil, nil, storageUnavailable(err)
	}
	content, err := readVerifiedFile(resource, stored.ContentFilename)
	if err != nil {
		return nil, nil, storageUnavailable(err)
	}
	if stored.ContextFilename == "" {
		return content, nil, nil
	}
	if err := fs.validateResourceFilename(namespace, id, stored.ContextFilename); err != nil {
		return nil, nil, storageUnavailable(err)
	}
	creatorContext, err := readVerifiedFile(resource, stored.ContextFilename)
	if err != nil {
		return nil, nil, storageUnavailable(err)
	}
	return content, creatorContext, nil
}

func (fs *FS) replace(ctx context.Context, namespace, id string, content, creatorContext []byte, stored metadata, expectedKind string) error {
	release, err := fs.locks.lock(ctx, resourceLockKey(namespace, id))
	if err != nil {
		return err
	}
	defer release()
	resource, err := fs.openResource(namespace, id)
	if err != nil {
		return err
	}
	old, err := fs.loadMetadata(ctx, resource, expectedKind)
	if err != nil {
		_ = resource.Close()
		return err
	}
	if fs.isExpired(old) {
		if err := fs.removeOpenedResource(ctx, namespace, id, resource); err != nil {
			return err
		}
		return notFound()
	}
	defer resource.Close()
	for _, name := range referencedArtifacts(old) {
		if err := fs.validateResourceFilename(namespace, id, name); err != nil {
			return storageUnavailable(err)
		}
		oldFile, err := openVerifiedRegular(resource, name)
		if err != nil {
			return storageUnavailable(err)
		}
		if err := oldFile.Close(); err != nil {
			return storageUnavailable(err)
		}
	}
	stored.CreatedAt = old.CreatedAt
	_, err = fs.commit(ctx, resource, content, creatorContext, &stored, referencedArtifacts(old))
	return err
}

func (fs *FS) delete(ctx context.Context, namespace, id, expectedKind string) error {
	release, err := fs.locks.lock(ctx, resourceLockKey(namespace, id))
	if err != nil {
		return err
	}
	defer release()
	resource, err := fs.openResource(namespace, id)
	if err != nil {
		return err
	}
	stored, err := fs.loadMetadata(ctx, resource, expectedKind)
	if err != nil {
		_ = resource.Close()
		return err
	}
	expired := fs.isExpired(stored)
	if err := fs.removeOpenedResource(ctx, namespace, id, resource); err != nil {
		return err
	}
	if expired {
		return notFound()
	}
	return nil
}

func (fs *FS) Ready(ctx context.Context) error {
	if err := fs.beginOperation(ctx); err != nil {
		return err
	}
	defer fs.active.Done()
	readiness, err := fs.category(".readiness")
	if err != nil {
		return err
	}
	probeName, probe, err := createExclusiveTemp(readiness, ".probe-")
	if err != nil {
		return storageUnavailable(err)
	}
	defer func() {
		_ = probe.Close()
		_ = readiness.Remove(probeName)
	}()
	if err := probe.Chmod(filePermissions); err != nil {
		return storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return err
	}
	if err := writeAll(probe, []byte("ready")); err != nil {
		return storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return err
	}
	if err := probe.Sync(); err != nil {
		return storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return err
	}
	if err := probe.Close(); err != nil {
		return storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return err
	}
	if err := readiness.Remove(probeName); err != nil {
		return storageUnavailable(err)
	}
	return nil
}

func (fs *FS) Close() error {
	fs.lifecycleMu.Lock()
	if fs.closing {
		done := fs.closeDone
		fs.lifecycleMu.Unlock()
		<-done
		return fs.closeErr
	}
	fs.closing = true
	done := fs.closeDone
	fs.lifecycleMu.Unlock()

	fs.cancel()
	fs.cleanup.Wait()
	fs.active.Wait()
	closeErr := fs.closeHandles()
	if closeErr != nil {
		closeErr = storageUnavailable(closeErr)
	}

	fs.lifecycleMu.Lock()
	fs.closeErr = closeErr
	close(done)
	fs.lifecycleMu.Unlock()
	return closeErr
}

func (fs *FS) commit(ctx context.Context, resource *os.Root, content, creatorContext []byte, stored *metadata, oldArtifacts []string) (bool, error) {
	if stored.SchemaVersion == pairedMetadataSchemaVersion {
		return fs.commitWhiteboardPair(ctx, resource, content, creatorContext, stored, oldArtifacts)
	}
	return fs.commitSingle(ctx, resource, content, stored, oldArtifacts)
}

func (fs *FS) commitSingle(ctx context.Context, resource *os.Root, content []byte, stored *metadata, oldArtifacts []string) (bool, error) {
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	contentTempName, contentTemp, err := createExclusiveTemp(resource, ".content-temp-")
	if err != nil {
		return false, storageUnavailable(err)
	}
	publishedGeneration := ""
	metadataTempName := ""
	metadataPublished := false
	preservePublishedGeneration := false
	defer func() {
		_ = contentTemp.Close()
		if contentTempName != "" {
			_ = resource.Remove(contentTempName)
		}
		if metadataTempName != "" {
			_ = resource.Remove(metadataTempName)
		}
		if publishedGeneration != "" && !metadataPublished && !preservePublishedGeneration {
			_ = resource.Remove(publishedGeneration)
		}
	}()
	if err := contentTemp.Chmod(filePermissions); err != nil {
		return false, storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := writeAll(contentTemp, content); err != nil {
		return false, storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := contentTemp.Sync(); err != nil {
		return false, storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := contentTemp.Close(); err != nil {
		return false, storageUnavailable(err)
	}
	for attempt := 0; attempt < randomNameAttempts; attempt++ {
		if err := ctxErr(ctx, fs.ctx); err != nil {
			return false, err
		}
		generation, err := generationFilename(stored.Kind)
		if err != nil {
			return false, storageUnavailable(err)
		}
		err = publishGeneration(resource, contentTempName, generation)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return false, storageUnavailable(err)
		}
		publishedGeneration = generation
		contentTempName = ""
		break
	}
	if publishedGeneration == "" {
		return false, storageUnavailable(errors.New("generation collision limit exceeded"))
	}
	if err := fs.directorySync(resource); err != nil {
		return false, storageUnavailable(err)
	}
	stored.ContentFilename = publishedGeneration
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return false, storageUnavailable(err)
	}
	metadataTempName, metadataTemp, err := createExclusiveTemp(resource, ".metadata-temp-")
	if err != nil {
		return false, storageUnavailable(err)
	}
	defer metadataTemp.Close()
	if err := metadataTemp.Chmod(filePermissions); err != nil {
		return false, storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := writeAll(metadataTemp, encoded); err != nil {
		return false, storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := metadataTemp.Sync(); err != nil {
		return false, storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := metadataTemp.Close(); err != nil {
		return false, storageUnavailable(err)
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := fs.renameArtifact(resource, metadataTempName, metadataFilename); err != nil {
		switch fs.inspectMetadataPublication(resource, encoded) {
		case publicationApplied:
			metadataTempName = ""
			metadataPublished = true
			return true, storageUnavailable(err)
		case publicationUncertain:
			preservePublishedGeneration = true
			return true, storageUnavailable(err)
		default:
			return false, storageUnavailable(err)
		}
	}
	metadataTempName = ""
	metadataPublished = true
	if err := fs.directorySync(resource); err != nil {
		return true, storageUnavailable(err)
	}
	removedOld := false
	for _, oldGeneration := range oldArtifacts {
		if oldGeneration != "" && oldGeneration != publishedGeneration {
			if err := resource.Remove(oldGeneration); err == nil {
				removedOld = true
			}
		}
	}
	if removedOld {
		_ = fs.directorySync(resource)
	}
	return true, nil
}

func (fs *FS) commitWhiteboardPair(ctx context.Context, resource *os.Root, source, creatorContext []byte, stored *metadata, oldArtifacts []string) (bool, error) {
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	sourceTempName, sourceTemp, err := createExclusiveTemp(resource, ".content-temp-")
	if err != nil {
		return false, storageUnavailable(err)
	}
	contextTempName := ""
	var contextTemp *os.File
	metadataTempName := ""
	publishedSource := ""
	publishedContext := ""
	sourcePublicationApplied := false
	contextPublicationApplied := false
	metadataPublished := false
	preservePublishedArtifacts := false
	defer func() {
		_ = sourceTemp.Close()
		if contextTemp != nil {
			_ = contextTemp.Close()
		}
		if sourceTempName != "" {
			_ = resource.Remove(sourceTempName)
		}
		if contextTempName != "" {
			_ = resource.Remove(contextTempName)
		}
		if metadataTempName != "" {
			_ = resource.Remove(metadataTempName)
		}
		removedPublished := false
		if !metadataPublished && !preservePublishedArtifacts && sourcePublicationApplied {
			removedPublished = resource.Remove(publishedSource) == nil || removedPublished
		}
		if !metadataPublished && !preservePublishedArtifacts && contextPublicationApplied {
			removedPublished = resource.Remove(publishedContext) == nil || removedPublished
		}
		if removedPublished {
			_ = fs.directorySync(resource)
		}
	}()
	if err := sourceTemp.Chmod(filePermissions); err != nil {
		return false, storageUnavailable(err)
	}
	if err := writeSyncedArtifact(ctx, fs.ctx, sourceTemp, source); err != nil {
		return false, err
	}

	contextTempName, contextTemp, err = createExclusiveTemp(resource, ".context-temp-")
	if err != nil {
		return false, storageUnavailable(err)
	}
	if err := contextTemp.Chmod(filePermissions); err != nil {
		return false, storageUnavailable(err)
	}
	if err := writeSyncedArtifact(ctx, fs.ctx, contextTemp, creatorContext); err != nil {
		return false, err
	}

	generation, err := randomHex(16)
	if err != nil {
		return false, storageUnavailable(err)
	}
	sourceExtension := ".md"
	if stored.Kind == string(whiteboardDomain.KindHTML) {
		sourceExtension = ".html"
	}
	publishedSource = "source-" + generation + sourceExtension
	publishedContext = "context-" + generation + ".md"
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := fs.publishArtifact(resource, sourceTempName, publishedSource); err != nil {
		switch inspectArtifactPublication(resource, sourceTempName, publishedSource) {
		case publicationApplied:
			sourcePublicationApplied = true
		case publicationUncertain:
			preservePublishedArtifacts = true
		}
		return false, storageUnavailable(err)
	}
	sourcePublicationApplied = true
	sourceTempName = ""
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := fs.publishArtifact(resource, contextTempName, publishedContext); err != nil {
		switch inspectArtifactPublication(resource, contextTempName, publishedContext) {
		case publicationApplied:
			contextPublicationApplied = true
		case publicationUncertain:
			preservePublishedArtifacts = true
		}
		return false, storageUnavailable(err)
	}
	contextPublicationApplied = true
	contextTempName = ""
	if err := fs.directorySync(resource); err != nil {
		return false, storageUnavailable(err)
	}

	stored.ContentFilename = publishedSource
	stored.ContextFilename = publishedContext
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return false, storageUnavailable(err)
	}
	metadataTempName, metadataTemp, err := createExclusiveTemp(resource, ".metadata-temp-")
	if err != nil {
		return false, storageUnavailable(err)
	}
	defer metadataTemp.Close()
	if err := metadataTemp.Chmod(filePermissions); err != nil {
		return false, storageUnavailable(err)
	}
	if err := writeSyncedArtifact(ctx, fs.ctx, metadataTemp, encoded); err != nil {
		return false, err
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return false, err
	}
	if err := fs.renameArtifact(resource, metadataTempName, metadataFilename); err != nil {
		switch fs.inspectMetadataPublication(resource, encoded) {
		case publicationApplied:
			metadataTempName = ""
			metadataPublished = true
			return true, storageUnavailable(err)
		case publicationUncertain:
			preservePublishedArtifacts = true
			return true, storageUnavailable(err)
		default:
			return false, storageUnavailable(err)
		}
	}
	metadataTempName = ""
	metadataPublished = true
	if err := fs.directorySync(resource); err != nil {
		return true, storageUnavailable(err)
	}
	if err := fs.removeOldArtifacts(resource, oldArtifacts, publishedSource, publishedContext); err != nil {
		return true, storageUnavailable(err)
	}
	return true, nil
}

func writeSyncedArtifact(request, lifecycle context.Context, file *os.File, content []byte) error {
	if err := ctxErr(request, lifecycle); err != nil {
		return err
	}
	if err := writeAll(file, content); err != nil {
		return storageUnavailable(err)
	}
	if err := ctxErr(request, lifecycle); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return storageUnavailable(err)
	}
	if err := ctxErr(request, lifecycle); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return storageUnavailable(err)
	}
	return nil
}

func (fs *FS) removeOldArtifacts(resource *os.Root, oldArtifacts []string, current ...string) error {
	currentSet := make(map[string]struct{}, len(current))
	for _, name := range current {
		currentSet[name] = struct{}{}
	}
	removed := false
	var cleanupErrors []error
	for _, name := range oldArtifacts {
		if name == "" {
			continue
		}
		if _, keep := currentSet[name]; keep {
			continue
		}
		if err := fs.removeArtifact(resource, name); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		removed = true
	}
	if removed {
		if err := fs.directorySync(resource); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func renameArtifact(root *os.Root, oldName, newName string) error {
	return root.Rename(oldName, newName)
}

func removeArtifact(root *os.Root, name string) error {
	return root.Remove(name)
}

type publicationState int

const (
	publicationNotApplied publicationState = iota
	publicationApplied
	publicationUncertain
)

func inspectArtifactPublication(root *os.Root, temp, final string) publicationState {
	finalInfo, finalErr := root.Lstat(final)
	tempInfo, tempErr := root.Lstat(temp)
	switch {
	case finalErr == nil && errors.Is(tempErr, os.ErrNotExist):
		return publicationApplied
	case finalErr == nil && tempErr == nil && os.SameFile(finalInfo, tempInfo):
		return publicationApplied
	case errors.Is(finalErr, os.ErrNotExist):
		return publicationNotApplied
	case finalErr == nil && tempErr == nil:
		return publicationNotApplied
	default:
		return publicationUncertain
	}
}

func inspectMetadataPublication(root *os.Root, expected []byte) publicationState {
	actual, err := readVerifiedFile(root, metadataFilename)
	if err == nil {
		if bytes.Equal(actual, expected) {
			return publicationApplied
		}
		return publicationNotApplied
	}
	if errors.Is(err, os.ErrNotExist) {
		return publicationNotApplied
	}
	return publicationUncertain
}

func publishGeneration(root *os.Root, temp, final string) error {
	if !validInternalFilename(temp) || !validInternalFilename(final) {
		return errors.New("invalid generation filename")
	}
	if err := root.Link(temp, final); err != nil {
		return err
	}
	if err := root.Remove(temp); err != nil {
		_ = root.Remove(final)
		return err
	}
	return nil
}

func (fs *FS) loadMetadata(ctx context.Context, resource *os.Root, expectedKind string) (metadata, error) {
	if err := ctxErr(ctx, fs.ctx); err != nil {
		return metadata{}, err
	}
	encoded, err := readVerifiedFile(resource, metadataFilename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return metadata{}, notFound()
		}
		return metadata{}, storageUnavailable(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var stored metadata
	if err := decoder.Decode(&stored); err != nil {
		return metadata{}, storageUnavailable(err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return metadata{}, storageUnavailable(err)
	}
	if err := validateMetadata(stored, expectedKind); err != nil {
		return metadata{}, storageUnavailable(err)
	}
	return stored, nil
}

func (fs *FS) openResource(namespace, id string) (*os.Root, error) {
	if _, err := fs.resourcePath(namespace, id); err != nil {
		return nil, err
	}
	category, err := fs.category(namespace)
	if err != nil {
		return nil, err
	}
	resource, err := openVerifiedNestedRoot(category, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFound()
		}
		return nil, storageUnavailable(err)
	}
	return resource, nil
}

func openVerifiedFilesystemRoot(path string, before os.FileInfo) (*os.Root, error) {
	if !realDirectory(before) {
		return nil, errors.New("configured root is not a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		_ = root.Close()
		return nil, errors.New("configured root identity changed while opening")
	}
	after, err := os.Lstat(path)
	if err != nil || !realDirectory(after) || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, errors.New("configured root identity changed after opening")
	}
	return root, nil
}

func ensureManagedRoot(parent *os.Root, name string) (*os.Root, error) {
	if !validInternalFilename(name) {
		return nil, errors.New("invalid managed directory name")
	}
	created := false
	if err := parent.Mkdir(name, directoryPermissions); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	root, err := openVerifiedNestedRoot(parent, name)
	if err != nil {
		return nil, err
	}
	if err := chmodRootDirectory(root); err != nil {
		_ = root.Close()
		return nil, err
	}
	if created {
		if err := syncDirectory(parent); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return root, nil
}

func openVerifiedNestedRoot(parent *os.Root, name string) (*os.Root, error) {
	if !validInternalFilename(name) {
		return nil, errors.New("invalid managed directory name")
	}
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !realDirectory(before) {
		return nil, errors.New("managed path is not a real directory")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		_ = root.Close()
		return nil, errors.New("managed directory identity changed while opening")
	}
	after, err := parent.Lstat(name)
	if err != nil || !realDirectory(after) || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, errors.New("managed directory identity changed after opening")
	}
	return root, nil
}

func openVerifiedRegular(root *os.Root, name string) (*os.File, error) {
	if !validInternalFilename(name) {
		return nil, errors.New("invalid internal filename")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	pathInfo, err := root.Lstat(name)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("managed file is not a real regular file")
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		_ = file.Close()
		return nil, errors.New("managed file identity changed while opening")
	}
	pathInfo, err = root.Lstat(name)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		_ = file.Close()
		return nil, errors.New("managed file identity changed after opening")
	}
	return file, nil
}

func readVerifiedFile(root *os.Root, name string) ([]byte, error) {
	file, err := openVerifiedRegular(root, name)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return content, nil
}

func createExclusiveTemp(root *os.Root, prefix string) (string, *os.File, error) {
	if !validInternalFilename(prefix + "placeholder") {
		return "", nil, errors.New("invalid temporary filename prefix")
	}
	for attempt := 0; attempt < randomNameAttempts; attempt++ {
		random, err := randomHex(16)
		if err != nil {
			return "", nil, err
		}
		name := prefix + random
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, filePermissions)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, file, nil
	}
	return "", nil, errors.New("temporary filename collision limit exceeded")
}

func chmodRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil || !opened.IsDir() {
		return errors.New("opened root is not a directory")
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, rootInfo) {
		return errors.New("opened root identity mismatch")
	}
	return directory.Chmod(directoryPermissions)
}

func syncDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func removeResourceContents(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, name := range names {
		file, err := openVerifiedRegular(root, name)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

func (fs *FS) removeOpenedResource(ctx context.Context, namespace, id string, resource *os.Root) error {
	category, err := fs.category(namespace)
	if err != nil {
		_ = resource.Close()
		return err
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		_ = resource.Close()
		return err
	}
	if err := removeResourceContents(resource); err != nil {
		_ = resource.Close()
		return storageUnavailable(err)
	}
	return fs.removeOpenedResourceDirectory(category, id, resource)
}

func (fs *FS) removeOpenedResourceDirectory(category *os.Root, id string, resource *os.Root) error {
	openedInfo, err := resource.Stat(".")
	if err != nil {
		_ = resource.Close()
		return storageUnavailable(err)
	}
	currentInfo, err := category.Lstat(id)
	if err != nil || !realDirectory(currentInfo) || !os.SameFile(openedInfo, currentInfo) {
		_ = resource.Close()
		return storageUnavailable(err)
	}
	if err := resource.Close(); err != nil {
		return storageUnavailable(err)
	}
	if err := category.Remove(id); err != nil {
		return storageUnavailable(err)
	}
	if err := fs.directorySync(category); err != nil {
		return storageUnavailable(err)
	}
	return nil
}

func (fs *FS) isExpired(stored metadata) bool {
	return common.IsExpired(fs.clock.Now(), stored.ExpiresAt.timePtr())
}

func (fs *FS) cleanupLoop() {
	defer fs.cleanup.Done()
	if fs.interval <= 0 {
		<-fs.ctx.Done()
		return
	}
	ticker := time.NewTicker(fs.interval)
	defer ticker.Stop()
	for {
		select {
		case <-fs.ctx.Done():
			return
		case <-ticker.C:
			fs.sweep(fs.ctx)
		}
	}
}

func (fs *FS) sweep(ctx context.Context) {
	for _, candidate := range []struct {
		namespace    string
		expectedKind string
	}{
		{namespace: "whiteboards", expectedKind: "whiteboard"},
		{namespace: "images", expectedKind: "image"},
	} {
		if ctx.Err() != nil {
			return
		}
		ids, err := fs.categoryIDs(candidate.namespace)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			if err := common.ValidateID(id); err != nil {
				continue
			}
			release, err := fs.locks.lock(ctx, resourceLockKey(candidate.namespace, id))
			if err != nil {
				return
			}
			_ = fs.cleanupResource(ctx, candidate.namespace, id, candidate.expectedKind)
			release()
		}
	}
}

func (fs *FS) categoryIDs(namespace string) ([]string, error) {
	category, err := fs.category(namespace)
	if err != nil {
		return nil, err
	}
	directory, err := category.Open(".")
	if err != nil {
		return nil, storageUnavailable(err)
	}
	ids, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return nil, storageUnavailable(readErr)
	}
	if closeErr != nil {
		return nil, storageUnavailable(closeErr)
	}
	return ids, nil
}

func (fs *FS) cleanupResource(ctx context.Context, namespace, id, expectedKind string) error {
	resource, err := fs.openResource(namespace, id)
	if err != nil {
		return err
	}
	stored, err := fs.loadMetadata(ctx, resource, expectedKind)
	if err != nil {
		if common.HasCode(err, common.CodeNotFound) {
			return fs.cleanupIncompleteResource(ctx, namespace, id, resource)
		}
		_ = resource.Close()
		return err
	}
	if fs.isExpired(stored) {
		return fs.removeOpenedResource(ctx, namespace, id, resource)
	}
	defer resource.Close()
	referenced := referencedArtifacts(stored)
	for _, name := range referenced {
		if err := fs.validateResourceFilename(namespace, id, name); err != nil {
			return storageUnavailable(err)
		}
		file, err := openVerifiedRegular(resource, name)
		if err != nil {
			return storageUnavailable(err)
		}
		if err := file.Close(); err != nil {
			return storageUnavailable(err)
		}
	}
	return fs.removeOrphanArtifacts(resource, namespace, referenced...)
}

func (fs *FS) cleanupIncompleteResource(ctx context.Context, namespace, id string, resource *os.Root) error {
	names, err := rootNames(resource)
	if err != nil {
		_ = resource.Close()
		return storageUnavailable(err)
	}
	for _, name := range names {
		if !cleanupArtifact(namespace, name) {
			return resource.Close()
		}
		file, err := openVerifiedRegular(resource, name)
		if err != nil {
			_ = resource.Close()
			return storageUnavailable(err)
		}
		if err := file.Close(); err != nil {
			_ = resource.Close()
			return storageUnavailable(err)
		}
	}
	if err := ctxErr(ctx, fs.ctx); err != nil {
		_ = resource.Close()
		return err
	}
	for _, name := range names {
		if err := resource.Remove(name); err != nil {
			_ = resource.Close()
			return storageUnavailable(err)
		}
	}
	if len(names) > 0 {
		if err := fs.directorySync(resource); err != nil {
			_ = resource.Close()
			return storageUnavailable(err)
		}
	}
	category, err := fs.category(namespace)
	if err != nil {
		_ = resource.Close()
		return err
	}
	return fs.removeOpenedResourceDirectory(category, id, resource)
}

func (fs *FS) removeOrphanArtifacts(resource *os.Root, namespace string, referenced ...string) error {
	names, err := rootNames(resource)
	if err != nil {
		return storageUnavailable(err)
	}
	preserved := make(map[string]struct{}, len(referenced)+1)
	preserved[metadataFilename] = struct{}{}
	for _, name := range referenced {
		preserved[name] = struct{}{}
	}
	removed := false
	for _, name := range names {
		if _, keep := preserved[name]; keep || !cleanupArtifact(namespace, name) {
			continue
		}
		file, err := openVerifiedRegular(resource, name)
		if err != nil {
			return storageUnavailable(err)
		}
		if err := file.Close(); err != nil {
			return storageUnavailable(err)
		}
		if err := resource.Remove(name); err != nil {
			return storageUnavailable(err)
		}
		removed = true
	}
	if removed {
		if err := fs.directorySync(resource); err != nil {
			return storageUnavailable(err)
		}
	}
	return nil
}

func rootNames(root *os.Root) ([]string, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return names, nil
}

func cleanupArtifact(namespace, name string) bool {
	if temporaryArtifactPattern.MatchString(name) {
		return true
	}
	if namespace == "whiteboards" {
		return whiteboardGenerationPattern.MatchString(name) || creatorContextPattern.MatchString(name)
	}
	if namespace == "images" {
		return imageGenerationPattern.MatchString(name)
	}
	return false
}

func (fs *FS) closeHandles() error {
	var closeErrors []error
	for _, name := range []string{".readiness", "images", "whiteboards"} {
		if root := fs.categories[name]; root != nil {
			if err := root.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
	}
	if fs.rootHandle != nil {
		if err := fs.rootHandle.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

func (fs *FS) beginOperation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fs.lifecycleMu.Lock()
	defer fs.lifecycleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if fs.closing {
		return storageUnavailable(nil)
	}
	if err := fs.ctx.Err(); err != nil {
		return err
	}
	fs.active.Add(1)
	return nil
}

func resourceLockKey(namespace, id string) string {
	return namespace + "/" + id
}

func (locks *lockSet) lock(ctx context.Context, key string) (func(), error) {
	return locks.acquire(ctx, key, false)
}

func (locks *lockSet) rlock(ctx context.Context, key string) (func(), error) {
	return locks.acquire(ctx, key, true)
}

func (locks *lockSet) acquire(ctx context.Context, key string, read bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	locks.mu.Lock()
	entry := locks.entries[key]
	if entry == nil {
		entry = &lockEntry{}
		locks.entries[key] = entry
	}
	entry.refs++
	locks.mu.Unlock()
	if read {
		entry.mu.RLock()
	} else {
		entry.mu.Lock()
	}
	release := func() {
		if read {
			entry.mu.RUnlock()
		} else {
			entry.mu.Unlock()
		}
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.entries, key)
		}
		locks.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (fs *FS) category(namespace string) (*os.Root, error) {
	if namespace != "whiteboards" && namespace != "images" && namespace != ".readiness" {
		return nil, storageUnavailable(errors.New("invalid namespace"))
	}
	root := fs.categories[namespace]
	if root == nil {
		return nil, storageUnavailable(errors.New("managed category unavailable"))
	}
	return root, nil
}

func (fs *FS) resourcePath(namespace, id string) (string, error) {
	if err := common.ValidateID(id); err != nil {
		return "", err
	}
	if namespace != "whiteboards" && namespace != "images" {
		return "", storageUnavailable(errors.New("invalid namespace"))
	}
	path, err := fs.containedPath(namespace, id)
	if err != nil {
		return "", storageUnavailable(err)
	}
	return path, nil
}

func (fs *FS) validateResourceFilename(namespace, id, filename string) error {
	if !validInternalFilename(filename) {
		return errors.New("invalid internal filename")
	}
	_, err := fs.containedPath(namespace, id, filename)
	return err
}

func (fs *FS) containedPath(elements ...string) (string, error) {
	target := filepath.Join(append([]string{fs.rootPath}, elements...)...)
	relative, err := filepath.Rel(fs.rootPath, target)
	if err != nil || escapesRoot(relative) {
		return "", errors.New("path escapes storage root")
	}
	return target, nil
}

func realDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func validInternalFilename(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

func validateWhiteboardRecord(record whiteboardDomain.Whiteboard) error {
	if err := common.ValidateID(record.ID); err != nil {
		return err
	}
	if record.Kind != whiteboardDomain.KindMarkdown && record.Kind != whiteboardDomain.KindHTML {
		return invalidRequest("invalid whiteboard kind")
	}
	return nil
}

func validateImageRecord(record imageDomain.Image) error {
	if err := common.ValidateID(record.ID); err != nil {
		return err
	}
	if !validImageFormat(record.Extension, record.MediaType) {
		return invalidRequest("invalid image format")
	}
	return nil
}

func validateMetadata(stored metadata, expectedKind string) error {
	if !stored.CreatedAt.valid() || !stored.UpdatedAt.valid() || (stored.ExpiresAt != nil && !stored.ExpiresAt.valid()) {
		return errors.New("invalid metadata timestamp")
	}
	switch expectedKind {
	case "whiteboard":
		if stored.Extension != "" || stored.MediaType != "" {
			return errors.New("invalid whiteboard metadata")
		}
		switch stored.SchemaVersion {
		case legacyMetadataSchemaVersion:
			if stored.ContextFilename != "" || stored.Kind != string(whiteboardDomain.KindMarkdown) {
				return errors.New("invalid legacy whiteboard metadata")
			}
			if !whiteboardGenerationPattern.MatchString(stored.ContentFilename) || !strings.HasSuffix(stored.ContentFilename, ".md") {
				return errors.New("invalid legacy whiteboard generation")
			}
		case pairedMetadataSchemaVersion:
			if !matchingWhiteboardGeneration(stored.Kind, stored.ContentFilename, stored.ContextFilename) {
				return errors.New("invalid paired whiteboard metadata")
			}
		default:
			return errors.New("invalid whiteboard metadata schema")
		}
	case "image":
		if stored.SchemaVersion != legacyMetadataSchemaVersion || stored.ContextFilename != "" || stored.Kind != "image" || !imageGenerationPattern.MatchString(stored.ContentFilename) || !validImageFormat(stored.Extension, stored.MediaType) {
			return errors.New("invalid image metadata")
		}
	default:
		return errors.New("invalid metadata kind")
	}
	return nil
}

func matchingWhiteboardGeneration(kind, sourceName, contextName string) bool {
	source := pairedSourcePattern.FindStringSubmatch(sourceName)
	creatorContext := creatorContextPattern.FindStringSubmatch(contextName)
	if len(source) != 3 || len(creatorContext) != 2 || source[1] != creatorContext[1] {
		return false
	}
	return kind == string(whiteboardDomain.KindMarkdown) && source[2] == "md" || kind == string(whiteboardDomain.KindHTML) && source[2] == "html"
}

func referencedArtifacts(stored metadata) []string {
	if stored.ContextFilename == "" {
		return []string{stored.ContentFilename}
	}
	return []string{stored.ContentFilename, stored.ContextFilename}
}

func whiteboardMetadata(record whiteboardDomain.Whiteboard) metadata {
	return metadata{
		SchemaVersion: pairedMetadataSchemaVersion, Kind: string(record.Kind),
		CreatedAt: fromTime(record.CreatedAt), UpdatedAt: fromTime(record.UpdatedAt), ExpiresAt: fromTimePtr(record.ExpiresAt),
	}
}

func imageMetadata(record imageDomain.Image) metadata {
	return metadata{
		SchemaVersion: legacyMetadataSchemaVersion, Kind: "image",
		CreatedAt: fromTime(record.CreatedAt), UpdatedAt: fromTime(record.UpdatedAt), ExpiresAt: fromTimePtr(record.ExpiresAt),
		Extension: record.Extension, MediaType: record.MediaType,
	}
}

func generationFilename(kind string) (string, error) {
	generation, err := randomHex(16)
	if err != nil {
		return "", err
	}
	switch kind {
	case string(whiteboardDomain.KindMarkdown):
		return "source-" + generation + ".md", nil
	case string(whiteboardDomain.KindHTML):
		return "source-" + generation + ".html", nil
	case "image":
		return "content-" + generation, nil
	default:
		return "", errors.New("invalid resource kind")
	}
}

func randomHex(bytesCount int) (string, error) {
	random := make([]byte, bytesCount)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func validImageFormat(extension, mediaType string) bool {
	formats := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}
	return formats[extension] == mediaType && mediaType != ""
}

func fromTime(value time.Time) storedTime {
	return storedTime{Seconds: value.Unix(), Nanoseconds: int32(value.Nanosecond())}
}

func fromTimePtr(value *time.Time) *storedTime {
	if value == nil {
		return nil
	}
	result := fromTime(*value)
	return &result
}

func (value storedTime) valid() bool {
	return value.Nanoseconds >= 0 && value.Nanoseconds < int32(time.Second)
}

func (value storedTime) time() time.Time {
	return time.Unix(value.Seconds, int64(value.Nanoseconds)).UTC()
}

func (value *storedTime) timePtr() *time.Time {
	if value == nil {
		return nil
	}
	result := value.time()
	return &result
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple metadata values")
		}
		return err
	}
	return nil
}

func ctxErr(request, lifecycle context.Context) error {
	if err := request.Err(); err != nil {
		return err
	}
	return lifecycle.Err()
}

func writeAll(file *os.File, content []byte) error {
	for len(content) > 0 {
		written, err := file.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func escapesRoot(relative string) bool {
	return relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func invalidRequest(message string) error {
	return common.NewError(common.CodeInvalidRequest, message, nil)
}

func notFound() error {
	return common.NewError(common.CodeNotFound, "resource not found", nil)
}

func storageUnavailable(cause error) error {
	return common.NewError(common.CodeStorageUnavailable, "storage unavailable", cause)
}
