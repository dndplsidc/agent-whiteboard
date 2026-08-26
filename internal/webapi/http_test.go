package webapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
	"github.com/stretchr/testify/require"
)

type readinessFunc func(context.Context) error

func (ready readinessFunc) Ready(ctx context.Context) error {
	return ready(ctx)
}

type pointerReadiness struct{}

func (*pointerReadiness) Ready(context.Context) error {
	return nil
}

func TestRegisterHealthServesLiveAndReadyResponses(t *testing.T) {
	t.Parallel()

	requestContext := context.WithValue(context.Background(), struct{}{}, "request context")
	var receivedContext context.Context
	mux := http.NewServeMux()
	httpx.RegisterHealth(mux, readinessFunc(func(ctx context.Context) error {
		receivedContext = ctx
		return nil
	}))

	health := serveRequest(mux, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	readyRequest := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(requestContext)
	ready := serveRequest(mux, readyRequest)

	require.Equal(t, http.StatusOK, health.Code)
	require.Equal(t, "application/json", health.Header().Get("Content-Type"))
	require.Equal(t, "no-store", health.Header().Get("Cache-Control"))
	require.Equal(t, "{\"status\":\"ok\"}\n", health.Body.String())
	require.Equal(t, http.StatusOK, ready.Code)
	require.Equal(t, "application/json", ready.Header().Get("Content-Type"))
	require.Equal(t, "no-store", ready.Header().Get("Cache-Control"))
	require.Equal(t, "{\"status\":\"ready\"}\n", ready.Body.String())
	require.Same(t, requestContext, receivedContext)
}

func TestRegisterHealthReturnsUnavailableWithoutExposingReadinessError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	httpx.RegisterHealth(mux, readinessFunc(func(context.Context) error {
		return errors.New("database password leaked")
	}))

	ready := serveRequest(mux, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	health := serveRequest(mux, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusServiceUnavailable, ready.Code)
	require.Equal(t, "application/json", ready.Header().Get("Content-Type"))
	require.Equal(t, "no-store", ready.Header().Get("Cache-Control"))
	require.Equal(t, "{\"status\":\"unavailable\"}\n", ready.Body.String())
	require.NotContains(t, ready.Body.String(), "database")
	require.Equal(t, http.StatusOK, health.Code)
}

func TestRegisterHealthTreatsNilReadinessAsUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		readiness httpx.Readiness
	}{
		{name: "nil"},
		{name: "typed nil", readiness: (*pointerReadiness)(nil)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			httpx.RegisterHealth(mux, test.readiness)

			ready := serveRequest(mux, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			health := serveRequest(mux, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			require.Equal(t, http.StatusServiceUnavailable, ready.Code)
			require.Equal(t, "{\"status\":\"unavailable\"}\n", ready.Body.String())
			require.Equal(t, http.StatusOK, health.Code)
		})
	}
}

func TestRegisterHealthUsesExactGetPatterns(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	httpx.RegisterHealth(mux, readinessFunc(func(context.Context) error { return nil }))

	require.Equal(t, http.StatusMethodNotAllowed, serveRequest(mux, httptest.NewRequest(http.MethodPost, "/healthz", nil)).Code)
	require.Equal(t, http.StatusMethodNotAllowed, serveRequest(mux, httptest.NewRequest(http.MethodPost, "/readyz", nil)).Code)
	require.Equal(t, http.StatusNotFound, serveRequest(mux, httptest.NewRequest(http.MethodGet, "/healthz/child", nil)).Code)
	require.Equal(t, http.StatusNotFound, serveRequest(mux, httptest.NewRequest(http.MethodGet, "/readyz/child", nil)).Code)
}

func TestRegisterHealthRejectsHeadWithoutCheckingReadiness(t *testing.T) {
	t.Parallel()

	readinessCalls := 0
	mux := http.NewServeMux()
	httpx.RegisterHealth(mux, readinessFunc(func(context.Context) error {
		readinessCalls++
		return nil
	}))

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			response := serveRequest(mux, httptest.NewRequest(http.MethodHead, path, nil))

			require.Equal(t, http.StatusMethodNotAllowed, response.Code)
			require.Empty(t, response.Body.String())
			require.NotContains(t, response.Body.String(), "status")
		})
	}
	require.Zero(t, readinessCalls)
}

