package whiteboard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/testutil"
	httpx "github.com/edocsss/agent-whiteboard/internal/webapi"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testWhiteboardID       = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	defaultMaxBytes        = int64(1 << 20)
	defaultMaxContextBytes = int64(1 << 19)
)

type handlerContextKey struct{}

type whiteboardRoute struct {
	name       string
	kind       whiteboard.Kind
	apiPath    string
	publicPath string
}

var whiteboardRoutes = []whiteboardRoute{
	{name: "markdown", kind: whiteboard.KindMarkdown, apiPath: httpx.APIWhiteboardMarkdown, publicPath: httpx.PublicMarkdown},
	{name: "html", kind: whiteboard.KindHTML, apiPath: httpx.APIWhiteboardHTML, publicPath: httpx.PublicHTML},
}

func TestHandlerConstructorRejectsInvalidDependenciesAndLimits(t *testing.T) {
	viewer := newViewer(t)
	var typedNil *testutil.MockWhiteboardOperations

	tests := []struct {
		name            string
		operations      whiteboard.Operations
		viewer          *whiteboard.Viewer
		maxBytes        int64
		maxContextBytes int64
	}{
		{name: "nil operations", viewer: viewer},
		{name: "typed nil operations", operations: typedNil, viewer: viewer},
		{name: "nil viewer", operations: testutil.NewMockWhiteboardOperations(t)},
		{name: "negative max bytes", operations: testutil.NewMockWhiteboardOperations(t), viewer: viewer, maxBytes: -1},
		{name: "negative max context bytes", operations: testutil.NewMockWhiteboardOperations(t), viewer: viewer, maxContextBytes: -1},
		{name: "aggregate limit overflow", operations: testutil.NewMockWhiteboardOperations(t), viewer: viewer, maxBytes: int64(^uint64(0) >> 1), maxContextBytes: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := whiteboard.NewHandler(tt.operations, tt.viewer, whiteboard.HandlerConfig{
				MaxWhiteboardBytes: tt.maxBytes,
				MaxContextBytes:    tt.maxContextBytes,
			})

			require.Nil(t, handler)
			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
		})
	}
}

func TestHandlerConstructorAcceptsZeroLimit(t *testing.T) {
	handler, err := whiteboard.NewHandler(testutil.NewMockWhiteboardOperations(t), newViewer(t), whiteboard.HandlerConfig{})

	require.NoError(t, err)
	require.NotNil(t, handler)
}

