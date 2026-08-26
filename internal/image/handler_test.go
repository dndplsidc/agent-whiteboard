package image_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/dndplsidc/agent-whiteboard/internal/image"
	"github.com/dndplsidc/agent-whiteboard/internal/testutil"
	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testImageID         = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	secondTestImageID   = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	defaultImageLimit   = int64(1 << 20)
	defaultRequestLimit = int64(4 << 20)
)

type handlerContextKey struct{}

func TestHandlerConstructorRejectsInvalidDependenciesAndLimits(t *testing.T) {
	var typedNil *testutil.MockImageOperations
	tests := []struct {
		name       string
		operations image.Operations
		config     image.HandlerConfig
	}{
		{name: "nil operations"},
		{name: "typed nil operations", operations: typedNil},
		{name: "negative image limit", operations: testutil.NewMockImageOperations(t), config: image.HandlerConfig{MaxImageBytes: -1}},
		{name: "negative request limit", operations: testutil.NewMockImageOperations(t), config: image.HandlerConfig{MaxRequestBytes: -1}},
		{
			name:       "request limit below image limit",
			operations: testutil.NewMockImageOperations(t),
			config:     image.HandlerConfig{MaxImageBytes: 2, MaxRequestBytes: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := image.NewHandler(tt.operations, tt.config)

			require.Nil(t, handler)
			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
		})
	}
}

func TestHandlerConstructorAcceptsZeroLimits(t *testing.T) {
	handler, err := image.NewHandler(testutil.NewMockImageOperations(t), image.HandlerConfig{})

	require.NoError(t, err)
	require.NotNil(t, handler)
}

