package agentwb_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/pkg/agentwb"
	"github.com/stretchr/testify/require"
)

type memoryWhiteboards struct{}

func (*memoryWhiteboards) Create(context.Context, agentwb.Whiteboard) error { return nil }
func (*memoryWhiteboards) Get(context.Context, string) (agentwb.Whiteboard, error) {
	return agentwb.Whiteboard{}, nil
}
func (*memoryWhiteboards) Replace(context.Context, agentwb.Whiteboard) error { return nil }
func (*memoryWhiteboards) Delete(context.Context, string) error              { return nil }
func (*memoryWhiteboards) Ready(context.Context) error                       { return nil }
func (*memoryWhiteboards) Close() error                                      { return nil }

type memoryImages struct{}

func (*memoryImages) Create(context.Context, agentwb.Image) error { return nil }
func (*memoryImages) Get(context.Context, string) (agentwb.Image, error) {
	return agentwb.Image{}, nil
}
func (*memoryImages) Replace(context.Context, agentwb.Image) error { return nil }
func (*memoryImages) Delete(context.Context, string) error         { return nil }
func (*memoryImages) Ready(context.Context) error                  { return nil }
func (*memoryImages) Close() error                                 { return nil }

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type fixedIDs struct{ id string }

func (ids fixedIDs) NewID() (string, error) { return ids.id, nil }

var (
	_ agentwb.WhiteboardStore = (*memoryWhiteboards)(nil)
	_ agentwb.ImageStore      = (*memoryImages)(nil)
	_ agentwb.Clock           = fixedClock{}
	_ agentwb.IDGenerator     = fixedIDs{}
)

func externalHandler(service *agentwb.Service) http.Handler { return service.Handler() }
func externalReady(ctx context.Context, service *agentwb.Service) error {
	return service.Ready(ctx)
}

func TestExternalConsumerCanUseCompleteFacade(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	service, err := agentwb.New(agentwb.Config{
		WhiteboardStore:          &memoryWhiteboards{},
		ImageStore:               &memoryImages{},
		DefaultExpirationSeconds: 1,
		CleanupInterval:          time.Minute,
		Host:                     "127.0.0.1",
		Port:                     1,
		ShutdownTimeout:          time.Second,
		MaxWhiteboardBytes:       1,
		MaxContextBytes:          1,
		MaxImageBytes:            1,
		MaxImageRequestBytes:     1,
		ViewerLocalAgentEnabled:  true,
		LogMode:                  agentwb.LogModeConsole,
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
	},
		agentwb.WithPort(0),
		agentwb.WithDefaultExpiration(0),
		agentwb.WithClock(fixedClock{now: time.Unix(1, 0)}),
		agentwb.WithIDGenerator(fixedIDs{id: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}),
		agentwb.WithListener(listener),
		agentwb.WithViewerAssets([]byte("body{}"), []byte("void 0")),
	)
	require.NoError(t, err)
	require.NotNil(t, externalHandler(service))

	ctx := context.Background()
	require.NoError(t, externalReady(ctx, service))
	_, _ = service.CreateMarkdown(ctx, agentwb.CreateWhiteboardInput{
		Source: []byte("# markdown"), Context: []byte("creator context"),
	})
	_, _ = service.CreateHTML(ctx, agentwb.CreateWhiteboardInput{
		Source:  []byte("<!doctype html><html><head></head><body></body></html>"),
		Context: []byte("creator context"),
	})
	_, _ = service.GetWhiteboard(ctx, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_, _ = service.UpdateWhiteboard(ctx, agentwb.UpdateWhiteboardInput{
		ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Kind: agentwb.KindMarkdown,
		Source: []byte("# updated"), Context: []byte("updated context"),
	})
	_ = service.DeleteWhiteboard(ctx, agentwb.KindMarkdown, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_, _ = service.CreateImages(ctx, agentwb.CreateImagesInput{})
	_, _ = service.GetImage(ctx, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_, _ = service.UpdateImage(ctx, agentwb.UpdateImageInput{})
	_ = service.DeleteImage(ctx, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_ = service.Shutdown(ctx)
	_ = service.Close()

	var lifecycle interface {
		Ready(context.Context) error
		ListenAndServe(context.Context) error
		Serve(context.Context, net.Listener) error
		Shutdown(context.Context) error
		Close() error
	} = service
	require.NotNil(t, lifecycle)
}

func TestPublicWhiteboardTypesExposeCreatorContext(t *testing.T) {
	contextBytes := []byte("exact creator context")
	require.Equal(t, contextBytes, agentwb.Whiteboard{Context: contextBytes}.Context)
	require.Equal(t, contextBytes, agentwb.CreateWhiteboardInput{Context: contextBytes}.Context)
	require.Equal(t, contextBytes, agentwb.UpdateWhiteboardInput{Context: contextBytes}.Context)
}

func TestPublicNamedConstantsAreAvailable(t *testing.T) {
	require.Equal(t, agentwb.WhiteboardKind("markdown"), agentwb.KindMarkdown)
	require.Equal(t, agentwb.WhiteboardKind("html"), agentwb.KindHTML)
	require.Equal(t, agentwb.ErrorCode("invalid_request"), agentwb.CodeInvalidRequest)
	require.Equal(t, agentwb.ErrorCode("not_found"), agentwb.CodeNotFound)
	require.Equal(t, agentwb.ErrorCode("content_too_large"), agentwb.CodeContentTooLarge)
	require.Equal(t, agentwb.ErrorCode("unsupported_media_type"), agentwb.CodeUnsupportedMediaType)
	require.Equal(t, agentwb.ErrorCode("storage_unavailable"), agentwb.CodeStorageUnavailable)
	require.Equal(t, agentwb.ErrorCode("internal_error"), agentwb.CodeInternal)
	require.True(t, errors.Is(agentwb.ErrIDCollision, agentwb.ErrIDCollision))
}
