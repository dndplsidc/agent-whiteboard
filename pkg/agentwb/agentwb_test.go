package agentwb

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const facadeTestID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type facadeContextKey struct{}

type testClock struct{ now time.Time }

func (clock testClock) Now() time.Time { return clock.now }

type testIDs struct{ id string }

func (ids testIDs) NewID() (string, error) { return ids.id, nil }

type recordingWhiteboardStore struct {
	ctx       context.Context
	created   Whiteboard
	replaced  Whiteboard
	gotID     string
	deletedID string
	get       Whiteboard
	close     int
}

func (store *recordingWhiteboardStore) Create(ctx context.Context, value Whiteboard) error {
	store.ctx, store.created = ctx, value
	return nil
}
func (store *recordingWhiteboardStore) Get(ctx context.Context, id string) (Whiteboard, error) {
	store.ctx, store.gotID = ctx, id
	return store.get, nil
}
func (store *recordingWhiteboardStore) Replace(ctx context.Context, value Whiteboard) error {
	store.ctx, store.replaced = ctx, value
	return nil
}
func (store *recordingWhiteboardStore) Delete(ctx context.Context, id string) error {
	store.ctx, store.deletedID = ctx, id
	return nil
}
func (*recordingWhiteboardStore) Ready(context.Context) error { return nil }
func (store *recordingWhiteboardStore) Close() error {
	store.close++
	return nil
}

type recordingImageStore struct {
	ctx       context.Context
	created   Image
	replaced  Image
	gotID     string
	deletedID string
	get       Image
	close     int
}

func (store *recordingImageStore) Create(ctx context.Context, value Image) error {
	store.ctx, store.created = ctx, value
	return nil
}
func (store *recordingImageStore) Get(ctx context.Context, id string) (Image, error) {
	store.ctx, store.gotID = ctx, id
	return store.get, nil
}
func (store *recordingImageStore) Replace(ctx context.Context, value Image) error {
	store.ctx, store.replaced = ctx, value
	return nil
}
func (store *recordingImageStore) Delete(ctx context.Context, id string) error {
	store.ctx, store.deletedID = ctx, id
	return nil
}
func (*recordingImageStore) Ready(context.Context) error { return nil }
func (store *recordingImageStore) Close() error {
	store.close++
	return nil
}

func TestNewRejectsInvalidConfigurationAndTypedNilDependencies(t *testing.T) {
	var nilWhiteboards *recordingWhiteboardStore
	var nilImages *recordingImageStore
	var nilClock *testClock
	var nilIDs *testIDs
	var nilListener *net.TCPListener

	tests := []struct {
		name    string
		config  Config
		options []Option
	}{
		{name: "typed nil whiteboard store", config: Config{WhiteboardStore: nilWhiteboards}},
		{name: "typed nil image store", config: Config{ImageStore: nilImages}},
		{name: "negative expiration", config: Config{DefaultExpirationSeconds: -1}},
		{name: "negative cleanup", config: Config{CleanupInterval: -time.Second}},
		{name: "negative port", config: Config{Port: -1}},
		{name: "large port", config: Config{Port: 65536}},
		{name: "negative shutdown", config: Config{ShutdownTimeout: -time.Second}},
		{name: "negative whiteboard limit", config: Config{MaxWhiteboardBytes: -1}},
		{name: "negative image limit", config: Config{MaxImageBytes: -1}},
		{name: "negative request limit", config: Config{MaxImageRequestBytes: -1}},
		{name: "request below image limit", config: Config{MaxImageBytes: 2, MaxImageRequestBytes: 1}},
		{name: "invalid log mode", config: Config{LogMode: "xml"}},
		{name: "nil option", options: []Option{nil}},
		{name: "typed nil clock", options: []Option{WithClock(nilClock)}},
		{name: "typed nil ids", options: []Option{WithIDGenerator(nilIDs)}},
		{name: "typed nil listener", options: []Option{WithListener(nilListener)}},
		{name: "empty viewer css", options: []Option{WithViewerAssets(nil, []byte("js"))}},
		{name: "empty viewer js", options: []Option{WithViewerAssets([]byte("css"), nil)}},
		{name: "negative option expiration", options: []Option{WithDefaultExpiration(-1)}},
		{name: "negative option port", options: []Option{WithPort(-1)}},
		{name: "large option port", options: []Option{WithPort(65536)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := New(test.config, test.options...)
			require.Nil(t, service)
			require.Error(t, err)
		})
	}
}