func TestHandlerCreateReturnsResourceAndPassesExactContext(t *testing.T) {
	createdAt := time.Date(2026, time.July, 17, 3, 4, 5, 0, time.UTC)
	expiresAt := createdAt.Add(5 * time.Minute)
	expiresIn := int64(300)

	for _, route := range whiteboardRoutes {
		t.Run(route.name, func(t *testing.T) {
			operations := testutil.NewMockWhiteboardOperations(t)
			ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
			result := whiteboard.Result{
				ID:        testWhiteboardID,
				Kind:      route.kind,
				CreatedAt: createdAt,
				UpdatedAt: createdAt,
				ExpiresAt: &expiresAt,
			}
			expectedContext := mock.MatchedBy(func(got context.Context) bool {
				return got == ctx && got.Value(handlerContextKey{}) == "sentinel"
			})
			expectedInput := mock.MatchedBy(func(got whiteboard.CreateInput) bool {
				wantContext := []byte(nil)
				if route.kind == whiteboard.KindMarkdown {
					wantContext = []byte("creator context")
				}
				return bytes.Equal(got.Source, []byte("source body")) && bytes.Equal(got.Context, wantContext) &&
					got.ExpiresInSeconds != nil && *got.ExpiresInSeconds == expiresIn
			})
			if route.kind == whiteboard.KindMarkdown {
				operations.EXPECT().CreateMarkdown(expectedContext, expectedInput).Return(result, nil).Once()
			} else {
				operations.EXPECT().CreateHTML(expectedContext, expectedInput).Return(result, nil).Once()
			}
			handler := newHandler(t, operations, defaultMaxBytes)
			fields := []multipartField{{name: "file", filename: "board.txt", value: "source body"}}
			if route.kind == whiteboard.KindMarkdown {
				fields = append(fields, multipartField{name: "context", filename: "context.md", value: "creator context"})
			}
			fields = append(fields, multipartField{name: "expires_in_seconds", value: fmt.Sprint(expiresIn)})
			body, contentType := multipartRequestBody(t, fields...)
			req := httptest.NewRequest(http.MethodPost, route.apiPath, bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", contentType)
			rr := httptest.NewRecorder()

			handlerMux(t, handler).ServeHTTP(rr, req)

			require.Equal(t, http.StatusCreated, rr.Code)
			require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
			resource := decodeResource(t, rr)
			require.Equal(t, httpx.Resource{
				ID:        testWhiteboardID,
				Type:      string(route.kind),
				Path:      route.publicPath + testWhiteboardID,
				CreatedAt: createdAt,
				UpdatedAt: createdAt,
				ExpiresAt: int64Pointer(expiresAt.Unix()),
				Permanent: false,
			}, resource)
		})
	}
}

func TestHandlerCreateReturnsCapabilityWithUncertainStorageError(t *testing.T) {
	createdAt := time.Date(2026, time.July, 17, 3, 4, 5, 0, time.UTC)
	operations := testutil.NewMockWhiteboardOperations(t)
	result := whiteboard.Result{
		ID: testWhiteboardID, Kind: whiteboard.KindMarkdown, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	operations.EXPECT().CreateMarkdown(mock.Anything, mock.Anything).Return(
		result,
		common.NewError(common.CodeStorageUnavailable, "storage unavailable", errors.New("rollback durability uncertain")),
	).Once()
	handler := newHandler(t, operations, defaultMaxBytes)
	body, contentType := multipartRequestBody(t,
		multipartField{name: "file", filename: "board.md", value: "# source"},
		multipartField{name: "context", filename: "context.md", value: "creator context"},
	)
	req := httptest.NewRequest(http.MethodPost, httpx.APIWhiteboardMarkdown, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	handlerMux(t, handler).ServeHTTP(rr, req)

	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
	response := decodeErrorBody(t, rr)
	require.Equal(t, httpx.ErrorBody{Code: common.CodeStorageUnavailable, Message: "storage unavailable"}, response.Error)
	require.NotNil(t, response.Resource)
	require.Equal(t, testWhiteboardID, response.Resource.ID)
	require.Equal(t, httpx.PublicMarkdown+testWhiteboardID, response.Resource.Path)
	require.NotContains(t, rr.Body.String(), "rollback durability uncertain")
}

func TestHandlerUpdateReturnsResourceAndPassesExactContext(t *testing.T) {
	createdAt := time.Date(2026, time.July, 16, 3, 4, 5, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)

	for _, route := range whiteboardRoutes {
		t.Run(route.name, func(t *testing.T) {
			operations := testutil.NewMockWhiteboardOperations(t)
			ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
			operations.EXPECT().Update(
				mock.MatchedBy(func(got context.Context) bool {
					return got == ctx && got.Value(handlerContextKey{}) == "sentinel"
				}),
				mock.MatchedBy(func(got whiteboard.UpdateInput) bool {
					wantContext := []byte(nil)
					if route.kind == whiteboard.KindMarkdown {
						wantContext = []byte("replacement context")
					}
					return got.ID == testWhiteboardID && got.Kind == route.kind &&
						bytes.Equal(got.Source, []byte("replacement")) && bytes.Equal(got.Context, wantContext) &&
						got.ExpiresInSeconds == nil
				}),
			).Return(whiteboard.Result{
				ID:        testWhiteboardID,
				Kind:      route.kind,
				CreatedAt: createdAt,
				UpdatedAt: updatedAt,
			}, nil).Once()
			handler := newHandler(t, operations, defaultMaxBytes)
			fields := []multipartField{{name: "file", filename: "board.txt", value: "replacement"}}
			if route.kind == whiteboard.KindMarkdown {
				fields = append([]multipartField{{name: "context", filename: "context.md", value: "replacement context"}}, fields...)
			}
			body, contentType := multipartRequestBody(t, fields...)
			req := httptest.NewRequest(http.MethodPut, route.apiPath+"/"+testWhiteboardID, bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", contentType)
			rr := httptest.NewRecorder()

			handlerMux(t, handler).ServeHTTP(rr, req)

			require.Equal(t, http.StatusOK, rr.Code)
			resource := decodeResource(t, rr)
			require.Equal(t, httpx.Resource{
				ID:        testWhiteboardID,
				Type:      string(route.kind),
				Path:      route.publicPath + testWhiteboardID,
				CreatedAt: createdAt,
				UpdatedAt: updatedAt,
				Permanent: true,
			}, resource)
		})
	}
}

func TestHandlerDeleteReturnsNoContentAndPassesExactContext(t *testing.T) {
	for _, route := range whiteboardRoutes {
		t.Run(route.name, func(t *testing.T) {
			operations := testutil.NewMockWhiteboardOperations(t)
			ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
			operations.EXPECT().Delete(
				mock.MatchedBy(func(got context.Context) bool {
					return got == ctx && got.Value(handlerContextKey{}) == "sentinel"
				}),
				route.kind,
				testWhiteboardID,
			).Return(nil).Once()
			handler := newHandler(t, operations, defaultMaxBytes)
			req := httptest.NewRequest(http.MethodDelete, route.apiPath+"/"+testWhiteboardID, nil).WithContext(ctx)
			rr := httptest.NewRecorder()

			handlerMux(t, handler).ServeHTTP(rr, req)

			require.Equal(t, http.StatusNoContent, rr.Code)
			require.Empty(t, rr.Body.String())
		})
	}
}

func TestHandlerViewMarkdownRendersShellWithExactContextAndPublicHeaders(t *testing.T) {
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockWhiteboardOperations(t)
	operations.EXPECT().Get(
		mock.MatchedBy(func(got context.Context) bool {
			return got == ctx && got.Value(handlerContextKey{}) == "sentinel"
		}),
		testWhiteboardID,
	).Return(whiteboard.Whiteboard{
		ID:     testWhiteboardID,
		Kind:   whiteboard.KindMarkdown,
		Source: []byte("# Public whiteboard"),
	}, nil).Once()
	req := httptest.NewRequest(http.MethodGet, httpx.PublicMarkdown+testWhiteboardID, nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	handlerMux(t, newHandler(t, operations, defaultMaxBytes)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "text/html; charset=utf-8", rr.Header().Get("Content-Type"))
	require.Contains(t, rr.Body.String(), testViewerCSS)
	require.Contains(t, rr.Body.String(), testViewerJS)
	require.Contains(t, rr.Body.String(), `{"markdown":"# Public whiteboard"}`)
	require.NotContains(t, rr.Body.String(), `"context"`)
	require.NotContains(t, rr.Body.String(), `"local_agent"`)
	assertMarkdownHeaders(t, rr, newViewer(t).ContentSecurityPolicy())
}

func TestHandlerGetMarkdownReturnsExactPublicResourceMarkdownAndContext(t *testing.T) {
	createdAt := time.Date(2026, time.July, 17, 3, 4, 5, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	expiresAt := updatedAt.Add(time.Hour)
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockWhiteboardOperations(t)
	operations.EXPECT().Get(
		mock.MatchedBy(func(got context.Context) bool { return got == ctx }),
		testWhiteboardID,
	).Return(whiteboard.Whiteboard{
		ID:        testWhiteboardID,
		Kind:      whiteboard.KindMarkdown,
		Source:    []byte("# Exact markdown\n"),
		Context:   []byte("## Exact creator context\n"),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		ExpiresAt: &expiresAt,
	}, nil).Once()
	req := httptest.NewRequest(http.MethodGet, httpx.APIWhiteboardMarkdownResource+testWhiteboardID, nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	handlerMux(t, newHandler(t, operations, defaultMaxBytes)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
	require.Equal(t, fmt.Sprintf("{\"resource\":{\"id\":%q,\"type\":\"markdown\",\"path\":%q,\"created_at\":\"2026-07-17T03:04:05Z\",\"updated_at\":\"2026-07-17T04:04:05Z\",\"expires_at\":1784264645,\"permanent\":false},\"markdown\":\"# Exact markdown\\n\",\"context\":\"## Exact creator context\\n\"}\n", testWhiteboardID, httpx.PublicMarkdown+testWhiteboardID), rr.Body.String())
	for _, privateName := range []string{"source_path", "context_path", "schema", "generation"} {
		require.NotContains(t, rr.Body.String(), privateName)
	}
}

func TestHandlerGetMarkdownReturnsEmptyContextForLegacyResource(t *testing.T) {
	operations := testutil.NewMockWhiteboardOperations(t)
	operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(whiteboard.Whiteboard{
		ID:        testWhiteboardID,
		Kind:      whiteboard.KindMarkdown,
		Source:    []byte("legacy markdown"),
		Context:   nil,
		CreatedAt: time.Date(2026, time.July, 17, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.July, 17, 3, 4, 5, 0, time.UTC),
	}, nil).Once()
	rr := httptest.NewRecorder()

	handlerMux(t, newHandler(t, operations, defaultMaxBytes)).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, httpx.APIWhiteboardMarkdownResource+testWhiteboardID, nil))

	require.Equal(t, http.StatusOK, rr.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &response))
	require.Equal(t, "legacy markdown", response["markdown"])
	require.Equal(t, "", response["context"])
	require.Len(t, response, 3)
}

func TestHandlerGetMarkdownHidesMalformedMissingExpiredAndWrongKindAsSameNotFound(t *testing.T) {
	wantBody := "{\"error\":{\"code\":\"not_found\",\"message\":\"resource not found\"}}\n"
	tests := []struct {
		name       string
		id         string
		operations func(*testing.T) *testutil.MockWhiteboardOperations
	}{
		{
			name: "malformed", id: "malformed",
			operations: func(t *testing.T) *testutil.MockWhiteboardOperations { return testutil.NewMockWhiteboardOperations(t) },
		},
		{
			name: "missing", id: testWhiteboardID,
			operations: func(t *testing.T) *testutil.MockWhiteboardOperations {
				operations := testutil.NewMockWhiteboardOperations(t)
				operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(whiteboard.Whiteboard{},
					common.NewError(common.CodeNotFound, "resource not found", errors.New("private missing path"))).Once()
				return operations
			},
		},
		{
			name: "expired", id: testWhiteboardID,
			operations: func(t *testing.T) *testutil.MockWhiteboardOperations {
				operations := testutil.NewMockWhiteboardOperations(t)
				operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(whiteboard.Whiteboard{},
					common.NewError(common.CodeNotFound, "resource not found", errors.New("private expired generation"))).Once()
				return operations
			},
		},
		{
			name: "wrong kind", id: testWhiteboardID,
			operations: func(t *testing.T) *testutil.MockWhiteboardOperations {
				operations := testutil.NewMockWhiteboardOperations(t)
				operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(whiteboard.Whiteboard{
					ID: testWhiteboardID, Kind: whiteboard.KindHTML, Source: []byte("private html"),
				}, nil).Once()
				return operations
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handlerMux(t, newHandler(t, tt.operations(t), defaultMaxBytes)).ServeHTTP(rr,
				httptest.NewRequest(http.MethodGet, httpx.APIWhiteboardMarkdownResource+tt.id, nil))

			require.Equal(t, http.StatusNotFound, rr.Code)
			require.Equal(t, wantBody, rr.Body.String())
			require.NotContains(t, rr.Body.String(), "private")
			require.NotContains(t, rr.Body.String(), "generation")
		})
	}
}

func TestHandlerViewHTMLOuterServesStaticSandboxWrapperWithoutStoredBytes(t *testing.T) {
	source := []byte(`<!doctype html><script>globalThis.SUBMITTED_SECRET = "private"</script>`)
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockWhiteboardOperations(t)
	operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(whiteboard.Whiteboard{
		ID: testWhiteboardID, Kind: whiteboard.KindHTML, Source: source,
	}, nil).Once()
	rr := httptest.NewRecorder()

	handlerMux(t, newHandler(t, operations, defaultMaxBytes)).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, httpx.PublicHTML+testWhiteboardID, nil).WithContext(ctx))

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "text/html; charset=utf-8", rr.Header().Get("Content-Type"))
	require.NotEqual(t, source, rr.Body.Bytes())
	require.NotContains(t, rr.Body.String(), "SUBMITTED_SECRET")
	require.Contains(t, rr.Body.String(), `src="`+httpx.PublicHTML+testWhiteboardID+httpx.PublicHTMLContentSuffix+`"`)
	require.Contains(t, rr.Body.String(), `sandbox="allow-scripts"`)
	require.Contains(t, rr.Body.String(), `referrerpolicy="no-referrer"`)
	require.Contains(t, rr.Body.String(), ` credentialless`)
	assertHTMLOuterHeaders(t, rr)
}

func TestHandlerViewHTMLInnerServesStoredDocumentBytesUnchanged(t *testing.T) {
	source := []byte("<!DOCTYPE html>\n<html><head><style>body { color: red; }</style></head>\n<body><script>globalThis.answer = 42;</script></body></html>\n")
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockWhiteboardOperations(t)
	operations.EXPECT().Get(
		mock.MatchedBy(func(got context.Context) bool {
			return got == ctx && got.Value(handlerContextKey{}) == "sentinel"
		}), testWhiteboardID,
	).Return(whiteboard.Whiteboard{ID: testWhiteboardID, Kind: whiteboard.KindHTML, Source: source}, nil).Once()
	rr := httptest.NewRecorder()

	handlerMux(t, newHandler(t, operations, defaultMaxBytes)).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, httpx.PublicHTML+testWhiteboardID+httpx.PublicHTMLContentSuffix, nil).WithContext(ctx))

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "text/html; charset=utf-8", rr.Header().Get("Content-Type"))
	require.Equal(t, source, rr.Body.Bytes())
	assertHTMLInnerHeaders(t, rr)
}

