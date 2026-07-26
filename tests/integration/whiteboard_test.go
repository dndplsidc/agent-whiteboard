package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMarkdownLifecycle(t *testing.T) {
	server := startServer(t)
	configPath := writeConfigFixture(t, fmt.Sprintf("version: 1\nclient:\n  server: %s\n  timeout: 5s\n", server.URL))
	firstSource := "# First title\n\n`</script>` and unicode ✓\n"
	firstContext := "## Creator summary\n\nFirst goals, assumptions, and open questions. ✓\n"
	firstFile := writeFixture(t, "first.md", []byte(firstSource))
	firstContextFile := writeContextFixture(t, firstContext)
	created := runCLIResourceWithConfig(t, server, configPath, "--json", "create", "markdown", "--context", firstContextFile, firstFile)
	require.Equal(t, 1, created.SchemaVersion)
	require.True(t, strings.HasPrefix(created.Resource.URL, server.URL+"/whiteboards/markdown/"))

	got := runCLIMarkdownWithConfig(t, server, configPath, "--json", "get", "markdown", "--", created.Resource.ID)
	require.Equal(t, 1, got.SchemaVersion)
	require.Equal(t, created.Resource, got.Resource)
	require.Equal(t, firstSource, got.Markdown)
	require.Equal(t, firstContext, got.Context)

	response, body := fetch(t, created.Resource.URL)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "text/html; charset=utf-8", response.Header.Get("Content-Type"))
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	require.Equal(t, "noindex, nofollow, noarchive", response.Header.Get("X-Robots-Tag"))
	require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
	require.Contains(t, body, `<meta name="robots" content="noindex, nofollow, noarchive">`)
	require.Contains(t, body, "<style>")
	require.Contains(t, body, "<script>")
	encodedSource, err := json.Marshal(map[string]string{"markdown": firstSource})
	require.NoError(t, err)
	require.Contains(t, body, string(encodedSource))
	assertNoExternalReferences(t, body)

	secondSource := "# Updated\n\nExact replacement.\n"
	secondContext := "## Updated creator summary\n\nA new decision replaces the first context.\n"
	secondFile := writeFixture(t, "second.md", []byte(secondSource))
	secondContextFile := writeContextFixture(t, secondContext)
	updated := runCLIResourceWithConfig(t, server, configPath, "--json", "update", "markdown", "--context", secondContextFile, "--", created.Resource.ID, secondFile)
	require.Equal(t, created.Resource.URL, updated.Resource.URL)
	got = runCLIMarkdownWithConfig(t, server, configPath, "--json", "get", "markdown", "--", created.Resource.ID)
	require.Equal(t, secondSource, got.Markdown)
	require.Equal(t, secondContext, got.Context)
	_, body = fetch(t, created.Resource.URL)
	encodedSource, err = json.Marshal(map[string]string{"markdown": secondSource})
	require.NoError(t, err)
	require.Contains(t, body, string(encodedSource))
	require.NotContains(t, body, string(mustJSON(t, map[string]string{"markdown": firstSource})))

	deleteOutput := runCLIWithConfigSuccess(t, server, configPath, "--json", "delete", "markdown", "--", created.Resource.ID)
	require.JSONEq(t, `{"schema_version":1}`, deleteOutput)
	response, body = fetch(t, created.Resource.URL)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	requireHTTPErrorCode(t, body, "not_found")
	requireCategoryEmpty(t, server.Root, "whiteboards")
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	stdout, stderr, err := server.RunCLIWithConfig(ctx, configPath, "--json", "update", "markdown", "--context", secondContextFile, "--", created.Resource.ID, secondFile)
	require.Error(t, err)
	require.Empty(t, stdout)
	requireJSONError(t, stderr, "not_found")
}