func serveRequest(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestRouteConstants(t *testing.T) {
	require.Equal(t, "/api/v1/whiteboards/markdown", httpx.APIWhiteboardMarkdown)
	require.Equal(t, "/api/v1/whiteboards/markdown/", httpx.APIWhiteboardMarkdownResource)
	require.Equal(t, "/api/v1/whiteboards/html", httpx.APIWhiteboardHTML)
	require.Equal(t, "/api/v1/whiteboards/html/", httpx.APIWhiteboardHTMLResource)
	require.Equal(t, "/api/v1/images", httpx.APIImages)
	require.Equal(t, "/whiteboards/markdown/", httpx.PublicMarkdown)
	require.Equal(t, "/whiteboards/html/", httpx.PublicHTML)
	require.Equal(t, "/content", httpx.PublicHTMLContentSuffix)
	require.Equal(t, "/images/", httpx.PublicImages)
}

func TestWriteJSONUsesExactWireShapeAndTrailingNewline(t *testing.T) {
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	expiresAt := int64(1784289600)
	rr := httptest.NewRecorder()

	httpx.WriteJSON(rr, http.StatusCreated, httpx.ResourceResponse{Resource: httpx.Resource{
		ID:        "resource-id",
		Type:      "markdown",
		Path:      "/whiteboards/markdown/resource-id",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
		ExpiresAt: &expiresAt,
		Permanent: false,
	}})

	require.Equal(t, http.StatusCreated, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	require.Equal(t, "{\"resource\":{\"id\":\"resource-id\",\"type\":\"markdown\",\"path\":\"/whiteboards/markdown/resource-id\",\"created_at\":\"2026-07-16T12:00:00Z\",\"updated_at\":\"2026-07-16T12:00:00Z\",\"expires_at\":1784289600,\"permanent\":false}}\n", rr.Body.String())
}

func TestMarkdownResponseUsesExactPublicWireShape(t *testing.T) {
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	rr := httptest.NewRecorder()

	httpx.WriteJSON(rr, http.StatusOK, httpx.MarkdownResponse{
		Resource: httpx.Resource{
			ID:        "resource-id",
			Type:      "markdown",
			Path:      "/whiteboards/markdown/resource-id",
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
			Permanent: true,
		},
		Markdown: "# Exact markdown\n",
		Context:  "## Exact context\n",
	})

	require.Equal(t, "{\"resource\":{\"id\":\"resource-id\",\"type\":\"markdown\",\"path\":\"/whiteboards/markdown/resource-id\",\"created_at\":\"2026-07-16T12:00:00Z\",\"updated_at\":\"2026-07-16T12:00:00Z\",\"expires_at\":null,\"permanent\":true},\"markdown\":\"# Exact markdown\\n\",\"context\":\"## Exact context\\n\"}\n", rr.Body.String())
	require.NotContains(t, rr.Body.String(), "source_path")
	require.NotContains(t, rr.Body.String(), "context_path")
	require.NotContains(t, rr.Body.String(), "generation")
}

func TestHTMLResponseUsesExactPublicWireShape(t *testing.T) {
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	rr := httptest.NewRecorder()

	httpx.WriteJSON(rr, http.StatusOK, httpx.HTMLResponse{
		Resource: httpx.Resource{
			ID:        "resource-id",
			Type:      "html",
			Path:      "/whiteboards/html/resource-id",
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
			Permanent: true,
		},
		HTML:    "<!doctype html><html></html>",
		Context: "## Exact context\n",
	})

	require.Equal(t, "{\"resource\":{\"id\":\"resource-id\",\"type\":\"html\",\"path\":\"/whiteboards/html/resource-id\",\"created_at\":\"2026-07-16T12:00:00Z\",\"updated_at\":\"2026-07-16T12:00:00Z\",\"expires_at\":null,\"permanent\":true},\"html\":\"\\u003c!doctype html\\u003e\\u003chtml\\u003e\\u003c/html\\u003e\",\"context\":\"## Exact context\\n\"}\n", rr.Body.String())
	require.NotContains(t, rr.Body.String(), "source_path")
	require.NotContains(t, rr.Body.String(), "context_path")
	require.NotContains(t, rr.Body.String(), "generation")
}

func TestWriteError(t *testing.T) {
	tests := []struct {
		name       string
		code       common.ErrorCode
		statusCode int
	}{
		{name: "invalid request", code: common.CodeInvalidRequest, statusCode: http.StatusBadRequest},
		{name: "not found", code: common.CodeNotFound, statusCode: http.StatusNotFound},
		{name: "content too large", code: common.CodeContentTooLarge, statusCode: http.StatusRequestEntityTooLarge},
		{name: "unsupported media type", code: common.CodeUnsupportedMediaType, statusCode: http.StatusUnsupportedMediaType},
		{name: "storage unavailable", code: common.CodeStorageUnavailable, statusCode: http.StatusServiceUnavailable},
		{name: "internal error", code: common.CodeInternal, statusCode: http.StatusInternalServerError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			err := fmt.Errorf("operation failed: %w", common.NewError(test.code, test.name, errors.New("secret path")))

			httpx.WriteError(rr, err)

			require.Equal(t, test.statusCode, rr.Code)
			require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
			require.Equal(t, fmt.Sprintf("{\"error\":{\"code\":%q,\"message\":%q}}\n", test.code, test.name), rr.Body.String())
			require.NotContains(t, rr.Body.String(), "secret path")
			require.NotContains(t, rr.Body.String(), "operation failed")
		})
	}
}

func TestWriteErrorHidesUnknownErrors(t *testing.T) {
	rr := httptest.NewRecorder()

	httpx.WriteError(rr, errors.New("database password leaked"))

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	require.Equal(t, "{\"error\":{\"code\":\"internal_error\",\"message\":\"internal error\"}}\n", rr.Body.String())
	require.NotContains(t, rr.Body.String(), "database password")
}

func TestWriteErrorHidesInternalDomainMessage(t *testing.T) {
	rr := httptest.NewRecorder()

	httpx.WriteError(rr, common.NewError(common.CodeInternal, "database password leaked", errors.New("secret path")))

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	require.Equal(t, "{\"error\":{\"code\":\"internal_error\",\"message\":\"internal error\"}}\n", rr.Body.String())
	require.NotContains(t, rr.Body.String(), "database password")
	require.NotContains(t, rr.Body.String(), "secret path")
}

func TestWriteErrorHidesUnrecognizedDomainError(t *testing.T) {
	rr := httptest.NewRecorder()

	httpx.WriteError(rr, common.NewError(common.ErrorCode("future_error"), "private implementation detail", errors.New("secret cause")))

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	require.Equal(t, "{\"error\":{\"code\":\"internal_error\",\"message\":\"internal error\"}}\n", rr.Body.String())
	require.NotContains(t, rr.Body.String(), "private implementation detail")
	require.NotContains(t, rr.Body.String(), "secret cause")
}

func TestSetPublicHeaders(t *testing.T) {
	tests := []struct {
		name   string
		image  bool
		robots string
	}{
		{name: "whiteboard", robots: "noindex, nofollow, noarchive"},
		{name: "image", image: true, robots: "noindex, nofollow, noarchive, noimageindex"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rr := httptest.NewRecorder()

			httpx.SetPublicHeaders(rr, test.image)

			require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
			require.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
			require.Equal(t, test.robots, rr.Header().Get("X-Robots-Tag"))
		})
	}
}