func TestNewPreflightsServerConfigurationBeforeTouchingStorage(t *testing.T) {
	var typedNilListener *net.TCPListener
	tests := []struct {
		name    string
		config  Config
		options []Option
	}{
		{name: "host whitespace", config: Config{Host: "bad host"}},
		{name: "host with port", config: Config{Host: "127.0.0.1:8567"}},
		{name: "host with brackets", config: Config{Host: "[::1]"}},
		{name: "host empty label", config: Config{Host: "bad..host"}},
		{name: "typed nil listener", options: []Option{WithListener(typedNilListener)}},
		{name: "listener nil address", options: []Option{WithListener(nilAddressListener{})}},
	}

	for _, test := range tests {
		t.Run(test.name+" absent root", func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "must-not-exist")
			test.config.RootDir = root

			service, err := New(test.config, test.options...)

			require.Nil(t, service)
			require.Error(t, err)
			require.True(t, HasErrorCode(err, CodeInvalidRequest), "expected invalid_request, got %v", err)
			_, statErr := os.Stat(root)
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})

		t.Run(test.name+" existing root", func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "existing")
			require.NoError(t, os.Mkdir(root, 0o755))
			beforeInfo, statErr := os.Stat(root)
			require.NoError(t, statErr)
			beforeMode := beforeInfo.Mode().Perm()
			test.config.RootDir = root

			service, err := New(test.config, test.options...)

			require.Nil(t, service)
			require.Error(t, err)
			require.True(t, HasErrorCode(err, CodeInvalidRequest), "expected invalid_request, got %v", err)
			info, statErr := os.Stat(root)
			require.NoError(t, statErr)
			require.Equal(t, beforeMode, info.Mode().Perm())
			require.NoDirExists(t, filepath.Join(root, "whiteboards"))
			require.NoDirExists(t, filepath.Join(root, "images"))
		})
	}
}

func TestServiceForwardsExactContextsAndValues(t *testing.T) {
	now := time.Unix(10, 20).UTC()
	whiteboards := &recordingWhiteboardStore{}
	images := &recordingImageStore{}
	service := newFacadeForStores(t, whiteboards, images, now)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	ctx := context.WithValue(context.Background(), facadeContextKey{}, "sentinel")
	expires := int64(0)

	createdMarkdown, err := service.CreateMarkdown(ctx, CreateWhiteboardInput{Source: []byte("# hello"), Context: []byte("creator context"), ExpiresInSeconds: &expires})
	require.NoError(t, err)
	require.Same(t, ctx, whiteboards.ctx)
	require.Equal(t, KindMarkdown, whiteboards.created.Kind)
	require.Equal(t, []byte("# hello"), whiteboards.created.Source)
	require.Equal(t, []byte("creator context"), whiteboards.created.Context)
	require.Equal(t, facadeTestID, createdMarkdown.ID)

	whiteboards.get = whiteboards.created
	gotWhiteboard, err := service.GetWhiteboard(ctx, facadeTestID)
	require.NoError(t, err)
	require.Same(t, ctx, whiteboards.ctx)
	require.Equal(t, whiteboards.created, gotWhiteboard)

	whiteboards.get = whiteboards.created
	updatedWhiteboard, err := service.UpdateWhiteboard(ctx, UpdateWhiteboardInput{
		ID: facadeTestID, Kind: KindMarkdown, Source: []byte("# updated"), Context: []byte("updated context"), ExpiresInSeconds: &expires,
	})
	require.NoError(t, err)
	require.Same(t, ctx, whiteboards.ctx)
	require.Equal(t, []byte("# updated"), whiteboards.replaced.Source)
	require.Equal(t, []byte("updated context"), whiteboards.replaced.Context)
	require.Equal(t, facadeTestID, updatedWhiteboard.ID)

	whiteboards.get = whiteboards.replaced
	require.NoError(t, service.DeleteWhiteboard(ctx, KindMarkdown, facadeTestID))
	require.Same(t, ctx, whiteboards.ctx)
	require.Equal(t, facadeTestID, whiteboards.deletedID)

	png := validPNG(t)
	createdImages, err := service.CreateImages(ctx, CreateImagesInput{Images: []ImageUpload{{Content: png, ExpiresInSeconds: &expires}}})
	require.NoError(t, err)
	require.Same(t, ctx, images.ctx)
	require.Equal(t, png, images.created.Content)
	require.Len(t, createdImages, 1)

	images.get = images.created
	gotImage, err := service.GetImage(ctx, facadeTestID)
	require.NoError(t, err)
	require.Same(t, ctx, images.ctx)
	require.Equal(t, images.created, gotImage)

	images.get = images.created
	updatedImage, err := service.UpdateImage(ctx, UpdateImageInput{ID: facadeTestID, Content: png, ExpiresInSeconds: &expires})
	require.NoError(t, err)
	require.Same(t, ctx, images.ctx)
	require.Equal(t, png, images.replaced.Content)
	require.Equal(t, facadeTestID, updatedImage.ID)

	images.get = images.replaced
	require.NoError(t, service.DeleteImage(ctx, facadeTestID))
	require.Same(t, ctx, images.ctx)
	require.Equal(t, facadeTestID, images.deletedID)
}