func TestMarkdownDirectAPIPairLifecycle(t *testing.T) {
	server := startServer(t)
	firstSource := []byte("# API source\n\nExact bytes ✓\n")
	firstContext := []byte("# API context\n\nGoals and open questions ✓\n")
	created := requestMarkdownPair(t, http.MethodPost, server.URL+"/api/v1/whiteboards/markdown", firstSource, firstContext, http.StatusCreated)

	got := getMarkdownAPI(t, server.URL+"/api/v1/whiteboards/markdown/"+created.Resource.ID, http.StatusOK)
	require.Equal(t, created.Resource.ID, got.Resource.ID)
	require.Equal(t, string(firstSource), got.Markdown)
	require.Equal(t, string(firstContext), got.Context)

	secondSource := []byte("# API replacement\n")
	secondContext := []byte("# Replacement context\n")
	updated := requestMarkdownPair(t, http.MethodPut, server.URL+"/api/v1/whiteboards/markdown/"+created.Resource.ID, secondSource, secondContext, http.StatusOK)
	require.Equal(t, created.Resource.ID, updated.Resource.ID)
	got = getMarkdownAPI(t, server.URL+"/api/v1/whiteboards/markdown/"+created.Resource.ID, http.StatusOK)
	require.Equal(t, string(secondSource), got.Markdown)
	require.Equal(t, string(secondContext), got.Context)

	requestNoBody(t, http.MethodDelete, server.URL+"/api/v1/whiteboards/markdown/"+created.Resource.ID, http.StatusNoContent)
	missing := getMarkdownAPI(t, server.URL+"/api/v1/whiteboards/markdown/"+created.Resource.ID, http.StatusNotFound)
	require.Empty(t, missing.Markdown)
	requireCategoryEmpty(t, server.Root, "whiteboards")
}

func TestLegacyMarkdownReadMigratesOnFirstPairedUpdateAndDeletesBothArtifacts(t *testing.T) {
	server := startServer(t)
	const id = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	legacySource := []byte("# Legacy source\n")
	writeLegacyMarkdownFixture(t, server.Root, id, legacySource)

	got := getMarkdownAPI(t, server.URL+"/api/v1/whiteboards/markdown/"+id, http.StatusOK)
	require.Equal(t, string(legacySource), got.Markdown)
	require.Empty(t, got.Context)

	newSource := []byte("# Migrated source\n")
	newContext := []byte("# First creator context\n")
	requestMarkdownPair(t, http.MethodPut, server.URL+"/api/v1/whiteboards/markdown/"+id, newSource, newContext, http.StatusOK)
	got = getMarkdownAPI(t, server.URL+"/api/v1/whiteboards/markdown/"+id, http.StatusOK)
	require.Equal(t, string(newSource), got.Markdown)
	require.Equal(t, string(newContext), got.Context)

	resourceDir := filepath.Join(server.Root, "whiteboards", id)
	metadata := readMetadataFixture(t, resourceDir)
	require.Equal(t, float64(2), metadata["schema_version"])
	sourceName, sourceOK := metadata["content_filename"].(string)
	contextName, contextOK := metadata["context_filename"].(string)
	require.True(t, sourceOK)
	require.True(t, contextOK)
	require.Regexp(t, `^source-[a-f0-9]{32}\.md$`, sourceName)
	require.Regexp(t, `^context-[a-f0-9]{32}\.md$`, contextName)
	require.Equal(t, strings.TrimSuffix(strings.TrimPrefix(sourceName, "source-"), ".md"), strings.TrimSuffix(strings.TrimPrefix(contextName, "context-"), ".md"))
	require.NoFileExists(t, filepath.Join(resourceDir, "source-00000000000000000000000000000000.md"))
	entries, err := os.ReadDir(resourceDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	requestNoBody(t, http.MethodDelete, server.URL+"/api/v1/whiteboards/markdown/"+id, http.StatusNoContent)
	require.NoDirExists(t, resourceDir)
}

func TestHTMLLifecycleAndValidation(t *testing.T) {
	server := startServer(t)
	firstSource := []byte(`<!doctype html><html><head><style>body{color:#123}</style><script>window.inline=true</script></head><body><p>exact ✓</p></body></html>`)
	firstFile := writeFixture(t, "first.html", firstSource)
	created := runCLIResource(t, server, "--json", "create", "html", firstFile)
	require.True(t, strings.HasPrefix(created.Resource.URL, server.URL+"/whiteboards/html/"))

	response, body := fetch(t, created.Resource.URL)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, firstSource, []byte(body))
	require.Equal(t, "text/html; charset=utf-8", response.Header.Get("Content-Type"))
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	require.Equal(t, "noindex, nofollow, noarchive", response.Header.Get("X-Robots-Tag"))
	require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))

	secondSource := []byte(`<!doctype html><html><head><style>p{font-weight:bold}</style></head><body><script>window.updated=true</script><p>replacement</p></body></html>`)
	secondFile := writeFixture(t, "second.html", secondSource)
	updated := runCLIResource(t, server, "--json", "update", "html", "--", created.Resource.ID, secondFile)
	require.Equal(t, created.Resource.URL, updated.Resource.URL)
	_, body = fetch(t, created.Resource.URL)
	require.Equal(t, secondSource, []byte(body))

	runCLIDelete(t, server, "--json", "delete", "html", "--", created.Resource.ID)
	response, _ = fetch(t, created.Resource.URL)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	requireCategoryEmpty(t, server.Root, "whiteboards")

	invalid := []struct {
		name    string
		content []byte
	}{
		{name: "external script", content: []byte(`<!doctype html><html><head></head><body><script src="https://example.invalid/x.js"></script></body></html>`)},
		{name: "external stylesheet", content: []byte(`<!doctype html><html><head><link rel="stylesheet" href="https://example.invalid/x.css"></head><body></body></html>`)},
		{name: "missing doctype", content: []byte(`<html><head></head><body></body></html>`)},
		{name: "missing head", content: []byte(`<!doctype html><html><body></body></html>`)},
		{name: "invalid UTF-8", content: []byte{0xff, 0xfe}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			path := writeFixture(t, "invalid.html", test.content)
			ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
			defer cancel()
			stdout, stderr, err := server.RunCLI(ctx, "--json", "create", "html", path)
			require.Error(t, err)
			require.Empty(t, stdout)
			requireJSONError(t, stderr, "invalid_request")
			requireCategoryEmpty(t, server.Root, "whiteboards")
		})
	}
}