func TestHandlerHTMLOuterAndInnerErrorsHaveSecurityHeadersAndIndistinguishableBodies(t *testing.T) {
	wantBody := "{\"error\":{\"code\":\"not_found\",\"message\":\"resource not found\"}}\n"
	paths := []struct {
		name          string
		assertHeaders func(*testing.T, *httptest.ResponseRecorder)
	}{
		{name: "outer", assertHeaders: assertHTMLOuterHeaders},
		{name: "inner", assertHeaders: assertHTMLInnerHeaders},
	}
	for _, route := range paths {
		t.Run(route.name, func(t *testing.T) {
			for _, condition := range []string{"malformed", "missing", "expired", "wrong kind"} {
				t.Run(condition, func(t *testing.T) {
					id := testWhiteboardID
					operations := testutil.NewMockWhiteboardOperations(t)
					switch condition {
					case "malformed":
						id = "malformed"
					case "wrong kind":
						operations.EXPECT().Get(mock.Anything, id).Return(whiteboard.Whiteboard{Kind: whiteboard.KindMarkdown}, nil).Once()
					default:
						operations.EXPECT().Get(mock.Anything, id).Return(whiteboard.Whiteboard{},
							common.NewError(common.CodeNotFound, "resource not found", errors.New("private "+condition))).Once()
					}
					path := httpx.PublicHTML + id
					if route.name == "inner" {
						path += httpx.PublicHTMLContentSuffix
					}
					rr := httptest.NewRecorder()
					handlerMux(t, newHandler(t, operations, defaultMaxBytes)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
					require.Equal(t, http.StatusNotFound, rr.Code)
					require.Equal(t, wantBody, rr.Body.String())
					require.NotContains(t, rr.Body.String(), "private")
					route.assertHeaders(t, rr)
				})
			}
		})
	}
}

func TestHandlerPublicViewsHideMalformedMissingExpiredAndWrongKindAsSameNotFound(t *testing.T) {
	wantBody := "{\"error\":{\"code\":\"not_found\",\"message\":\"resource not found\"}}\n"

	for _, route := range whiteboardRoutes {
		t.Run(route.name, func(t *testing.T) {
			tests := []struct {
				name       string
				id         string
				operations func(*testing.T) *testutil.MockWhiteboardOperations
			}{
				{
					name: "malformed",
					id:   "malformed",
					operations: func(t *testing.T) *testutil.MockWhiteboardOperations {
						return testutil.NewMockWhiteboardOperations(t)
					},
				},
				{
					name: "missing",
					id:   testWhiteboardID,
					operations: func(t *testing.T) *testutil.MockWhiteboardOperations {
						operations := testutil.NewMockWhiteboardOperations(t)
						operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(
							whiteboard.Whiteboard{},
							common.NewError(common.CodeNotFound, "resource not found", errors.New("missing private record")),
						).Once()
						return operations
					},
				},
				{
					name: "expired",
					id:   testWhiteboardID,
					operations: func(t *testing.T) *testutil.MockWhiteboardOperations {
						operations := testutil.NewMockWhiteboardOperations(t)
						operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(
							whiteboard.Whiteboard{},
							common.NewError(common.CodeNotFound, "resource not found", errors.New("expired private record")),
						).Once()
						return operations
					},
				},
				{
					name: "wrong kind",
					id:   testWhiteboardID,
					operations: func(t *testing.T) *testutil.MockWhiteboardOperations {
						operations := testutil.NewMockWhiteboardOperations(t)
						wrongKind := whiteboard.KindHTML
						if route.kind == whiteboard.KindHTML {
							wrongKind = whiteboard.KindMarkdown
						}
						operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(whiteboard.Whiteboard{
							ID: testWhiteboardID, Kind: wrongKind, Source: []byte("private source"),
						}, nil).Once()
						return operations
					},
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodGet, route.publicPath+tt.id, nil)
					rr := httptest.NewRecorder()

					handlerMux(t, newHandler(t, tt.operations(t), defaultMaxBytes)).ServeHTTP(rr, req)

					require.Equal(t, http.StatusNotFound, rr.Code)
					require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
					require.Equal(t, wantBody, rr.Body.String())
					require.NotContains(t, rr.Body.String(), tt.name)
					require.NotContains(t, rr.Body.String(), "private")
					if route.kind == whiteboard.KindMarkdown {
						assertMarkdownHeaders(t, rr, newViewer(t).ContentSecurityPolicy())
					} else {
						assertHTMLOuterHeaders(t, rr)
					}
				})
			}
		})
	}
}