func TestHandlerCreatePreservesOrderAndPassesExactContext(t *testing.T) {
	createdAt := time.Date(2026, time.July, 17, 3, 4, 5, 0, time.UTC)
	expiresAt := createdAt.Add(5 * time.Minute)
	expiresIn := int64(300)
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockImageOperations(t)
	operations.EXPECT().CreateImages(
		exactContext(ctx),
		mock.MatchedBy(func(got image.CreateInput) bool {
			return len(got.Images) == 2 &&
				bytes.Equal(got.Images[0].Content, []byte("first image")) &&
				bytes.Equal(got.Images[1].Content, []byte("second image")) &&
				got.Images[0].ExpiresInSeconds != nil && *got.Images[0].ExpiresInSeconds == expiresIn &&
				got.Images[1].ExpiresInSeconds != nil && *got.Images[1].ExpiresInSeconds == expiresIn
		}),
	).Return([]image.Result{
		{
			ID: testImageID, Extension: ".png", MediaType: "image/png",
			CreatedAt: createdAt, UpdatedAt: createdAt, ExpiresAt: &expiresAt,
		},
		{
			ID: secondTestImageID, Extension: ".webp", MediaType: "image/webp",
			CreatedAt: createdAt, UpdatedAt: createdAt,
		},
	}, nil).Once()
	body, contentType := multipartRequestBody(t,
		multipartField{name: "images", filename: "client-second-name.svg", value: "first image"},
		multipartField{name: "images", filename: "client-first-name.png", value: "second image"},
		multipartField{name: "expires_in_seconds", value: fmt.Sprint(expiresIn)},
	)
	req := httptest.NewRequest(http.MethodPost, httpx.APIImages, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	require.Equal(t, []httpx.Resource{
		{
			ID: testImageID, Filename: testImageID + ".png", Extension: ".png", MediaType: "image/png",
			Path: httpx.PublicImages + testImageID, CreatedAt: createdAt, UpdatedAt: createdAt,
			ExpiresAt: int64Pointer(expiresAt.Unix()), Permanent: false,
		},
		{
			ID: secondTestImageID, Filename: secondTestImageID + ".webp", Extension: ".webp", MediaType: "image/webp",
			Path: httpx.PublicImages + secondTestImageID, CreatedAt: createdAt, UpdatedAt: createdAt,
			Permanent: true,
		},
	}, decodeImages(t, rr))
}

func TestHandlerRejectsInvalidCreateFormsBeforeServiceCalls(t *testing.T) {
	tests := []struct {
		name       string
		fields     []multipartField
		wantStatus int
	}{
		{name: "no image parts", fields: []multipartField{{name: "expires_in_seconds", value: "60"}}, wantStatus: http.StatusBadRequest},
		{name: "wrong file field", fields: []multipartField{{name: "file", filename: "image.png", value: "content"}}, wantStatus: http.StatusBadRequest},
		{name: "text in image field", fields: []multipartField{{name: "images", value: "content"}}, wantStatus: http.StatusBadRequest},
		{
			name: "duplicate expiration",
			fields: []multipartField{
				{name: "images", filename: "image.png", value: "content"},
				{name: "expires_in_seconds", value: "1"},
				{name: "expires_in_seconds", value: "2"},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid expiration",
			fields: []multipartField{
				{name: "images", filename: "image.png", value: "content"},
				{name: "expires_in_seconds", value: "tomorrow"},
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			operations := testutil.NewMockImageOperations(t)
			body, contentType := multipartRequestBody(t, tt.fields...)
			req := httptest.NewRequest(http.MethodPost, httpx.APIImages, bytes.NewReader(body))
			req.Header.Set("Content-Type", contentType)
			rr := httptest.NewRecorder()

			newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

			require.Equal(t, tt.wantStatus, rr.Code)
			require.Equal(t, common.CodeInvalidRequest, decodeError(t, rr).Code)
		})
	}
}

func TestHandlerRejectsPerImageAndAggregateLimitsBeforeServiceCalls(t *testing.T) {
	t.Run("per image", func(t *testing.T) {
		operations := testutil.NewMockImageOperations(t)
		body, contentType := multipartRequestBody(t,
			multipartField{name: "images", filename: "large.png", value: "1234"},
		)
		req := httptest.NewRequest(http.MethodPost, httpx.APIImages, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		rr := httptest.NewRecorder()

		newMux(t, newHandler(t, operations, 3, int64(len(body)))).ServeHTTP(rr, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
		require.Equal(t, common.CodeContentTooLarge, decodeError(t, rr).Code)
	})

	t.Run("aggregate request", func(t *testing.T) {
		operations := testutil.NewMockImageOperations(t)
		body, contentType := multipartRequestBody(t,
			multipartField{name: "images", filename: "one.png", value: "1"},
			multipartField{name: "images", filename: "two.png", value: "2"},
		)
		req := httptest.NewRequest(http.MethodPost, httpx.APIImages, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		rr := httptest.NewRecorder()

		newMux(t, newHandler(t, operations, 1, int64(len(body)-1))).ServeHTTP(rr, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
		require.Equal(t, common.CodeContentTooLarge, decodeError(t, rr).Code)
	})
}

func TestHandlerImageLimitDoesNotApplyToExpirationField(t *testing.T) {
	expiresIn := int64(60)
	operations := testutil.NewMockImageOperations(t)
	operations.EXPECT().CreateImages(mock.Anything, mock.MatchedBy(func(got image.CreateInput) bool {
		return len(got.Images) == 1 && bytes.Equal(got.Images[0].Content, []byte("x")) &&
			got.Images[0].ExpiresInSeconds != nil && *got.Images[0].ExpiresInSeconds == expiresIn
	})).Return([]image.Result{{ID: testImageID, Extension: ".png", MediaType: "image/png"}}, nil).Once()
	body, contentType := multipartRequestBody(t,
		multipartField{name: "images", filename: "image.png", value: "x"},
		multipartField{name: "expires_in_seconds", value: "60"},
	)
	req := httptest.NewRequest(http.MethodPost, httpx.APIImages, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	newMux(t, newHandler(t, operations, 1, int64(len(body)))).ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
}

func TestHandlerPassesSignedExpirationAndMapsCreateErrors(t *testing.T) {
	expiresIn := int64(-1)
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockImageOperations(t)
	operations.EXPECT().CreateImages(
		exactContext(ctx),
		mock.MatchedBy(func(got image.CreateInput) bool {
			return len(got.Images) == 1 && got.Images[0].ExpiresInSeconds != nil && *got.Images[0].ExpiresInSeconds == expiresIn
		}),
	).Return(nil, common.NewError(common.CodeInvalidRequest, "expiration must not be negative", errors.New("private detail"))).Once()
	body, contentType := multipartRequestBody(t,
		multipartField{name: "images", filename: "image.png", value: "content"},
		multipartField{name: "expires_in_seconds", value: "-1"},
	)
	req := httptest.NewRequest(http.MethodPost, httpx.APIImages, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Equal(t, httpx.ErrorBody{Code: common.CodeInvalidRequest, Message: "expiration must not be negative"}, decodeError(t, rr))
	require.NotContains(t, rr.Body.String(), "private detail")
}

func TestHandlerUpdateAcceptsExactlyOneFileAndPreservesPublicPath(t *testing.T) {
	createdAt := time.Date(2026, time.July, 16, 3, 4, 5, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockImageOperations(t)
	operations.EXPECT().Update(
		exactContext(ctx),
		mock.MatchedBy(func(got image.UpdateInput) bool {
			return got.ID == testImageID && bytes.Equal(got.Content, []byte("replacement")) && got.ExpiresInSeconds == nil
		}),
	).Return(image.Result{
		ID: testImageID, Extension: ".webp", MediaType: "image/webp",
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil).Once()
	body, contentType := multipartRequestBody(t,
		multipartField{name: "file", filename: "misleading.png", value: "replacement"},
	)
	req := httptest.NewRequest(http.MethodPut, httpx.APIImages+"/"+testImageID, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()

	newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, httpx.Resource{
		ID: testImageID, Filename: testImageID + ".webp", Extension: ".webp", MediaType: "image/webp",
		Path: httpx.PublicImages + testImageID, CreatedAt: createdAt, UpdatedAt: updatedAt, Permanent: true,
	}, decodeResource(t, rr))
}

func TestHandlerUpdateRejectsInvalidFileCountsBeforeServiceCall(t *testing.T) {
	tests := []struct {
		name   string
		fields []multipartField
	}{
		{name: "missing file", fields: []multipartField{{name: "expires_in_seconds", value: "60"}}},
		{
			name: "multiple files",
			fields: []multipartField{
				{name: "file", filename: "one.png", value: "one"},
				{name: "file", filename: "two.png", value: "two"},
			},
		},
		{name: "create field", fields: []multipartField{{name: "images", filename: "image.png", value: "content"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			operations := testutil.NewMockImageOperations(t)
			body, contentType := multipartRequestBody(t, tt.fields...)
			req := httptest.NewRequest(http.MethodPut, httpx.APIImages+"/"+testImageID, bytes.NewReader(body))
			req.Header.Set("Content-Type", contentType)
			rr := httptest.NewRecorder()

			newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

			require.Equal(t, http.StatusBadRequest, rr.Code)
			require.Equal(t, common.CodeInvalidRequest, decodeError(t, rr).Code)
		})
	}
}

func TestHandlerDeleteReturnsNoContentAndPassesExactContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
	operations := testutil.NewMockImageOperations(t)
	operations.EXPECT().Delete(exactContext(ctx), testImageID).Return(nil).Once()
	req := httptest.NewRequest(http.MethodDelete, httpx.APIImages+"/"+testImageID, nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusNoContent, rr.Code)
	require.Empty(t, rr.Body.String())
}

func TestHandlerMapsMutationServiceErrorsAndCancellation(t *testing.T) {
	t.Run("update not found", func(t *testing.T) {
		operations := testutil.NewMockImageOperations(t)
		operations.EXPECT().Update(mock.Anything, mock.Anything).Return(image.Result{}, common.NewError(common.CodeNotFound, "resource not found", errors.New("expired"))).Once()
		body, contentType := multipartRequestBody(t, multipartField{name: "file", filename: "image.png", value: "replacement"})
		req := httptest.NewRequest(http.MethodPut, httpx.APIImages+"/"+testImageID, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		rr := httptest.NewRecorder()

		newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

		require.Equal(t, http.StatusNotFound, rr.Code)
		require.Equal(t, httpx.ErrorBody{Code: common.CodeNotFound, Message: "resource not found"}, decodeError(t, rr))
	})

	t.Run("delete canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		operations := testutil.NewMockImageOperations(t)
		operations.EXPECT().Delete(exactContext(ctx), testImageID).Return(context.Canceled).Once()
		req := httptest.NewRequest(http.MethodDelete, httpx.APIImages+"/"+testImageID, nil).WithContext(ctx)
		rr := httptest.NewRecorder()

		newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

		require.Equal(t, http.StatusInternalServerError, rr.Code)
		require.Equal(t, httpx.ErrorBody{Code: common.CodeInternal, Message: "internal error"}, decodeError(t, rr))
	})
}

func TestHandlerHidesMalformedCapabilityIDsAsNotFoundBeforeCallingService(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			operations := testutil.NewMockImageOperations(t)
			path := httpx.APIImages + "/malformed"
			if method == http.MethodGet {
				path = httpx.PublicImages + "malformed"
			}
			req := httptest.NewRequest(method, path, strings.NewReader("not multipart"))
			rr := httptest.NewRecorder()

			newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

			require.Equal(t, http.StatusNotFound, rr.Code)
			require.Equal(t, "{\"error\":{\"code\":\"not_found\",\"message\":\"resource not found\"}}\n", rr.Body.String())
			require.NotContains(t, rr.Body.String(), "invalid")
			require.NotContains(t, rr.Body.String(), "malformed")
		})
	}
}

func TestHandlerViewServesExactBytesAndDetectedMetadataAtExtensionlessPath(t *testing.T) {
	formats := []struct {
		name      string
		extension string
		mediaType string
		content   []byte
	}{
		{name: "png", extension: ".png", mediaType: "image/png", content: []byte{'P', 'N', 'G', 0, 1}},
		{name: "webp", extension: ".webp", mediaType: "image/webp", content: []byte{'W', 'E', 'B', 'P', 0, 2}},
	}

	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), handlerContextKey{}, "sentinel")
			operations := testutil.NewMockImageOperations(t)
			operations.EXPECT().Get(exactContext(ctx), testImageID).Return(image.Image{
				ID: testImageID, Extension: format.extension, MediaType: format.mediaType, Content: format.content,
			}, nil).Once()
			req := httptest.NewRequest(http.MethodGet, httpx.PublicImages+testImageID, nil).WithContext(ctx)
			rr := httptest.NewRecorder()

			newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

			require.Equal(t, http.StatusOK, rr.Code)
			require.Equal(t, format.content, rr.Body.Bytes())
			require.Equal(t, format.mediaType, rr.Header().Get("Content-Type"))
			require.Equal(t, mime.FormatMediaType("inline", map[string]string{
				"filename": testImageID + format.extension,
			}), rr.Header().Get("Content-Disposition"))
			assertPublicImageHeaders(t, rr)
		})
	}
}

func TestHandlerViewMapsMissingAndExpiredImagesToStableJSONNotFound(t *testing.T) {
	for _, name := range []string{"missing", "expired"} {
		t.Run(name, func(t *testing.T) {
			operations := testutil.NewMockImageOperations(t)
			operations.EXPECT().Get(mock.Anything, testImageID).Return(image.Image{}, common.NewError(common.CodeNotFound, "resource not found", errors.New(name))).Once()
			req := httptest.NewRequest(http.MethodGet, httpx.PublicImages+testImageID, nil)
			rr := httptest.NewRecorder()

			newMux(t, newHandler(t, operations, defaultImageLimit, defaultRequestLimit)).ServeHTTP(rr, req)

			require.Equal(t, http.StatusNotFound, rr.Code)
			require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
			require.Equal(t, httpx.ErrorBody{Code: common.CodeNotFound, Message: "resource not found"}, decodeError(t, rr))
			require.NotContains(t, rr.Body.String(), name)
			assertPublicImageHeaders(t, rr)
		})
	}
}

func TestHandlerRegistersOnlyImageAPIAndExtensionlessPublicRoutes(t *testing.T) {
	mux := newMux(t, newHandler(t, testutil.NewMockImageOperations(t), defaultImageLimit, defaultRequestLimit))

	tests := []struct {
		name      string
		method    string
		path      string
		wantCode  int
		wantAllow string
	}{
		{name: "wrong collection method", method: http.MethodGet, path: httpx.APIImages, wantCode: http.StatusMethodNotAllowed, wantAllow: http.MethodPost},
		{name: "wrong item method", method: http.MethodPost, path: httpx.APIImages + "/" + testImageID, wantCode: http.StatusMethodNotAllowed, wantAllow: "DELETE, PUT"},
		{name: "wrong public method", method: http.MethodPost, path: httpx.PublicImages + testImageID, wantCode: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "collection trailing slash", method: http.MethodPost, path: httpx.APIImages + "/", wantCode: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))
			require.Equal(t, tt.wantCode, rr.Code)
			require.Equal(t, tt.wantAllow, rr.Header().Get("Allow"))
		})
	}
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