func TestNewInjectsEachCustomStoreOnlyIntoItsDomain(t *testing.T) {
	whiteboards := &recordingWhiteboardStore{}
	service, err := New(Config{WhiteboardStore: whiteboards, RootDir: t.TempDir()},
		WithClock(testClock{now: time.Unix(10, 0)}),
		WithIDGenerator(testIDs{id: facadeTestID}),
		WithViewerAssets([]byte("body{}"), []byte("void 0")),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	_, err = service.CreateMarkdown(context.Background(), CreateWhiteboardInput{Source: []byte("# custom"), Context: []byte("creator context")})
	require.NoError(t, err)
	require.Equal(t, facadeTestID, whiteboards.created.ID)
	_, err = service.CreateImages(context.Background(), CreateImagesInput{Images: []ImageUpload{{Content: validPNG(t)}}})
	require.NoError(t, err)
}

func TestDefaultFilesystemCompositionPersistsBothDomainsAndHandlerRoutes(t *testing.T) {
	service, err := New(Config{RootDir: t.TempDir()},
		WithClock(testClock{now: time.Unix(10, 0)}),
		WithIDGenerator(&sequenceIDs{ids: []string{facadeTestID, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	_, err = service.CreateMarkdown(context.Background(), CreateWhiteboardInput{Source: []byte("# stored"), Context: []byte("creator context")})
	require.NoError(t, err)
	stored, err := service.GetWhiteboard(context.Background(), facadeTestID)
	require.NoError(t, err)
	require.Equal(t, []byte("# stored"), stored.Source)
	require.Equal(t, []byte("creator context"), stored.Context)
	_, err = service.CreateImages(context.Background(), CreateImagesInput{Images: []ImageUpload{{Content: validPNG(t)}}})
	require.NoError(t, err)
	_, err = service.GetImage(context.Background(), "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	require.NoError(t, err)

	for _, path := range []string{"/healthz", "/api/v1/whiteboards/markdown", "/api/v1/images"} {
		method := http.MethodPost
		if path == "/healthz" {
			method = http.MethodGet
		}
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(method, path, nil))
		require.NotEqual(t, http.StatusNotFound, response.Code, path)
	}
}

func TestViewerAssetsAreCopiedAtOptionBoundary(t *testing.T) {
	css := []byte("copied-css")
	js := []byte("copied-js")
	option := WithViewerAssets(css, js)
	css[0] = 'X'
	js[0] = 'X'
	whiteboards := &recordingWhiteboardStore{}
	service, err := New(Config{WhiteboardStore: whiteboards, ImageStore: &recordingImageStore{}},
		WithClock(testClock{now: time.Unix(10, 0)}), WithIDGenerator(testIDs{id: facadeTestID}), option)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	whiteboards.get = Whiteboard{ID: facadeTestID, Kind: KindMarkdown, Source: []byte("# viewer")}

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/whiteboards/markdown/"+facadeTestID, nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), "copied-css")
	require.Contains(t, response.Body.String(), "copied-js")
	require.NotContains(t, response.Body.String(), "Xopied")
}

func TestCloseIsIdempotentAndClosesEachCustomViewOnce(t *testing.T) {
	whiteboards := &recordingWhiteboardStore{}
	images := &recordingImageStore{}
	service := newFacadeForStores(t, whiteboards, images, time.Unix(10, 0))

	require.NoError(t, service.Close())
	require.NoError(t, service.Close())
	require.Equal(t, 1, whiteboards.close)
	require.Equal(t, 1, images.close)
}

func TestCloseIsSafeWhenCustomViewsShareAnIdempotentOwner(t *testing.T) {
	owner := &sharedCloseOwner{}
	whiteboards := &sharedWhiteboardView{recordingWhiteboardStore: &recordingWhiteboardStore{}, owner: owner}
	images := &sharedImageView{recordingImageStore: &recordingImageStore{}, owner: owner}
	service := newFacadeForStores(t, whiteboards, images, time.Unix(10, 0))

	require.NoError(t, service.Close())
	require.NoError(t, service.Close())
	require.Equal(t, 1, owner.calls)
}

func TestLifecycleMethodsDelegateToOneServer(t *testing.T) {
	service := newFacadeForStores(t, &recordingWhiteboardStore{}, &recordingImageStore{}, time.Unix(10, 0))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Serve(ctx, listener) }()
	waitForFacadeReady(t, service.Handler())

	cancel()
	require.NoError(t, <-done)
	require.NoError(t, service.Close())
	require.Error(t, service.ListenAndServe(context.Background()))
	require.Error(t, service.Serve(context.Background(), listener))
}

func TestListenAndServeUsesConfiguredListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	service, err := New(Config{
		WhiteboardStore: &recordingWhiteboardStore{},
		ImageStore:      &recordingImageStore{},
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	},
		WithClock(testClock{now: time.Unix(10, 0)}),
		WithIDGenerator(testIDs{id: facadeTestID}),
		WithListener(listener),
		WithViewerAssets([]byte("body{}"), []byte("void 0")),
	)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.ListenAndServe(ctx) }()
	waitForFacadeReady(t, service.Handler())

	cancel()
	require.NoError(t, <-done)
	require.NoError(t, service.Close())
}

func newFacadeForStores(t *testing.T, whiteboards WhiteboardStore, images ImageStore, now time.Time) *Service {
	t.Helper()
	service, err := New(Config{
		WhiteboardStore: whiteboards,
		ImageStore:      images,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	},
		WithClock(testClock{now: now}),
		WithIDGenerator(testIDs{id: facadeTestID}),
		WithViewerAssets([]byte("body{}"), []byte("void 0")),
	)
	require.NoError(t, err)
	return service
}

type sequenceIDs struct {
	mu  sync.Mutex
	ids []string
}

type sharedCloseOwner struct {
	once  sync.Once
	calls int
}

func (owner *sharedCloseOwner) Close() error {
	owner.once.Do(func() { owner.calls++ })
	return nil
}

type sharedWhiteboardView struct {
	*recordingWhiteboardStore
	owner *sharedCloseOwner
}

func (view *sharedWhiteboardView) Close() error { return view.owner.Close() }

type sharedImageView struct {
	*recordingImageStore
	owner *sharedCloseOwner
}

func (view *sharedImageView) Close() error { return view.owner.Close() }

type nilAddressListener struct{}

func (nilAddressListener) Accept() (net.Conn, error) { return nil, errors.New("not used") }
func (nilAddressListener) Close() error              { return nil }
func (nilAddressListener) Addr() net.Addr            { return nil }

func (ids *sequenceIDs) NewID() (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if len(ids.ids) == 0 {
		return "", errors.New("no ids")
	}
	id := ids.ids[0]
	ids.ids = ids.ids[1:]
	return id, nil
}

func validPNG(t *testing.T) []byte {
	t.Helper()
	// One transparent 1x1 PNG.
	return []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 31, 21, 196, 137, 0, 0, 0, 13, 73, 68, 65, 84, 8, 215, 99, 96, 0, 0, 0, 2, 0, 1, 226, 33, 188, 51, 0, 0, 0, 0, 73, 69, 78, 68, 174, 66, 96, 130}
}

func waitForFacadeReady(t *testing.T, handler http.Handler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code == http.StatusOK {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("facade server did not become ready")
}