func TestMultipartRequestLimitIncludesFixedOverheadAndRejectsOverflow(t *testing.T) {
	limit, err := httpx.MultipartRequestLimit(7, 11)
	require.NoError(t, err)
	require.Equal(t, int64(7+11)+httpx.MultipartOverheadBytes, limit)

	_, err = httpx.MultipartRequestLimit(1, -1)
	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)

	_, err = httpx.MultipartRequestLimit(int64(^uint64(0)>>1), 1)
	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
}

func TestReadMultipartFieldsAppliesIndependentLimitsInEitherOrder(t *testing.T) {
	for _, values := range [][]multipartValue{
		{
			{fieldName: "file", filename: "board.md", content: "source"},
			{fieldName: "context", filename: "context.md", content: "creator context"},
		},
		{
			{fieldName: "context", filename: "context.md", content: "creator context"},
			{fieldName: "file", filename: "board.md", content: "source"},
		},
	} {
		body, contentType := multipartBody(t, values)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)

		form, err := httpx.ReadMultipartFields(httptest.NewRecorder(), req, int64(len(body)),
			httpx.MultipartFileLimit{FieldName: "file", MaxBytes: 6},
			httpx.MultipartFileLimit{FieldName: "context", MaxBytes: 15},
		)

		require.NoError(t, err)
		require.Len(t, form.Files, 2)
		contents := make(map[string]string, 2)
		for _, file := range form.Files {
			contents[file.FieldName] = string(file.Content)
		}
		require.Equal(t, map[string]string{"file": "source", "context": "creator context"}, contents)
	}
}

