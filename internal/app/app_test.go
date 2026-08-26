package app_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/app"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/dndplsidc/agent-whiteboard/internal/image"
	"github.com/dndplsidc/agent-whiteboard/internal/testutil"

	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
	"github.com/dndplsidc/agent-whiteboard/internal/whiteboard"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type appContextKey struct{}

func TestAppRoutesCoexistOnOneHandler(t *testing.T) {
	application := newApp(t)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "whiteboard", method: http.MethodPost, path: httpx.APIWhiteboardMarkdown, wantStatus: http.StatusBadRequest},
		{name: "image", method: http.MethodPost, path: httpx.APIImages, wantStatus: http.StatusBadRequest},
		{name: "health", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusOK},
		{name: "readiness", method: http.MethodGet, path: "/readyz", wantStatus: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveRequest(application.Handler(), httptest.NewRequest(test.method, test.path, nil))

			require.Equal(t, test.wantStatus, response.Code)
		})
	}
}

func TestAppHealthStaysLiveWhileNotReady(t *testing.T) {
	dependency := testutil.NewMockReadiness(t)
	application := newApp(t, dependency)

	response := serveRequest(application.Handler(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "{\"status\":\"ok\"}\n", response.Body.String())
	dependency.AssertNotCalled(t, "Ready", mock.Anything)
}

func TestAppReadinessFollowsAcceptingState(t *testing.T) {
	dependency := testutil.NewMockReadiness(t)
	application := newApp(t, dependency)

	beforeStartup := serveRequest(application.Handler(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, beforeStartup.Code)
	dependency.AssertNotCalled(t, "Ready", mock.Anything)

	dependency.EXPECT().Ready(mock.Anything).Return(nil).Once()
	application.SetReady(true)
	afterStartup := serveRequest(application.Handler(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, afterStartup.Code)

	application.SetReady(false)
	afterShutdown := serveRequest(application.Handler(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, afterShutdown.Code)
}

func TestAppReadinessPassesExactContextInDependencyOrder(t *testing.T) {
	ctx := context.WithValue(context.Background(), appContextKey{}, "sentinel")
	first := testutil.NewMockReadiness(t)
	second := testutil.NewMockReadiness(t)
	order := make([]string, 0, 2)
	exactContext := mock.MatchedBy(func(got context.Context) bool {
		return got == ctx && got.Value(appContextKey{}) == "sentinel"
	})
	first.EXPECT().Ready(exactContext).Run(func(context.Context) {
		order = append(order, "first")
	}).Return(nil).Once()
	second.EXPECT().Ready(exactContext).Run(func(context.Context) {
		order = append(order, "second")
	}).Return(nil).Once()
	application := newApp(t, first, second)
	application.SetReady(true)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx)

	response := serveRequest(application.Handler(), request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []string{"first", "second"}, order)
}

func TestAppReadinessShortCircuitsAndHidesDependencyErrors(t *testing.T) {
	first := testutil.NewMockReadiness(t)
	second := testutil.NewMockReadiness(t)
	first.EXPECT().Ready(mock.Anything).Return(errors.New("database password leaked")).Once()
	application := newApp(t, first, second)
	application.SetReady(true)

	response := serveRequest(application.Handler(), httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Equal(t, "{\"status\":\"unavailable\"}\n", response.Body.String())
	require.NotContains(t, response.Body.String(), "database")
	require.NotContains(t, response.Body.String(), "password")
	second.AssertNotCalled(t, "Ready", mock.Anything)
}

func TestAppConstructorRejectsNilDependencies(t *testing.T) {
	whiteboards := newWhiteboardHandler(t)
	images := newImageHandler(t)
	var typedNilWhiteboards *whiteboard.Handler
	var typedNilImages *image.Handler
	var typedNilReadiness *testutil.MockReadiness

	tests := []struct {
		name   string
		config app.Config
	}{
		{name: "nil whiteboard handler", config: app.Config{Images: images}},
		{name: "typed nil whiteboard handler", config: app.Config{Whiteboards: typedNilWhiteboards, Images: images}},
		{name: "nil image handler", config: app.Config{Whiteboards: whiteboards}},
		{name: "typed nil image handler", config: app.Config{Whiteboards: whiteboards, Images: typedNilImages}},
		{name: "nil readiness entry", config: app.Config{Whiteboards: whiteboards, Images: images, Readiness: []app.Readiness{nil}}},
		{name: "typed nil readiness entry", config: app.Config{Whiteboards: whiteboards, Images: images, Readiness: []app.Readiness{typedNilReadiness}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application, err := app.New(test.config)

			require.Nil(t, application)
			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
		})
	}
}

func newApp(t *testing.T, readiness ...app.Readiness) *app.App {
	t.Helper()

	application, err := app.New(app.Config{
		Whiteboards: newWhiteboardHandler(t),
		Images:      newImageHandler(t),
		Readiness:   readiness,
	})
	require.NoError(t, err)
	return application
}

func newWhiteboardHandler(t *testing.T) *whiteboard.Handler {
	t.Helper()

	viewer, err := whiteboard.NewViewer(whiteboard.ViewerConfig{CSS: []byte("body{}"), JS: []byte("void 0")})
	require.NoError(t, err)
	handler, err := whiteboard.NewHandler(testutil.NewMockWhiteboardOperations(t), viewer, whiteboard.HandlerConfig{})
	require.NoError(t, err)
	return handler
}

func newImageHandler(t *testing.T) *image.Handler {
	t.Helper()

	handler, err := image.NewHandler(testutil.NewMockImageOperations(t), image.HandlerConfig{})
	require.NoError(t, err)
	return handler
}

func serveRequest(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
