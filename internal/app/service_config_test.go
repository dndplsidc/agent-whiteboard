package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	httpx "github.com/edocsss/agent-whiteboard/internal/http"
	"github.com/edocsss/agent-whiteboard/internal/image"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
	"github.com/stretchr/testify/require"
)

func TestResolveServiceConfigUsesExactDefaults(t *testing.T) {
	resolved, err := resolveServiceConfig(ServiceConfig{}, nil)
	require.NoError(t, err)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".agent-whiteboard"), resolved.rootDir)
	require.Equal(t, int64(86400), resolved.defaultExpiration)
	require.Equal(t, 15*time.Minute, resolved.cleanupInterval)
	require.Equal(t, "127.0.0.1", resolved.host)
	require.Equal(t, 8567, resolved.port)
	require.Equal(t, 10*time.Second, resolved.shutdownTimeout)
	require.Equal(t, int64(10<<20), resolved.maxWhiteboardBytes)
	require.Equal(t, int64(1<<20), resolved.maxContextBytes)
	require.Equal(t, int64(25<<20), resolved.maxImageBytes)
	require.Equal(t, int64(100<<20), resolved.maxImageRequestBytes)
	require.False(t, resolved.viewerLocalAgentEnabled)
	require.Equal(t, LogModeConsole, resolved.logMode)
	require.IsType(t, &slog.TextHandler{}, resolved.logger.Handler())
}

func TestResolveServiceConfigHonorsExplicitZerosAndJSONLogging(t *testing.T) {
	resolved, err := resolveServiceConfig(ServiceConfig{LogMode: LogModeJSON, MaxContextBytes: 2, ViewerLocalAgentEnabled: true}, []Option{
		WithPort(0),
		WithDefaultExpiration(0),
	})
	require.NoError(t, err)
	require.Zero(t, resolved.port)
	require.Zero(t, resolved.defaultExpiration)
	require.EqualValues(t, 2, resolved.maxContextBytes)
	require.True(t, resolved.viewerLocalAgentEnabled)
	require.IsType(t, &slog.JSONHandler{}, resolved.logger.Handler())
}

func TestNewServiceWiresResolvedViewerLocalAgentFlag(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			whiteboards := &serviceConfigWhiteboardStore{board: whiteboard.Whiteboard{
				ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Kind: whiteboard.KindMarkdown,
				Source: []byte("# source"), Context: []byte("creator context"),
			}}
			service, err := NewService(ServiceConfig{
				WhiteboardStore: whiteboards, ImageStore: &serviceConfigImageStore{},
				ViewerLocalAgentEnabled: enabled,
			}, WithViewerAssets([]byte("body{}"), []byte("void 0")))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, service.Close()) })

			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
				httpx.PublicMarkdown+whiteboards.board.ID, nil))

			require.Equal(t, http.StatusOK, response.Code)
			if enabled {
				require.Contains(t, response.Body.String(), `"context":"creator context"`)
				require.Contains(t, response.Body.String(), `"local_agent":{"enabled":true,`)
				require.Contains(t, response.Header().Get("Content-Security-Policy"), "connect-src 'self' http://127.0.0.1:* ws://127.0.0.1:*")
			} else {
				require.NotContains(t, response.Body.String(), `"context"`)
				require.NotContains(t, response.Body.String(), `"local_agent"`)
				require.Contains(t, response.Header().Get("Content-Security-Policy"), "connect-src 'none'")
			}
		})
	}
}

func TestNewServicePassesIndependentWhiteboardAndContextLimitsToHTTPHandler(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		context    string
		wantStatus int
	}{
		{name: "at both limits", source: "abc", context: "12345", wantStatus: http.StatusCreated},
		{name: "source over limit", source: "abcd", context: "12345", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "context over limit", source: "abc", context: "123456", wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, err := NewService(ServiceConfig{
				WhiteboardStore:    &serviceConfigWhiteboardStore{},
				ImageStore:         &serviceConfigImageStore{},
				MaxWhiteboardBytes: 3,
				MaxContextBytes:    5,
			}, WithViewerAssets([]byte("body{}"), []byte("void 0")))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, service.Close()) })

			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			file, err := writer.CreateFormFile("file", "board.md")
			require.NoError(t, err)
			_, err = file.Write([]byte(tt.source))
			require.NoError(t, err)
			creatorContext, err := writer.CreateFormFile("context", "context.md")
			require.NoError(t, err)
			_, err = creatorContext.Write([]byte(tt.context))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			request := httptest.NewRequest(http.MethodPost, httpx.APIWhiteboardMarkdown, &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			response := httptest.NewRecorder()

			service.Handler().ServeHTTP(response, request)

			require.Equal(t, tt.wantStatus, response.Code)
		})
	}
}

func TestNewServiceResolvesHomeOnlyWhenFilesystemStorageIsNeeded(t *testing.T) {
	originalUserHomeDir := userHomeDir
	homeErr := errors.New("home unavailable")
	homeCalls := 0
	userHomeDir = func() (string, error) {
		homeCalls++
		return "", homeErr
	}
	t.Cleanup(func() { userHomeDir = originalUserHomeDir })

	whiteboards := &serviceConfigWhiteboardStore{}
	images := &serviceConfigImageStore{}
	service, err := NewService(ServiceConfig{WhiteboardStore: whiteboards, ImageStore: images})
	require.NoError(t, err)
	require.NotNil(t, service)
	require.Zero(t, homeCalls)
	require.NoError(t, service.Close())

	customWhiteboards := &serviceConfigWhiteboardStore{}
	service, err = NewService(ServiceConfig{WhiteboardStore: customWhiteboards})
	require.Nil(t, service)
	require.ErrorIs(t, err, homeErr)
	require.Equal(t, 1, homeCalls)
	require.Zero(t, customWhiteboards.closeCalls)
}

type serviceConfigWhiteboardStore struct {
	closeCalls int
	board      whiteboard.Whiteboard
}

func (*serviceConfigWhiteboardStore) Create(context.Context, whiteboard.Whiteboard) error { return nil }
func (store *serviceConfigWhiteboardStore) Get(context.Context, string) (whiteboard.Whiteboard, error) {
	return store.board, nil
}
func (*serviceConfigWhiteboardStore) Replace(context.Context, whiteboard.Whiteboard) error {
	return nil
}
func (*serviceConfigWhiteboardStore) Delete(context.Context, string) error { return nil }
func (*serviceConfigWhiteboardStore) Ready(context.Context) error          { return nil }
func (store *serviceConfigWhiteboardStore) Close() error {
	store.closeCalls++
	return nil
}

type serviceConfigImageStore struct{ closeCalls int }

func (*serviceConfigImageStore) Create(context.Context, image.Image) error { return nil }
func (*serviceConfigImageStore) Get(context.Context, string) (image.Image, error) {
	return image.Image{}, nil
}
func (*serviceConfigImageStore) Replace(context.Context, image.Image) error { return nil }
func (*serviceConfigImageStore) Delete(context.Context, string) error       { return nil }
func (*serviceConfigImageStore) Ready(context.Context) error                { return nil }
func (store *serviceConfigImageStore) Close() error {
	store.closeCalls++
	return nil
}