func TestReadMultipartFieldsRejectsEachIndependentOverflow(t *testing.T) {
	tests := []struct {
		name         string
		fileLimit    int64
		contextLimit int64
	}{
		{name: "file", fileLimit: 5, contextLimit: 15},
		{name: "context", fileLimit: 6, contextLimit: 14},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := multipartBody(t, []multipartValue{
				{fieldName: "file", filename: "board.md", content: "source"},
				{fieldName: "context", filename: "context.md", content: "creator context"},
			})
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			req.Header.Set("Content-Type", contentType)

			_, err := httpx.ReadMultipartFields(httptest.NewRecorder(), req, int64(len(body)),
				httpx.MultipartFileLimit{FieldName: "file", MaxBytes: tt.fileLimit},
				httpx.MultipartFileLimit{FieldName: "context", MaxBytes: tt.contextLimit},
			)

			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeContentTooLarge), "expected content_too_large, got %v", err)
		})
	}
}

func TestReadMultipartRejectsAggregateOverflow(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "file", filename: "board.md", content: "content"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	_, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)-1), 1024, "file")

	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeContentTooLarge), "expected content_too_large, got %v", err)
}

func TestReadMultipartAcceptsAggregateAtLimit(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "file", filename: "board.md", content: "content"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	form, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 7, "file")

	require.NoError(t, err)
	require.Len(t, form.Files, 1)
	require.Equal(t, []byte("content"), form.Files[0].Content)
}

func TestReadMultipartRejectsChunkedOverflowAfterTerminalBoundary(t *testing.T) {
	prefix, contentType := multipartBody(t, []multipartValue{
		{fieldName: "file", filename: "board.md", content: "content"},
	})
	body := append(append([]byte(nil), prefix...), bytes.Repeat([]byte("epilogue"), 8)...)
	req := httptest.NewRequest(http.MethodPost, "/", &chunkedReader{content: body, chunkSize: 1})
	req.Header.Set("Content-Type", contentType)
	require.Equal(t, int64(-1), req.ContentLength)

	_, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(prefix)), 1024, "file")

	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeContentTooLarge), "expected content_too_large, got %v", err)
}

func TestReadPartRejectsPerPartOverflow(t *testing.T) {
	part := firstMultipartPart(t, "images", "image.png", "four")

	_, err := httpx.ReadPart(part, 3)

	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeContentTooLarge), "expected content_too_large, got %v", err)
}

func TestReadPartAcceptsPartAtLimit(t *testing.T) {
	part := firstMultipartPart(t, "images", "image.png", "four")

	content, err := httpx.ReadPart(part, 4)

	require.NoError(t, err)
	require.Equal(t, []byte("four"), content)
}

func TestReadMultipartRejectsMalformedBoundary(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("--different\r\ncontent"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=expected")

	_, err := httpx.ReadMultipart(httptest.NewRecorder(), req, 1024, 1024, "file")

	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
}

func TestReadMultipartRejectsInvalidSignedExpiration(t *testing.T) {
	for _, value := range []string{"", "--1", "1.5", "9223372036854775808"} {
		t.Run(fmt.Sprintf("value_%q", value), func(t *testing.T) {
			body, contentType := multipartBody(t, []multipartValue{
				{fieldName: "file", filename: "board.md", content: "content"},
				{fieldName: "expires_in_seconds", content: value},
			})
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			req.Header.Set("Content-Type", contentType)

			_, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1024, "file")

			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
		})
	}
}