func TestHandlerMarkdownWritesRejectInvalidPairsBeforeServiceCalls(t *testing.T) {
	tests := []struct {
		name            string
		fields          []multipartField
		maxBytes        int64
		maxContextBytes int64
		wantStatus      int
		wantBody        string
	}{
		{
			name:     "missing file",
			fields:   []multipartField{{name: "context", filename: "context.md", value: "context"}},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"exactly one file and context are required\"}}\n",
		},
		{
			name:     "missing context",
			fields:   []multipartField{{name: "file", filename: "board.md", value: "source"}},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"exactly one file and context are required\"}}\n",
		},
		{
			name: "duplicate file",
			fields: []multipartField{
				{name: "file", filename: "one.md", value: "one"},
				{name: "context", filename: "context.md", value: "context"},
				{name: "file", filename: "two.md", value: "two"},
			},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"exactly one file and context are required\"}}\n",
		},
		{
			name: "duplicate context",
			fields: []multipartField{
				{name: "file", filename: "board.md", value: "source"},
				{name: "context", filename: "one.md", value: "one"},
				{name: "context", filename: "two.md", value: "two"},
			},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"exactly one file and context are required\"}}\n",
		},
		{
			name: "unknown field",
			fields: []multipartField{
				{name: "file", filename: "board.md", value: "source"},
				{name: "context", filename: "context.md", value: "context"},
				{name: "private", filename: "private.md", value: "secret"},
			},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"unexpected multipart field\"}}\n",
		},
		{
			name: "context is not a file",
			fields: []multipartField{
				{name: "file", filename: "board.md", value: "source"},
				{name: "context", value: "context"},
			},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"unexpected multipart field\"}}\n",
		},
		{
			name: "empty file",
			fields: []multipartField{
				{name: "file", filename: "board.md", value: ""},
				{name: "context", filename: "context.md", value: "context"},
			},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"file and context must not be empty\"}}\n",
		},
		{
			name: "empty context",
			fields: []multipartField{
				{name: "file", filename: "board.md", value: "source"},
				{name: "context", filename: "context.md", value: ""},
			},
			maxBytes: 16, maxContextBytes: 16, wantStatus: http.StatusBadRequest,
			wantBody: "{\"error\":{\"code\":\"invalid_request\",\"message\":\"file and context must not be empty\"}}\n",
		},
		{
			name: "file over independent limit",
			fields: []multipartField{
				{name: "file", filename: "board.md", value: "four"},
				{name: "context", filename: "context.md", value: "context"},
			},
			maxBytes: 3, maxContextBytes: 16, wantStatus: http.StatusRequestEntityTooLarge,
			wantBody: "{\"error\":{\"code\":\"content_too_large\",\"message\":\"content too large\"}}\n",
		},
		{
			name: "context over independent limit",
			fields: []multipartField{
				{name: "file", filename: "board.md", value: "source"},
				{name: "context", filename: "context.md", value: "four"},
			},
			maxBytes: 16, maxContextBytes: 3, wantStatus: http.StatusRequestEntityTooLarge,
			wantBody: "{\"error\":{\"code\":\"content_too_large\",\"message\":\"content too large\"}}\n",
		},
	}

	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					operations := testutil.NewMockWhiteboardOperations(t)
					handler := newHandlerWithLimits(t, operations, tt.maxBytes, tt.maxContextBytes)
					body, contentType := multipartRequestBody(t, tt.fields...)
					path := httpx.APIWhiteboardMarkdown
					if method == http.MethodPut {
						path += "/" + testWhiteboardID
					}
					req := httptest.NewRequest(method, path, bytes.NewReader(body))
					req.Header.Set("Content-Type", contentType)
					rr := httptest.NewRecorder()

					handlerMux(t, handler).ServeHTTP(rr, req)

					require.Equal(t, tt.wantStatus, rr.Code)
					require.Equal(t, tt.wantBody, rr.Body.String())
				})
			}
		})
	}
}