type apiResource struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type apiResourceEnvelope struct {
	Resource apiResource `json:"resource"`
}

type apiMarkdownEnvelope struct {
	Resource apiResource `json:"resource"`
	Markdown string      `json:"markdown"`
	Context  string      `json:"context"`
}

func requestMarkdownPair(t *testing.T, method, endpoint string, source, creatorContext []byte, wantStatus int) apiResourceEnvelope {
	t.Helper()
	body, contentType := deterministicMultipart(t, []multipartFile{
		{field: "file", name: "whiteboard.md", content: source},
		{field: "context", name: "context.md", content: creatorContext},
	})
	responseBody := requestAPI(t, method, endpoint, contentType, body, wantStatus)
	var envelope apiResourceEnvelope
	require.NoError(t, json.Unmarshal(responseBody, &envelope), string(responseBody))
	require.NotEmpty(t, envelope.Resource.ID)
	return envelope
}

func getMarkdownAPI(t *testing.T, endpoint string, wantStatus int) apiMarkdownEnvelope {
	t.Helper()
	responseBody := requestAPI(t, http.MethodGet, endpoint, "", nil, wantStatus)
	if wantStatus != http.StatusOK {
		requireHTTPErrorCode(t, string(responseBody), "not_found")
		return apiMarkdownEnvelope{}
	}
	var envelope apiMarkdownEnvelope
	require.NoError(t, json.Unmarshal(responseBody, &envelope), string(responseBody))
	return envelope
}

func requestNoBody(t *testing.T, method, endpoint string, wantStatus int) {
	t.Helper()
	responseBody := requestAPI(t, method, endpoint, "", nil, wantStatus)
	require.Empty(t, responseBody)
}

func requestAPI(t *testing.T, method, endpoint, contentType string, body []byte, wantStatus int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := (&http.Client{Timeout: integrationTimeout}).Do(request)
	require.NoError(t, err)
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, wantStatus, response.StatusCode, string(responseBody))
	if len(responseBody) > 0 {
		require.True(t, json.Valid(responseBody), "API output must contain JSON only: %q", responseBody)
	}
	return responseBody
}

func writeLegacyMarkdownFixture(t *testing.T, root, id string, source []byte) {
	t.Helper()
	resourceDir := filepath.Join(root, "whiteboards", id)
	require.NoError(t, os.Mkdir(resourceDir, 0o700))
	const sourceName = "source-00000000000000000000000000000000.md"
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, sourceName), source, 0o600))
	metadata := map[string]any{
		"schema_version":   1,
		"kind":             "markdown",
		"created_at":       map[string]any{"seconds": time.Now().Add(-time.Minute).Unix(), "nanoseconds": 0},
		"updated_at":       map[string]any{"seconds": time.Now().Add(-time.Minute).Unix(), "nanoseconds": 0},
		"expires_at":       nil,
		"content_filename": sourceName,
		"extension":        "",
		"media_type":       "",
	}
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, "metadata.json"), mustJSON(t, metadata), 0o600))
}

func readMetadataFixture(t *testing.T, resourceDir string) map[string]any {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(resourceDir, "metadata.json"))
	require.NoError(t, err)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(encoded, &metadata))
	return metadata
}

func assertNoExternalReferences(t *testing.T, body string) {
	t.Helper()
	require.NotRegexp(t, `(?i)(?:src|href)\s*=\s*["'](?:https?:)?//`, body)
	require.NotContains(t, strings.ToLower(body), "cdn.jsdelivr.net")
}

func requireHTTPErrorCode(t *testing.T, body, code string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &envelope), body)
	require.Equal(t, code, envelope.Error.Code)
}