func TestReadMultipartParsesSignedExpiration(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "file", filename: "board.md", content: "content"},
		{fieldName: "expires_in_seconds", content: "-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	form, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1024, "file")

	require.NoError(t, err)
	require.Equal(t, int64(-1), *form.ExpiresInSeconds)
}

func TestReadMultipartParsesSignedExpirationBoundsIndependentlyOfFileLimit(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int64
	}{
		{name: "maximum", value: "9223372036854775807", want: int64(9223372036854775807)},
		{name: "minimum", value: "-9223372036854775808", want: int64(-9223372036854775807 - 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := multipartBody(t, []multipartValue{
				{fieldName: "file", filename: "image.png", content: "x"},
				{fieldName: "expires_in_seconds", content: tt.value},
			})
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			req.Header.Set("Content-Type", contentType)

			form, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1, "file")

			require.NoError(t, err)
			require.Len(t, form.Files, 1)
			require.Equal(t, []byte("x"), form.Files[0].Content)
			require.Equal(t, tt.want, *form.ExpiresInSeconds)
		})
	}
}

func TestReadMultipartRejectsOversizedExpirationAsInvalidRequest(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "file", filename: "image.png", content: "x"},
		{fieldName: "expires_in_seconds", content: "-92233720368547758080"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	_, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1, "file")

	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
}

func TestReadMultipartLeavesOmittedExpirationNil(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "file", filename: "board.md", content: "content"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	form, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1024, "file")

	require.NoError(t, err)
	require.Nil(t, form.ExpiresInSeconds)
}

func TestReadMultipartRejectsDuplicateExpiration(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "expires_in_seconds", content: "1"},
		{fieldName: "expires_in_seconds", content: "2"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	_, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1024, "file")

	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
}

func TestReadMultipartPreservesRepeatedImageOrder(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "images", filename: "first.png", content: "first"},
		{fieldName: "images", filename: "second.png", content: "second"},
		{fieldName: "images", filename: "third.png", content: "third"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	form, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1024, "images")

	require.NoError(t, err)
	require.Equal(t, []string{"first.png", "second.png", "third.png"}, []string{
		form.Files[0].Filename,
		form.Files[1].Filename,
		form.Files[2].Filename,
	})
	require.Equal(t, []string{"first", "second", "third"}, []string{
		string(form.Files[0].Content),
		string(form.Files[1].Content),
		string(form.Files[2].Content),
	})
}

func TestReadMultipartRejectsUnknownFileField(t *testing.T) {
	body, contentType := multipartBody(t, []multipartValue{
		{fieldName: "thumbnail", filename: "image.png", content: "content"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	_, err := httpx.ReadMultipart(httptest.NewRecorder(), req, int64(len(body)), 1024, "images")

	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
}

type multipartValue struct {
	fieldName string
	filename  string
	content   string
}

type chunkedReader struct {
	content   []byte
	chunkSize int
}

func (r *chunkedReader) Read(destination []byte) (int, error) {
	if len(r.content) == 0 {
		return 0, io.EOF
	}
	read := min(len(destination), r.chunkSize, len(r.content))
	copy(destination, r.content[:read])
	r.content = r.content[read:]
	return read, nil
}

func multipartBody(t *testing.T, values []multipartValue) ([]byte, string) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, value := range values {
		var (
			partWriter io.Writer
			err        error
		)
		if value.filename == "" {
			partWriter, err = writer.CreateFormField(value.fieldName)
		} else {
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, value.fieldName, value.filename))
			header.Set("Content-Type", "application/octet-stream")
			partWriter, err = writer.CreatePart(header)
		}
		require.NoError(t, err)
		_, err = partWriter.Write([]byte(value.content))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return body.Bytes(), writer.FormDataContentType()
}

func firstMultipartPart(t *testing.T, fieldName, filename, content string) *multipart.Part {
	t.Helper()

	body, contentType := multipartBody(t, []multipartValue{{
		fieldName: fieldName,
		filename:  filename,
		content:   content,
	}})
	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	part, err := reader.NextPart()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, part.Close()) })
	return part
}