func TestHandlerMarkdownMultipartAggregateAllowsConfiguredPayloadAndBoundsOverhead(t *testing.T) {
	operations := testutil.NewMockWhiteboardOperations(t)
	operations.EXPECT().CreateMarkdown(mock.Anything, mock.MatchedBy(func(input whiteboard.CreateInput) bool {
		return len(input.Source) == 64 && len(input.Context) == 32
	})).Return(whiteboard.Result{ID: testWhiteboardID, Kind: whiteboard.KindMarkdown}, nil).Once()
	handler := newHandlerWithLimits(t, operations, 64, 32)
	body, contentType := multipartRequestBody(t,
		multipartField{name: "file", filename: "board.md", value: strings.Repeat("s", 64)},
		multipartField{name: "context", filename: "context.md", value: strings.Repeat("c", 32)},
	)
	req := httptest.NewRequest(http.MethodPost, httpx.APIWhiteboardMarkdown, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	handlerMux(t, handler).ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)

	hugeFilename := strings.Repeat("x", int(httpx.MultipartOverheadBytes)+1) + ".md"
	body, contentType = multipartRequestBody(t,
		multipartField{name: "file", filename: hugeFilename, value: "s"},
		multipartField{name: "context", filename: "context.md", value: "c"},
	)
	req = httptest.NewRequest(http.MethodPost, httpx.APIWhiteboardMarkdown, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr = httptest.NewRecorder()

	handlerMux(t, newHandlerWithLimits(t, testutil.NewMockWhiteboardOperations(t), 1, 1)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
	require.Equal(t, "{\"error\":{\"code\":\"content_too_large\",\"message\":\"content too large\"}}\n", rr.Body.String())
}

func TestHandlerRejectsInvalidFormsBeforeServiceCalls(t *testing.T) {
	for _, route := range whiteboardRoutes {
		t.Run(route.name, func(t *testing.T) {
			tests := []struct {
				name       string
				fields     []multipartField
				maxBytes   int64
				wantStatus int
			}{
				{
					name:       "missing file",
					fields:     []multipartField{{name: "expires_in_seconds", value: "60"}},
					maxBytes:   defaultMaxBytes,
					wantStatus: http.StatusBadRequest,
				},
				{
					name: "extra file",
					fields: []multipartField{
						{name: "file", filename: "one.txt", value: "one"},
						{name: "file", filename: "two.txt", value: "two"},
					},
					maxBytes:   defaultMaxBytes,
					wantStatus: http.StatusBadRequest,
				},
				{
					name: "duplicate expiration",
					fields: []multipartField{
						{name: "file", filename: "board.txt", value: "source"},
						{name: "expires_in_seconds", value: "1"},
						{name: "expires_in_seconds", value: "2"},
					},
					maxBytes:   defaultMaxBytes,
					wantStatus: http.StatusBadRequest,
				},
				{
					name:       "oversized content",
					fields:     []multipartField{{name: "file", filename: "board.txt", value: strings.Repeat("x", 1024)}},
					maxBytes:   64,
					wantStatus: http.StatusRequestEntityTooLarge,
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					operations := testutil.NewMockWhiteboardOperations(t)
					handler := newHandler(t, operations, tt.maxBytes)
					body, contentType := multipartRequestBody(t, tt.fields...)
					req := httptest.NewRequest(http.MethodPost, route.apiPath, bytes.NewReader(body))
					req.Header.Set("Content-Type", contentType)
					rr := httptest.NewRecorder()

					handlerMux(t, handler).ServeHTTP(rr, req)

					require.Equal(t, tt.wantStatus, rr.Code)
				})
			}
		})
	}
}

func TestHandlerHidesMalformedCapabilityIDsBeforeReadingFormsOrCallingService(t *testing.T) {
	wantBody := "{\"error\":{\"code\":\"not_found\",\"message\":\"resource not found\"}}\n"

	for _, route := range whiteboardRoutes {
		t.Run(route.name, func(t *testing.T) {
			for _, method := range []string{http.MethodPut, http.MethodDelete} {
				t.Run(method, func(t *testing.T) {
					operations := testutil.NewMockWhiteboardOperations(t)
					handler := newHandler(t, operations, defaultMaxBytes)
					req := httptest.NewRequest(method, route.apiPath+"/malformed", strings.NewReader("not multipart"))
					rr := httptest.NewRecorder()

					handlerMux(t, handler).ServeHTTP(rr, req)

					require.Equal(t, http.StatusNotFound, rr.Code)
					require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
					require.Equal(t, wantBody, rr.Body.String())
					require.NotContains(t, rr.Body.String(), "malformed")
					require.NotContains(t, rr.Body.String(), "invalid resource id")
				})
			}
		})
	}
}

func TestHandlerMapsWrongKindServiceErrorsToNotFound(t *testing.T) {
	for _, route := range whiteboardRoutes {
		t.Run(route.name, func(t *testing.T) {
			operations := testutil.NewMockWhiteboardOperations(t)
			operations.EXPECT().Update(mock.Anything, mock.MatchedBy(func(got whiteboard.UpdateInput) bool {
				return got.ID == testWhiteboardID && got.Kind == route.kind
			})).Return(whiteboard.Result{}, common.NewError(common.CodeNotFound, "resource not found", errors.New("wrong kind"))).Once()
			handler := newHandler(t, operations, defaultMaxBytes)
			fields := []multipartField{{name: "file", filename: "board.txt", value: "replacement"}}
			if route.kind == whiteboard.KindMarkdown {
				fields = append(fields, multipartField{name: "context", filename: "context.md", value: "replacement context"})
			}
			body, contentType := multipartRequestBody(t, fields...)
			req := httptest.NewRequest(http.MethodPut, route.apiPath+"/"+testWhiteboardID, bytes.NewReader(body))
			req.Header.Set("Content-Type", contentType)
			rr := httptest.NewRecorder()

			handlerMux(t, handler).ServeHTTP(rr, req)

			require.Equal(t, http.StatusNotFound, rr.Code)
			require.Equal(t, httpx.ErrorBody{Code: common.CodeNotFound, Message: "resource not found"}, decodeError(t, rr))
			require.NotContains(t, rr.Body.String(), "wrong kind")
		})
	}
}

func TestHandlerRegistersOnlyExactMutationAndPublicViewRoutes(t *testing.T) {
	handler := newHandler(t, testutil.NewMockWhiteboardOperations(t), defaultMaxBytes)
	mux := handlerMux(t, handler)

	for _, route := range whiteboardRoutes {
		t.Run(route.name+" wrong management method", func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route.apiPath, nil))
			require.Equal(t, http.StatusMethodNotAllowed, rr.Code)
			require.Equal(t, http.MethodPost, rr.Header().Get("Allow"))
		})

		t.Run(route.name+" wrong public method", func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, route.publicPath+testWhiteboardID, nil))
			require.Equal(t, http.StatusMethodNotAllowed, rr.Code)
			require.Equal(t, "GET, HEAD", rr.Header().Get("Allow"))
		})

		t.Run(route.name+" empty public id is not registered", func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route.publicPath, nil))
			require.Equal(t, http.StatusNotFound, rr.Code)
		})

		t.Run(route.name+" nested public path is not registered", func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route.publicPath+testWhiteboardID+"/extra", nil))
			require.Equal(t, http.StatusNotFound, rr.Code)
		})

		t.Run(route.name+" trailing slash is not registered", func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, route.apiPath+"/", nil))
			require.Equal(t, http.StatusNotFound, rr.Code)
		})
	}

	for _, path := range []string{
		httpx.PublicHTML + testWhiteboardID + httpx.PublicHTMLContentSuffix + "/extra",
		httpx.PublicHTML + testWhiteboardID + "/raw",
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, rr.Code)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		httpx.PublicHTML+testWhiteboardID+httpx.PublicHTMLContentSuffix, nil))
	require.Equal(t, http.StatusMethodNotAllowed, rr.Code)
	require.Equal(t, "GET, HEAD", rr.Header().Get("Allow"))
}

