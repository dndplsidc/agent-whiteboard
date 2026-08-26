package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	imageDomain "github.com/dndplsidc/agent-whiteboard/internal/image"
	"github.com/dndplsidc/agent-whiteboard/internal/store"
	"github.com/dndplsidc/agent-whiteboard/pkg/agentwb"
	"github.com/stretchr/testify/require"
)

const (
	png1x1Base64  = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	jpeg1x1Base64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////2wBDAf//////////////////////////////////////////////////////////////////////////////////////wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAf/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIQAxAAAAF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABBQJ//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAwEBPwF//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAgEBPwF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQAGPwJ//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPyF//9oADAMBAAIAAwAAABD/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAEDAQE/EB//xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAECAQE/EB//xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oACAEBAAE/EB//2Q=="
	gif1x1Base64  = "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw=="
	webp1x1Base64 = "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"
)

type imageFixture struct {
	path      string
	content   []byte
	extension string
	mediaType string
}

func TestImageLifecycleOrderedFormats(t *testing.T) {
	server := startServer(t)
	fixtures := decodeImageFixtures(t)
	args := []string{"--json", "image", "upload"}
	for _, fixture := range fixtures {
		args = append(args, fixture.path)
	}
	stdout := runCLISuccess(t, server, args...)
	var created cliResourcesEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &created), stdout)
	require.Equal(t, 1, created.SchemaVersion)
	require.Len(t, created.Resources, len(fixtures))

	for index, resource := range created.Resources {
		fixture := fixtures[index]
		parsed, err := url.Parse(resource.URL)
		require.NoError(t, err)
		require.Equal(t, server.URL, parsed.Scheme+"://"+parsed.Host)
		require.Equal(t, "/images/"+resource.ID, parsed.Path)
		require.NotContains(t, filepath.Base(parsed.Path), ".")
		response, body := fetch(t, resource.URL)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Equal(t, fixture.content, []byte(body))
		require.Equal(t, fixture.mediaType, response.Header.Get("Content-Type"))
		require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
		require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
		require.Equal(t, "noindex, nofollow, noarchive, noimageindex", response.Header.Get("X-Robots-Tag"))
		disposition, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
		require.NoError(t, err)
		require.Equal(t, "inline", disposition)
		require.Equal(t, resource.ID+fixture.extension, parameters["filename"])
	}

	png := created.Resources[0]
	updated := runCLIResource(t, server, "--json", "image", "update", "--", png.ID, fixtures[3].path)
	require.Equal(t, png.URL, updated.Resource.URL)
	response, body := fetch(t, png.URL)
	require.Equal(t, fixtures[3].content, []byte(body))
	require.Equal(t, "image/webp", response.Header.Get("Content-Type"))
	_, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	require.NoError(t, err)
	require.Equal(t, png.ID+".webp", parameters["filename"])

	for _, resource := range created.Resources {
		runCLIDelete(t, server, "--json", "image", "delete", "--", resource.ID)
		response, _ := fetch(t, resource.URL)
		require.Equal(t, http.StatusNotFound, response.StatusCode)
	}
	requireCategoryEmpty(t, server.Root, "images")
}

func TestImageBatchValidationIsAtomic(t *testing.T) {
	server := startServer(t)
	valid := decodeImageFixtures(t)[0]
	invalid := writeFixture(t, "payload.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`))
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	stdout, stderr, err := server.RunCLI(ctx, "--json", "image", "upload", valid.path, invalid)
	require.Error(t, err)
	require.Empty(t, stdout)
	requireJSONError(t, stderr, "unsupported_media_type")
	requireCategoryEmpty(t, server.Root, "images")
}

func TestImageBatchPersistenceFailureRollsBackRealFilesystem(t *testing.T) {
	root := t.TempDir()
	storeContext, cancelStore := context.WithCancel(context.Background())
	t.Cleanup(cancelStore)
	filesystem, err := store.NewFS(store.Config{
		Root: root, CleanupInterval: time.Hour, Clock: common.SystemClock{}, Context: storeContext,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, filesystem.Close())
	})
	failingImages := &failAfterFirstImageCreate{delegate: filesystem.Images()}
	service, err := agentwb.New(agentwb.Config{
		WhiteboardStore:          filesystem.Whiteboards(),
		ImageStore:               failingImages,
		DefaultExpirationSeconds: 3600,
		CleanupInterval:          time.Hour,
		ShutdownTimeout:          time.Second,
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, service.Close())
	})
	httpServer := httptest.NewServer(service.Handler())
	t.Cleanup(httpServer.Close)

	fixtures := decodeImageFixtures(t)
	body, contentType := buildImageMultipart(t, fixtures[:2])
	requestContext, cancelRequest := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, httpServer.URL+"/api/v1/images", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", contentType)
	response, err := (&http.Client{Timeout: integrationTimeout}).Do(request)
	require.NoError(t, err)
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.JSONEq(t, `{"error":{"code":"storage_unavailable","message":"storage unavailable"}}`, string(responseBody))
	requireCategoryEmpty(t, root, "images")
}

func decodeImageFixtures(t *testing.T) []imageFixture {
	t.Helper()
	definitions := []struct {
		name, encoded, extension, mediaType string
	}{
		{name: "misleading-one.bin", encoded: png1x1Base64, extension: ".png", mediaType: "image/png"},
		{name: "misleading-two.bin", encoded: jpeg1x1Base64, extension: ".jpg", mediaType: "image/jpeg"},
		{name: "misleading-three.bin", encoded: gif1x1Base64, extension: ".gif", mediaType: "image/gif"},
		{name: "misleading-four.bin", encoded: webp1x1Base64, extension: ".webp", mediaType: "image/webp"},
	}
	directory := t.TempDir()
	fixtures := make([]imageFixture, 0, len(definitions))
	for _, definition := range definitions {
		content, err := base64.StdEncoding.DecodeString(definition.encoded)
		require.NoError(t, err)
		path := filepath.Join(directory, definition.name)
		require.NoError(t, os.WriteFile(path, content, 0o600))
		fixtures = append(fixtures, imageFixture{path: path, content: content, extension: definition.extension, mediaType: definition.mediaType})
	}
	return fixtures
}

func buildImageMultipart(t *testing.T, fixtures []imageFixture) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, fixture := range fixtures {
		part, err := writer.CreateFormFile("images", filepath.Base(fixture.path))
		require.NoError(t, err)
		_, err = part.Write(fixture.content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return body.Bytes(), writer.FormDataContentType()
}

type failAfterFirstImageCreate struct {
	delegate agentwb.ImageStore
	creates  int
}

func (store *failAfterFirstImageCreate) Create(ctx context.Context, image imageDomain.Image) error {
	store.creates++
	if store.creates > 1 {
		return common.NewError(common.CodeStorageUnavailable, "storage unavailable", errors.New("injected persistence failure"))
	}
	return store.delegate.Create(ctx, image)
}

func (store *failAfterFirstImageCreate) Get(ctx context.Context, id string) (imageDomain.Image, error) {
	return store.delegate.Get(ctx, id)
}

func (store *failAfterFirstImageCreate) Replace(ctx context.Context, image imageDomain.Image) error {
	return store.delegate.Replace(ctx, image)
}

func (store *failAfterFirstImageCreate) Delete(ctx context.Context, id string) error {
	return store.delegate.Delete(ctx, id)
}

func (store *failAfterFirstImageCreate) Ready(ctx context.Context) error {
	return store.delegate.Ready(ctx)
}

func (store *failAfterFirstImageCreate) Close() error {
	return store.delegate.Close()
}