func newHandler(t *testing.T, operations image.Operations, maxImageBytes, maxRequestBytes int64) *image.Handler {
	t.Helper()

	handler, err := image.NewHandler(operations, image.HandlerConfig{
		MaxImageBytes: maxImageBytes, MaxRequestBytes: maxRequestBytes,
	})
	require.NoError(t, err)
	require.NotNil(t, handler)
	return handler
}

func newMux(t *testing.T, handler *image.Handler) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	handler.Register(mux)
	return mux
}

func exactContext(want context.Context) any {
	return mock.MatchedBy(func(got context.Context) bool {
		if got != want {
			return false
		}
		wantSentinel := want.Value(handlerContextKey{})
		return wantSentinel == nil || got.Value(handlerContextKey{}) == wantSentinel
	})
}

func decodeImages(t *testing.T, rr *httptest.ResponseRecorder) []httpx.Resource {
	t.Helper()

	var response httpx.ImagesResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &response))
	return response.Images
}

func decodeResource(t *testing.T, rr *httptest.ResponseRecorder) httpx.Resource {
	t.Helper()

	var response httpx.ResourceResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &response))
	return response.Resource
}

func decodeError(t *testing.T, rr *httptest.ResponseRecorder) httpx.ErrorBody {
	t.Helper()

	var response httpx.ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &response))
	return response.Error
}

func assertPublicImageHeaders(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "noindex, nofollow, noarchive, noimageindex", rr.Header().Get("X-Robots-Tag"))
}

func int64Pointer(value int64) *int64 { return &value }