func TestHandlerPublicHeadResponsesUseExactRoutesHeadersAndNoBody(t *testing.T) {
	paths := []struct {
		name          string
		path          string
		kind          whiteboard.Kind
		assertHeaders func(*testing.T, *httptest.ResponseRecorder)
	}{
		{name: "markdown", path: httpx.PublicMarkdown + testWhiteboardID, kind: whiteboard.KindMarkdown,
			assertHeaders: func(t *testing.T, rr *httptest.ResponseRecorder) {
				assertMarkdownHeaders(t, rr, newViewer(t).ContentSecurityPolicy())
			}},
		{name: "html outer", path: httpx.PublicHTML + testWhiteboardID, kind: whiteboard.KindHTML, assertHeaders: assertHTMLOuterHeaders},
		{name: "html inner", path: httpx.PublicHTML + testWhiteboardID + httpx.PublicHTMLContentSuffix,
			kind: whiteboard.KindHTML, assertHeaders: assertHTMLInnerHeaders},
	}
	for _, tt := range paths {
		t.Run(tt.name, func(t *testing.T) {
			operations := testutil.NewMockWhiteboardOperations(t)
			operations.EXPECT().Get(mock.Anything, testWhiteboardID).Return(whiteboard.Whiteboard{
				ID: testWhiteboardID, Kind: tt.kind, Source: []byte("must not be served"),
			}, nil).Once()
			rr := httptest.NewRecorder()
			handlerMux(t, newHandler(t, operations, defaultMaxBytes)).ServeHTTP(rr,
				httptest.NewRequest(http.MethodHead, tt.path, nil))
			require.Equal(t, http.StatusOK, rr.Code)
			require.Empty(t, rr.Body.String())
			tt.assertHeaders(t, rr)
		})
	}
}

