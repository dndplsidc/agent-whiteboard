package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/catalog"
	"github.com/stretchr/testify/require"
)

type cliCatalogRecord struct {
	SchemaVersion  int    `json:"schema_version"`
	Server         string `json:"server"`
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Summary        string `json:"summary"`
	SourceFilename string `json:"source_filename"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	ExpiresAt      *int64 `json:"expires_at"`
	Permanent      bool   `json:"permanent"`
	State          string `json:"state"`
	DeletedAt      *int64 `json:"deleted_at"`
	Expired        bool   `json:"expired"`
}

type cliCatalogEnvelope struct {
	SchemaVersion int                `json:"schema_version"`
	Records       []cliCatalogRecord `json:"records"`
	Total         int                `json:"total"`
	Limit         int                `json:"limit"`
	Offset        int                `json:"offset"`
}

func TestCatalogProcessLifecycleOfflineAcrossOrigins(t *testing.T) {
	firstServer := startServer(t)
	secondServer := startServer(t)
	clientHome := t.TempDir()
	clientEnv := isolatedEnv(clientHome)
	markdownSource := "# Rendered Markdown heading\n\nBody kept separate from catalog metadata.\n"
	markdownContext := "# Markdown creator context\n\nPrivate authoring notes.\n"
	htmlSource := "<!doctype html><html><head><title>Rendered HTML title</title></head><body><main>HTML body only</main></body></html>"
	htmlContext := "# HTML creator context\n\nPrivate HTML notes.\n"
	markdownPath := writeFixture(t, "catalog-markdown.md", []byte(markdownSource))
	markdownContextPath := writeFixture(t, "catalog-markdown-context.md", []byte(markdownContext))
	htmlPath := writeFixture(t, "catalog-page.html", []byte(htmlSource))
	htmlContextPath := writeFixture(t, "catalog-html-context.md", []byte(htmlContext))

	markdown := runCatalogResource(t, clientEnv, "--server", firstServer.URL, "--json", "create", "markdown", markdownPath, "--context", markdownContextPath, "--title", "Recharge architecture catalog title", "--summary", "Markdown searchable local record", "--expires-in", "0")
	html := runCatalogResource(t, clientEnv, "--server", secondServer.URL, "--json", "create", "html", htmlPath, "--context", htmlContextPath, "--title", "Standalone catalog title", "--summary", "HTML searchable local record", "--expires-in", "0")

	page := runCatalogList(t, clientEnv, "--json", "--config", filepath.Join(t.TempDir(), "invalid.yaml"), "--server", "not-an-origin", "catalog", "list", "--query", "searchable local", "--limit", "20")
	require.Equal(t, 2, page.Total)
	require.ElementsMatch(t, []string{markdown.Resource.ID, html.Resource.ID}, []string{page.Records[0].ID, page.Records[1].ID})
	byID := map[string]cliCatalogRecord{page.Records[0].ID: page.Records[0], page.Records[1].ID: page.Records[1]}
	require.Equal(t, firstServer.URL, byID[markdown.Resource.ID].Server)
	require.Equal(t, secondServer.URL, byID[html.Resource.ID].Server)
	require.Equal(t, "catalog-markdown.md", byID[markdown.Resource.ID].SourceFilename)
	require.Equal(t, "catalog-page.html", byID[html.Resource.ID].SourceFilename)

	filtered := runCatalogList(t, clientEnv, "--json", "catalog", "list", "--kind", "html", "--query", "standalone record", "--limit", "1", "--offset", "0")
	require.Equal(t, 1, filtered.Total)
	require.Equal(t, html.Resource.ID, filtered.Records[0].ID)
	pageOne := runCatalogList(t, clientEnv, "--json", "catalog", "list", "--limit", "1", "--offset", "0")
	pageTwo := runCatalogList(t, clientEnv, "--json", "catalog", "list", "--limit", "1", "--offset", "1")
	require.Equal(t, 2, pageOne.Total)
	require.Equal(t, 2, pageTwo.Total)
	require.NotEqual(t, pageOne.Records[0].ID, pageTwo.Records[0].ID)

	markdownGot := runCatalogMarkdown(t, clientEnv, "--server", byID[markdown.Resource.ID].Server, "--json", "get", "markdown", "--", markdown.Resource.ID)
	require.Equal(t, markdownSource, markdownGot.Markdown)
	require.Equal(t, markdownContext, markdownGot.Context)
	htmlGot := runCatalogHTML(t, clientEnv, "--server", byID[html.Resource.ID].Server, "--json", "get", "html", "--", html.Resource.ID)
	require.Equal(t, htmlSource, htmlGot.HTML)
	require.Equal(t, htmlContext, htmlGot.Context)

	updatedSource := "# Updated rendered heading\n\nReplacement body.\n"
	updatedPath := writeFixture(t, "updated-catalog.md", []byte(updatedSource))
	updatedContextPath := writeFixture(t, "updated-catalog-context.md", []byte("Updated creator context\n"))
	updated := runCatalogResource(t, clientEnv, "--server", firstServer.URL, "--json", "update", "markdown", "--context", updatedContextPath, "--title", "Updated catalog title", "--", markdown.Resource.ID, updatedPath)
	require.Equal(t, markdown.Resource.URL, updated.Resource.URL)
	updatedPage := runCatalogList(t, clientEnv, "--json", "catalog", "list", "--query", "updated searchable", "--limit", "20")
	require.Equal(t, 1, updatedPage.Total)
	require.Equal(t, "Updated catalog title", updatedPage.Records[0].Title)
	require.Equal(t, "Markdown searchable local record", updatedPage.Records[0].Summary)
	require.Equal(t, byID[markdown.Resource.ID].CreatedAt, updatedPage.Records[0].CreatedAt)

	runCatalogSuccess(t, clientEnv, "--server", firstServer.URL, "--json", "delete", "markdown", "--", markdown.Resource.ID)
	deletedPage := runCatalogList(t, clientEnv, "--json", "catalog", "list", "--query", "updated catalog", "--limit", "20")
	require.Equal(t, 1, deletedPage.Total)
	require.Equal(t, "deleted", deletedPage.Records[0].State)
	require.NotNil(t, deletedPage.Records[0].DeletedAt)
	response, _ := fetch(t, markdown.Resource.URL)
	require.Equal(t, http.StatusNotFound, response.StatusCode)

	secondHomePage := runCatalogList(t, isolatedEnv(t.TempDir()), "--json", "catalog", "list")
	require.Zero(t, secondHomePage.Total)
	require.Empty(t, secondHomePage.Records)

	secondServer.Signal(t, syscall.SIGTERM)
	require.NoError(t, secondServer.Wait(t))
	offline := runCatalogList(t, clientEnv, "--json", "--server", secondServer.URL, "catalog", "list", "--query", "standalone", "--limit", "20")
	require.Equal(t, 1, offline.Total)
	require.Equal(t, html.Resource.ID, offline.Records[0].ID)
}

func TestCatalogProcessPreflightFailureSendsNoCreateRequest(t *testing.T) {
	requests := make(chan struct{}, 1)
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests <- struct{}{} }))
	defer remote.Close()
	home := t.TempDir()
	base := filepath.Join(home, ".agent-whiteboard")
	require.NoError(t, os.Mkdir(base, 0o700))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(base, "catalog")))
	path := writeFixture(t, "preflight.md", []byte("# preflight\n"))
	contextPath := writeContextFixture(t, "preflight context")

	stdout, stderr, err := runCatalogCommand(t, isolatedEnv(home), "--server", remote.URL, "--json", "create", "markdown", path, "--context", contextPath, "--title", "Preflight", "--summary", "Must not publish")
	require.Error(t, err)
	require.Empty(t, stdout)
	envelope := requireJSONError(t, stderr, "catalog_unavailable")
	require.Contains(t, envelope.Error.Message, "no whiteboard was published")
	select {
	case <-requests:
		require.Fail(t, "preflight failure sent an HTTP request")
	default:
	}
}

func TestCatalogProcessPostResponseWriteFailureWarnsAndSucceeds(t *testing.T) {
	const id = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	received := make(chan struct{})
	release := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		close(received)
		<-release
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		fmt.Fprintf(response, `{"resource":{"id":%q,"type":"markdown","path":%q,"expires_at":null,"permanent":true}}`, id, "/whiteboards/markdown/"+id)
	}))
	defer remote.Close()
	home := t.TempDir()
	env := isolatedEnv(home)
	source := writeFixture(t, "write-failure.md", []byte("# body\n"))
	contextPath := writeContextFixture(t, "context")
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, "--server", remote.URL, "--json", "create", "markdown", source, "--context", contextPath, "--title", "Published", "--summary", "Catalog write fails")
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	require.NoError(t, command.Start())
	select {
	case <-received:
	case <-ctx.Done():
		require.FailNow(t, "create request did not reach responder", ctx.Err())
	}
	kindDirectory := filepath.Join(home, ".agent-whiteboard", "catalog", "entries", catalog.ServerKey(remote.URL), "markdown")
	require.NoError(t, os.MkdirAll(kindDirectory, 0o700))
	require.NoError(t, os.Chmod(kindDirectory, 0o500))
	close(release)
	err := command.Wait()
	require.NoError(t, err, stderr.String())
	require.NoError(t, os.Chmod(kindDirectory, 0o700))
	require.Contains(t, stdout.String(), id)
	var warning struct {
		Warning struct {
			Code string `json:"code"`
		} `json:"warning"`
	}
	require.NoError(t, json.Unmarshal(stderr.Bytes(), &warning), stderr.String())
	require.Equal(t, "catalog_write_failed", warning.Warning.Code)
}

func TestCatalogProcessUncertainCreateIsRecordedWithoutExtraWarning(t *testing.T) {
	const id = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(response, `{"error":{"code":"storage_unavailable","message":"storage unavailable"},"resource":{"id":%q,"type":"markdown","path":%q,"expires_at":null,"permanent":true}}`, id, "/whiteboards/markdown/"+id)
	}))
	defer remote.Close()
	home := t.TempDir()
	env := isolatedEnv(home)
	stdout, stderr, err := runCatalogCommand(t, env, "--server", remote.URL, "--json", "create", "markdown", writeFixture(t, "uncertain.md", []byte("# uncertain\n")), "--context", writeContextFixture(t, "context"), "--title", "Uncertain board", "--summary", "Returned with a remote error")
	require.Error(t, err)
	var exitError *exec.ExitError
	require.ErrorAs(t, err, &exitError)
	require.Equal(t, 3, exitError.ExitCode())
	require.Contains(t, stdout, id)
	require.NotContains(t, stderr, "warning")
	requireJSONError(t, stderr, "storage_unavailable")
	page := runCatalogList(t, env, "--json", "catalog", "list", "--query", "uncertain board")
	require.Equal(t, 1, page.Total)
	require.Equal(t, "creation_uncertain", page.Records[0].State)
}

func TestCatalogProcessUncertainCreateWriteFailureOrdersWarningBeforeError(t *testing.T) {
	const id = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	received := make(chan struct{})
	release := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		close(received)
		<-release
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(response, `{"error":{"code":"storage_unavailable","message":"storage unavailable"},"resource":{"id":%q,"type":"markdown","path":%q,"expires_at":null,"permanent":true}}`, id, "/whiteboards/markdown/"+id)
	}))
	defer remote.Close()
	home := t.TempDir()
	env := isolatedEnv(home)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, "--server", remote.URL, "--json", "create", "markdown", writeFixture(t, "uncertain-write-failure.md", []byte("# uncertain\n")), "--context", writeContextFixture(t, "context"), "--title", "Uncertain", "--summary", "Write will fail")
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Start())
	select {
	case <-received:
	case <-ctx.Done():
		require.FailNow(t, "create request did not reach responder", ctx.Err())
	}
	kindDirectory := filepath.Join(home, ".agent-whiteboard", "catalog", "entries", catalog.ServerKey(remote.URL), "markdown")
	require.NoError(t, os.MkdirAll(kindDirectory, 0o700))
	require.NoError(t, os.Chmod(kindDirectory, 0o500))
	close(release)
	err := command.Wait()
	require.NoError(t, os.Chmod(kindDirectory, 0o700))
	var exitError *exec.ExitError
	require.ErrorAs(t, err, &exitError)
	require.Equal(t, 3, exitError.ExitCode())
	require.Contains(t, stdout.String(), id)
	lines := bytes.Split(bytes.TrimSpace(stderr.Bytes()), []byte("\n"))
	require.Len(t, lines, 2, stderr.String())
	var warning struct {
		Warning struct {
			Code string `json:"code"`
		} `json:"warning"`
	}
	require.NoError(t, json.Unmarshal(lines[0], &warning), string(lines[0]))
	require.Equal(t, "catalog_write_failed", warning.Warning.Code)
	requireJSONError(t, string(lines[1]), "storage_unavailable")
	page := runCatalogList(t, env, "--json", "catalog", "list")
	require.Zero(t, page.Total)
}

func TestCatalogProcessImagesAndUnknownBoardsAreNotEnrolled(t *testing.T) {
	server := startServer(t)
	home := t.TempDir()
	env := isolatedEnv(home)
	imagePath := writeFixture(t, "catalog-image.png", mustDecodeBase64(t, png1x1Base64))
	runCatalogSuccess(t, env, "--server", server.URL, "--json", "image", "upload", imagePath)
	created := requestMarkdownPair(t, http.MethodPost, server.URL+"/api/v1/whiteboards/markdown", []byte("# API board\n"), []byte("context"), http.StatusCreated)
	updatedPath := writeFixture(t, "unknown-update.md", []byte("# updated\n"))
	stdout, stderr, err := runCatalogCommand(t, env, "--server", server.URL, "--json", "update", "markdown", "--context", writeContextFixture(t, "updated context"), "--title", "Not enrolled", "--", created.Resource.ID, updatedPath)
	require.NoError(t, err, stderr)
	require.Contains(t, stdout, created.Resource.ID)
	var warning struct {
		Warning struct {
			Code string `json:"code"`
		} `json:"warning"`
	}
	require.NoError(t, json.Unmarshal([]byte(stderr), &warning), stderr)
	require.Equal(t, "catalog_record_missing", warning.Warning.Code)
	page := runCatalogList(t, env, "--json", "catalog", "list")
	require.Zero(t, page.Total)
	runCatalogSuccess(t, env, "--server", server.URL, "--json", "delete", "markdown", "--", created.Resource.ID)
	page = runCatalogList(t, env, "--json", "catalog", "list")
	require.Zero(t, page.Total)
}

func TestCatalogProcessSerializesTrackedMutationAcrossRemoteRequest(t *testing.T) {
	const id = "-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	updateReceived := make(chan struct{})
	releaseUpdate := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseUpdate) })
	deleteReceived := make(chan struct{}, 2)
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
		resourceBody := fmt.Sprintf(`{"resource":{"id":%q,"type":"markdown","path":%q,"expires_at":null,"permanent":true}}`, id, "/whiteboards/markdown/"+id)
		switch request.Method {
		case http.MethodPost:
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(resourceBody))
		case http.MethodPut:
			close(updateReceived)
			<-releaseUpdate
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(resourceBody))
		case http.MethodDelete:
			deleteReceived <- struct{}{}
			response.WriteHeader(http.StatusNoContent)
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer remote.Close()
	home := t.TempDir()
	env := isolatedEnv(home)
	source := writeFixture(t, "serial-create.md", []byte("# created\n"))
	contextPath := writeContextFixture(t, "created context")
	runCatalogResource(t, env, "--server", remote.URL, "--json", "create", "markdown", source, "--context", contextPath, "--title", "Serialized board", "--summary", "Cross-process mutation ordering")

	updatedSource := writeFixture(t, "serial-update.md", []byte("# updated\n"))
	updatedContext := writeContextFixture(t, "updated context")
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	updateCommand := exec.CommandContext(ctx, binaryPath, "--server", remote.URL, "--json", "update", "markdown", "--context", updatedContext, "--title", "Updated while locked", "--", id, updatedSource)
	updateCommand.Env = env
	var updateStdout, updateStderr bytes.Buffer
	updateCommand.Stdout = &updateStdout
	updateCommand.Stderr = &updateStderr
	require.NoError(t, updateCommand.Start())
	select {
	case <-updateReceived:
	case <-ctx.Done():
		require.FailNow(t, "tracked update did not reach responder", ctx.Err())
	}

	deleteStdout, deleteStderr, deleteErr := runCatalogCommand(t, env, "--server", remote.URL, "--timeout", "100ms", "--json", "delete", "markdown", "--", id)
	require.Error(t, deleteErr)
	require.Empty(t, deleteStdout)
	requireJSONError(t, deleteStderr, "timeout")
	select {
	case <-deleteReceived:
		require.Fail(t, "delete reached the server while the tracked update held the record lock")
	default:
	}

	releaseOnce.Do(func() { close(releaseUpdate) })
	require.NoError(t, updateCommand.Wait(), updateStderr.String())
	require.Empty(t, updateStderr.String())
	require.Contains(t, updateStdout.String(), id)
	runCatalogSuccess(t, env, "--server", remote.URL, "--json", "delete", "markdown", "--", id)
	select {
	case <-deleteReceived:
	case <-ctx.Done():
		require.FailNow(t, "delete did not reach responder after update released the lock", ctx.Err())
	}
	page := runCatalogList(t, env, "--json", "catalog", "list", "--query", "updated locked")
	require.Equal(t, 1, page.Total)
	require.Equal(t, "deleted", page.Records[0].State)
}

func runCatalogResource(t *testing.T, env []string, args ...string) cliResourceEnvelope {
	t.Helper()
	stdout := runCatalogSuccess(t, env, args...)
	var envelope cliResourceEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	return envelope
}

func runCatalogList(t *testing.T, env []string, args ...string) cliCatalogEnvelope {
	t.Helper()
	stdout := runCatalogSuccess(t, env, args...)
	var envelope cliCatalogEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	require.Equal(t, 1, envelope.SchemaVersion)
	if envelope.Records == nil {
		envelope.Records = []cliCatalogRecord{}
	}
	return envelope
}

func runCatalogMarkdown(t *testing.T, env []string, args ...string) cliMarkdownEnvelope {
	t.Helper()
	stdout := runCatalogSuccess(t, env, args...)
	var envelope cliMarkdownEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	return envelope
}

func runCatalogHTML(t *testing.T, env []string, args ...string) apiHTMLEnvelope {
	t.Helper()
	stdout := runCatalogSuccess(t, env, args...)
	var envelope apiHTMLEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	return envelope
}

func runCatalogSuccess(t *testing.T, env []string, args ...string) string {
	t.Helper()
	stdout, stderr, err := runCatalogCommand(t, env, args...)
	require.NoError(t, err, "stderr: %s", stderr)
	require.Empty(t, stderr)
	return stdout
}

func runCatalogCommand(t *testing.T, env []string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, args...)
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}