func TestHandlerDoesNotLogRequestBodiesOrCapabilityIDs(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	operations := testutil.NewMockWhiteboardOperations(t)
	operations.EXPECT().Update(mock.Anything, mock.Anything).Return(
		whiteboard.Result{},
		common.NewError(common.CodeStorageUnavailable, "storage unavailable", errors.New("private backend failure")),
	).Once()
	handler := newHandler(t, operations, defaultMaxBytes)
	bodySecret := "private-whiteboard-source"
	body, contentType := multipartRequestBody(t,
		multipartField{name: "file", filename: "board.txt", value: bodySecret},
		multipartField{name: "context", filename: "context.md", value: "private creator context"},
	)
	req := httptest.NewRequest(http.MethodPut, httpx.APIWhiteboardMarkdown+"/"+testWhiteboardID, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	handlerMux(t, handler).ServeHTTP(rr, req)

	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
	require.NotContains(t, logs.String(), bodySecret)
	require.NotContains(t, logs.String(), testWhiteboardID)
}

type multipartField struct {
	name     string
	filename string
	value    string
}

func multipartRequestBody(t *testing.T, fields ...multipartField) ([]byte, string) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, field := range fields {
		var (
			partWriter interface{ Write([]byte) (int, error) }
			err        error
		)
		if field.filename == "" {
			partWriter, err = writer.CreateFormField(field.name)
		} else {
			partWriter, err = writer.CreateFormFile(field.name, field.filename)
		}
		require.NoError(t, err)
		_, err = partWriter.Write([]byte(field.value))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return body.Bytes(), writer.FormDataContentType()
}

func newHandler(t *testing.T, operations whiteboard.Operations, maxBytes int64) *whiteboard.Handler {
	t.Helper()
	return newHandlerWithLimits(t, operations, maxBytes, defaultMaxContextBytes)
}

func newHandlerWithLimits(t *testing.T, operations whiteboard.Operations, maxBytes, maxContextBytes int64) *whiteboard.Handler {
	t.Helper()

	handler, err := whiteboard.NewHandler(operations, newViewer(t), whiteboard.HandlerConfig{
		MaxWhiteboardBytes: maxBytes,
		MaxContextBytes:    maxContextBytes,
	})
	require.NoError(t, err)
	return handler
}

func newViewer(t *testing.T) *whiteboard.Viewer {
	t.Helper()

	viewer, err := whiteboard.NewViewer(whiteboard.ViewerConfig{
		CSS: []byte(testViewerCSS),
		JS:  []byte(testViewerJS),
	})
	require.NoError(t, err)
	return viewer
}

func handlerMux(t *testing.T, handler *whiteboard.Handler) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	handler.Register(mux)
	return mux
}

func decodeResource(t *testing.T, rr *httptest.ResponseRecorder) httpx.Resource {
	t.Helper()

	var response httpx.ResourceResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &response))
	return response.Resource
}

func decodeError(t *testing.T, rr *httptest.ResponseRecorder) httpx.ErrorBody {
	t.Helper()
	return decodeErrorBody(t, rr).Error
}

func decodeErrorBody(t *testing.T, rr *httptest.ResponseRecorder) httpx.ErrorResponse {
	t.Helper()

	var response httpx.ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &response))
	return response
}

func int64Pointer(value int64) *int64 {
	return &value
}

func assertPublicWhiteboardHeaders(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "noindex, nofollow, noarchive", rr.Header().Get("X-Robots-Tag"))
}

func assertMarkdownHeaders(t *testing.T, rr *httptest.ResponseRecorder, csp string) {
	t.Helper()
	assertPublicWhiteboardHeaders(t, rr)
	require.Equal(t, "DENY", rr.Header().Get("X-Frame-Options"))
	require.Equal(t, "no-referrer", rr.Header().Get("Referrer-Policy"))
	require.Equal(t, whiteboard.RestrictivePermissionsPolicy, rr.Header().Get("Permissions-Policy"))
	require.Equal(t, csp, rr.Header().Get("Content-Security-Policy"))
	require.Contains(t, csp, "frame-ancestors 'none'")
}

func assertHTMLOuterHeaders(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	assertPublicWhiteboardHeaders(t, rr)
	require.Equal(t, "DENY", rr.Header().Get("X-Frame-Options"))
	require.Equal(t, "no-referrer", rr.Header().Get("Referrer-Policy"))
	require.Equal(t, whiteboard.RestrictivePermissionsPolicy, rr.Header().Get("Permissions-Policy"))
	require.Equal(t, whiteboard.StandaloneOuterContentSecurityPolicy, rr.Header().Get("Content-Security-Policy"))
}

func assertHTMLInnerHeaders(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	assertPublicWhiteboardHeaders(t, rr)
	require.Equal(t, "SAMEORIGIN", rr.Header().Get("X-Frame-Options"))
	require.Equal(t, "no-referrer", rr.Header().Get("Referrer-Policy"))
	require.Equal(t, whiteboard.RestrictivePermissionsPolicy, rr.Header().Get("Permissions-Policy"))
	require.Equal(t, whiteboard.StandaloneInnerContentSecurityPolicy, rr.Header().Get("Content-Security-Policy"))
	require.NotContains(t, rr.Header().Get("Content-Security-Policy"), "navigate-to")
}
