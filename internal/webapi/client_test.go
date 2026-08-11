package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	httpx "github.com/edocsss/agent-whiteboard/internal/webapi"
	"github.com/stretchr/testify/require"
)

const clientTestID = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3"
const clientSecondTestID = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"

func TestNewClientAcceptsOnlyHTTPOrigins(t *testing.T) {
	t.Parallel()

	client := &http.Client{}
	for _, server := range []string{"http://example.test", "https://example.test:8443", "http://example.test/"} {
		server := server
		t.Run("accepts "+server, func(t *testing.T) {
			t.Parallel()
			got, err := httpx.NewClient(httpx.ClientConfig{Server: server, HTTPClient: client})
			require.NoError(t, err)
			require.NotNil(t, got)
		})
	}

	invalid := []string{
		"", "example.test", "/local", "ftp://example.test", "http://", "http:///missing-host",
		"http://user@example.test", "http://example.test/api", "http://example.test//",
		"http://example.test?debug=1", "http://example.test/#fragment", "http://example.test#",
	}
	for _, server := range invalid {
		server := server
		t.Run("rejects "+server, func(t *testing.T) {
			t.Parallel()
			got, err := httpx.NewClient(httpx.ClientConfig{Server: server, HTTPClient: client})
			require.Error(t, err)
			require.Nil(t, got)
		})
	}

	got, err := httpx.NewClient(httpx.ClientConfig{Server: "http://example.test"})
	require.Error(t, err)
	require.Nil(t, got)
}

func TestClientRejectsUnsupportedWhiteboardKindBeforeTransport(t *testing.T) {
	t.Parallel()

	transportCalled := false
	client := newTestClient(t, "http://example.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		transportCalled = true
		return nil, errors.New("unexpected transport call")
	})})

	_, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardKind("text"), httpx.File{
		Name: "board.txt", Reader: strings.NewReader("body"),
	}, nil)
	require.Error(t, err)
	require.False(t, transportCalled)
}

func TestClientRejectsTypedNilReadersBeforeTransport(t *testing.T) {
	t.Parallel()

	var transportCalls atomic.Int32
	client := newTestClient(t, "http://example.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		return nil, errors.New("unexpected transport call")
	})})
	var reader *nilSafeReader
	file := httpx.File{Name: "typed-nil", Reader: reader}

	tests := []struct {
		name   string
		invoke func() error
	}{
		{name: "create whiteboard", invoke: func() error {
			_, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, file, nil)
			return err
		}},
		{name: "update whiteboard", invoke: func() error {
			_, err := client.UpdateWhiteboard(context.Background(), httpx.WhiteboardHTML, clientTestID, file, nil)
			return err
		}},
		{name: "create images", invoke: func() error {
			_, err := client.CreateImages(context.Background(), []httpx.File{file}, nil)
			return err
		}},
		{name: "update image", invoke: func() error {
			_, err := client.UpdateImage(context.Background(), clientTestID, file, nil)
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var protocolErr *common.Error
			require.ErrorAs(t, test.invoke(), &protocolErr)
			require.Equal(t, common.CodeInvalidRequest, protocolErr.Code)
		})
	}
	require.Zero(t, transportCalls.Load())
}

func TestClientRejectsUnpairedMarkdownBeforeTransport(t *testing.T) {
	t.Parallel()

	var transportCalls atomic.Int32
	client := newTestClient(t, "http://example.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		return nil, errors.New("unexpected transport call")
	})})

	_, createErr := client.CreateWhiteboard(context.Background(), httpx.WhiteboardMarkdown, httpx.File{
		Name: "board.md", Reader: strings.NewReader("source"),
	}, nil)
	_, updateErr := client.UpdateWhiteboard(context.Background(), httpx.WhiteboardMarkdown, clientTestID, httpx.File{
		Name: "board.md", Reader: strings.NewReader("source"),
	}, nil)
	for _, err := range []error{createErr, updateErr} {
		var protocolErr *common.Error
		require.ErrorAs(t, err, &protocolErr)
		require.Equal(t, common.CodeInvalidRequest, protocolErr.Code)
		require.Equal(t, "markdown context is required", protocolErr.Message)
	}
	require.Zero(t, transportCalls.Load())
}

func TestClientRejectsInvalidMarkdownPairBeforeTransport(t *testing.T) {
	t.Parallel()

	var transportCalls atomic.Int32
	client := newTestClient(t, "http://example.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		return nil, errors.New("unexpected transport call")
	})})
	var nilReader *nilSafeReader

	tests := []struct {
		name    string
		source  httpx.File
		context httpx.File
	}{
		{name: "missing source reader", source: httpx.File{Name: "source.md", Reader: nilReader}, context: httpx.File{Name: "context.md", Reader: strings.NewReader("context")}},
		{name: "missing context reader", source: httpx.File{Name: "source.md", Reader: strings.NewReader("source")}, context: httpx.File{Name: "context.md", Reader: nilReader}},
		{name: "missing context name", source: httpx.File{Name: "source.md", Reader: strings.NewReader("source")}, context: httpx.File{Reader: strings.NewReader("context")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.CreateMarkdown(context.Background(), test.source, test.context, nil)
			var protocolErr *common.Error
			require.ErrorAs(t, err, &protocolErr)
			require.Equal(t, common.CodeInvalidRequest, protocolErr.Code)
		})
	}
	require.Zero(t, transportCalls.Load())
}

func TestClientContainsReaderPanicAsPrivateStableError(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, request.Body)
		return nil, err
	})
	client := newTestClient(t, "http://example.test", &http.Client{Transport: transport})

	_, err := client.CreateMarkdown(context.Background(), httpx.File{
		Name: "panic.md", Reader: panicReader{},
	}, httpx.File{Name: "context.md", Reader: strings.NewReader("context")}, nil)
	var protocolErr *common.Error
	require.ErrorAs(t, err, &protocolErr)
	require.Equal(t, common.CodeInternal, protocolErr.Code)
	require.Equal(t, "could not stream request body", protocolErr.Message)
	require.NotContains(t, err.Error(), "secret panic payload")
}

func TestClientDoesNotFollowMutationRedirectsOrMutateCaller(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"multipart", "delete"} {
		operation := operation
		for _, destination := range []string{"same origin", "cross origin"} {
			destination := destination
			t.Run(operation+" "+destination, func(t *testing.T) {
				t.Parallel()

				var targetCalls atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					targetCalls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				t.Cleanup(target.Close)

				var source *httptest.Server
				source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirect-target" {
						targetCalls.Add(1)
						w.WriteHeader(http.StatusNoContent)
						return
					}
					location := target.URL + "/secret-target"
					if destination == "same origin" {
						location = source.URL + "/redirect-target?secret=private"
					}
					http.Redirect(w, r, location, http.StatusFound)
				}))
				t.Cleanup(source.Close)

				callerHookErr := errors.New("caller redirect hook")
				var callerHookCalls atomic.Int32
				callerClient := source.Client()
				callerClient.CheckRedirect = func(*http.Request, []*http.Request) error {
					callerHookCalls.Add(1)
					return callerHookErr
				}
				client := newTestClient(t, source.URL, callerClient)

				var err error
				if operation == "multipart" {
					_, err = client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
						Name: "board.html", Reader: strings.NewReader("body"),
					}, nil)
				} else {
					err = client.DeleteImage(context.Background(), clientTestID)
				}

				var protocolErr *common.Error
				require.ErrorAs(t, err, &protocolErr)
				require.Equal(t, common.CodeInternal, protocolErr.Code)
				require.Equal(t, "server returned an invalid error response", protocolErr.Message)
				require.NotContains(t, err.Error(), "secret")
				require.Zero(t, targetCalls.Load())
				require.Zero(t, callerHookCalls.Load())
				require.ErrorIs(t, callerClient.CheckRedirect(nil, nil), callerHookErr)
				require.Equal(t, int32(1), callerHookCalls.Load())
			})
		}
	}
}

func TestClientWhiteboardMutationsUseExactProtocol(t *testing.T) {
	t.Parallel()

	type expectation struct {
		method     string
		path       string
		field      string
		filename   string
		content    string
		expiration *string
		status     int
	}
	expectations := make(chan expectation, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expect := <-expectations
		require.Equal(t, expect.method, r.Method)
		require.Equal(t, expect.path, r.URL.EscapedPath())
		if expect.field == "" {
			require.Empty(t, r.Header.Get("Content-Type"))
		} else {
			parts := readMultipartParts(t, r)
			require.Equal(t, []multipartPart{{
				Field: expect.field, Filename: expect.filename, Content: expect.content,
			}}, fileParts(parts))
			require.Equal(t, expect.expiration, expirationPart(parts))
		}
		if expect.status == http.StatusNoContent {
			w.WriteHeader(expect.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(expect.status)
		_ = json.NewEncoder(w).Encode(httpx.ResourceResponse{Resource: httpx.Resource{
			ID: clientTestID, Type: "markdown", Path: httpx.PublicMarkdown + clientTestID,
			Permanent: true,
		}})
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	expectations <- expectation{
		method: http.MethodPost, path: httpx.APIWhiteboardHTML, field: "file",
		filename: "board.html", content: "<!doctype html>", status: http.StatusCreated,
	}
	created, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
		Name: "board.html", Reader: strings.NewReader("<!doctype html>"),
	}, nil)
	require.NoError(t, err)
	require.Equal(t, clientTestID, created.ID)

	zero := int64(0)
	zeroText := "0"
	expectations <- expectation{
		method: http.MethodPut, path: httpx.APIWhiteboardHTML + "/" + clientTestID, field: "file",
		filename: "board.html", content: "<!doctype html>", expiration: &zeroText, status: http.StatusOK,
	}
	updated, err := client.UpdateWhiteboard(context.Background(), httpx.WhiteboardHTML, clientTestID, httpx.File{
		Name: "board.html", Reader: strings.NewReader("<!doctype html>"),
	}, &zero)
	require.NoError(t, err)
	require.Equal(t, clientTestID, updated.ID)

	expectations <- expectation{
		method: http.MethodDelete, path: httpx.APIWhiteboardMarkdown + "/" + clientTestID,
		status: http.StatusNoContent,
	}
	require.NoError(t, client.DeleteWhiteboard(context.Background(), httpx.WhiteboardMarkdown, clientTestID))
}

func TestClientMarkdownMutationsSendExactFileContextPair(t *testing.T) {
	t.Parallel()

	type expectation struct {
		method     string
		path       string
		expiration *string
	}
	expectations := make(chan expectation, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expect := <-expectations
		require.Equal(t, expect.method, r.Method)
		require.Equal(t, expect.path, r.URL.EscapedPath())
		parts := readMultipartParts(t, r)
		require.Equal(t, expect.expiration, expirationPart(parts))

		files := fileParts(parts)
		require.Len(t, files, 2)
		byField := make(map[string]multipartPart, len(files))
		for _, part := range files {
			require.NotContains(t, byField, part.Field)
			byField[part.Field] = part
		}
		require.Equal(t, map[string]multipartPart{
			"file":    {Field: "file", Filename: "board.md", Content: "# document\x00bytes"},
			"context": {Field: "context", Filename: "creator.md", Content: "goals\ndecisions\x00"},
		}, byField)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(map[string]int{http.MethodPost: http.StatusCreated, http.MethodPut: http.StatusOK}[expect.method])
		_ = json.NewEncoder(w).Encode(httpx.ResourceResponse{Resource: httpx.Resource{
			ID: clientTestID, Type: "markdown", Path: httpx.PublicMarkdown + clientTestID, Permanent: true,
		}})
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())
	markdown := httpx.File{Name: "board.md", Reader: strings.NewReader("# document\x00bytes")}
	creatorContext := httpx.File{Name: "creator.md", Reader: strings.NewReader("goals\ndecisions\x00")}

	expectations <- expectation{method: http.MethodPost, path: httpx.APIWhiteboardMarkdown}
	created, err := client.CreateMarkdown(context.Background(), markdown, creatorContext, nil)
	require.NoError(t, err)
	require.Equal(t, clientTestID, created.ID)

	expires := int64(30)
	expiresText := "30"
	expectations <- expectation{
		method: http.MethodPut, path: httpx.APIWhiteboardMarkdown + "/" + clientTestID, expiration: &expiresText,
	}
	updated, err := client.UpdateMarkdown(context.Background(), clientTestID,
		httpx.File{Name: "board.md", Reader: strings.NewReader("# document\x00bytes")},
		httpx.File{Name: "creator.md", Reader: strings.NewReader("goals\ndecisions\x00")},
		&expires,
	)
	require.NoError(t, err)
	require.Equal(t, clientTestID, updated.ID)
}

func TestClientHTMLMutationsRemainSingleFileMultipart(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, httpx.APIWhiteboardHTML, r.URL.EscapedPath())
		require.Equal(t, []multipartPart{{Field: "file", Filename: "board.html", Content: "<!doctype html>"}}, fileParts(readMultipartParts(t, r)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(httpx.ResourceResponse{Resource: httpx.Resource{
			ID: clientTestID, Type: "html", Path: httpx.PublicHTML + clientTestID, Permanent: true,
		}})
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	_, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
		Name: "board.html", Reader: strings.NewReader("<!doctype html>"),
	}, nil)
	require.NoError(t, err)
}

func TestClientImageMutationsPreserveOrderAndExpiration(t *testing.T) {
	t.Parallel()

	type expectation struct {
		method     string
		path       string
		files      []multipartPart
		expiration *string
		status     int
		many       bool
	}
	expectations := make(chan expectation, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expect := <-expectations
		require.Equal(t, expect.method, r.Method)
		require.Equal(t, expect.path, r.URL.EscapedPath())
		if len(expect.files) > 0 {
			parts := readMultipartParts(t, r)
			require.Equal(t, expect.files, fileParts(parts))
			require.Equal(t, expect.expiration, expirationPart(parts))
		}
		if expect.status == http.StatusNoContent {
			w.WriteHeader(expect.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(expect.status)
		resource := httpx.Resource{ID: clientTestID, Path: httpx.PublicImages + clientTestID, Permanent: true}
		if expect.many {
			_ = json.NewEncoder(w).Encode(httpx.ImagesResponse{Images: []httpx.Resource{
				resource,
				{ID: clientSecondTestID, Path: httpx.PublicImages + clientSecondTestID, Permanent: true},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(httpx.ResourceResponse{Resource: resource})
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	zero := int64(0)
	zeroText := "0"
	expectations <- expectation{
		method: http.MethodPost, path: httpx.APIImages, status: http.StatusCreated, many: true,
		files: []multipartPart{
			{Field: "images", Filename: "first.png", Content: "first"},
			{Field: "images", Filename: "second.jpg", Content: "second"},
		},
		expiration: &zeroText,
	}
	created, err := client.CreateImages(context.Background(), []httpx.File{
		{Name: "first.png", Reader: strings.NewReader("first")},
		{Name: "second.jpg", Reader: strings.NewReader("second")},
	}, &zero)
	require.NoError(t, err)
	require.Equal(t, []string{clientTestID, clientSecondTestID}, []string{created[0].ID, created[1].ID})

	positive := int64(86400)
	positiveText := "86400"
	expectations <- expectation{
		method: http.MethodPut, path: httpx.APIImages + "/" + clientTestID, status: http.StatusOK,
		files:      []multipartPart{{Field: "file", Filename: "replacement.gif", Content: "replacement"}},
		expiration: &positiveText,
	}
	updated, err := client.UpdateImage(context.Background(), clientTestID, httpx.File{
		Name: "replacement.gif", Reader: strings.NewReader("replacement"),
	}, &positive)
	require.NoError(t, err)
	require.Equal(t, clientTestID, updated.ID)

	expectations <- expectation{method: http.MethodDelete, path: httpx.APIImages + "/" + clientTestID, status: http.StatusNoContent}
	require.NoError(t, client.DeleteImage(context.Background(), clientTestID))
}

func TestClientPreservesContextCancellationError(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	transportCanceled := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		require.NoError(t, r.Body.Close())
		close(requestStarted)
		<-r.Context().Done()
		transportCanceled <- r.Context().Err()
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.CreateMarkdown(ctx, httpx.File{
			Name: "blocked.md", Reader: strings.NewReader("body"),
		}, httpx.File{Name: "context.md", Reader: strings.NewReader("context")}, nil)
		result <- err
	}()
	<-requestStarted
	cancel()
	select {
	case err := <-result:
		require.True(t, err == context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("client did not return after cancellation")
	}
	select {
	case err := <-transportCanceled:
		require.True(t, err == context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach transport")
	}
}

func TestClientPreservesContextDeadlineError(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := newTestClient(t, "http://example.test", &http.Client{Transport: transport})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	err := client.DeleteImage(ctx, clientTestID)
	require.True(t, err == context.DeadlineExceeded)
}

func TestClientPreservesContextErrorsWhileReadingResponse(t *testing.T) {
	t.Parallel()

	for _, contextErr := range []error{context.Canceled, context.DeadlineExceeded} {
		contextErr := contextErr
		t.Run(contextErr.Error(), func(t *testing.T) {
			t.Parallel()
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Header:     make(http.Header),
					Body:       readCloser{Reader: errorReader{err: contextErr}},
					Request:    request,
				}, nil
			})
			client := newTestClient(t, "http://example.test", &http.Client{Transport: transport})

			err := client.DeleteImage(context.Background(), clientTestID)
			require.True(t, err == contextErr)
		})
	}
}

func TestClientDecodesStableErrorsAndHidesUnknownBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		body     string
		wantCode common.ErrorCode
		wantMsg  string
		hidden   string
	}{
		{
			name: "protocol error", status: http.StatusNotFound,
			body:     `{"error":{"code":"not_found","message":"resource not found"}}`,
			wantCode: common.CodeNotFound, wantMsg: "resource not found",
		},
		{
			name: "unknown body", status: http.StatusBadGateway,
			body: "upstream password=secret", wantCode: common.CodeInternal,
			wantMsg: "server returned an invalid error response", hidden: "password",
		},
		{
			name: "unknown code", status: http.StatusBadRequest,
			body:     `{"error":{"code":"private_code","message":"secret database path"}}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid error response", hidden: "database",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, server.Client())

			err := client.DeleteImage(context.Background(), clientTestID)
			var protocolErr *common.Error
			require.ErrorAs(t, err, &protocolErr)
			require.Equal(t, test.wantCode, protocolErr.Code)
			require.Equal(t, test.wantMsg, protocolErr.Message)
			if test.hidden != "" {
				require.NotContains(t, err.Error(), test.hidden)
			}
		})
	}
}

func TestClientGetsMarkdownUsingBoundedSafeJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, httpx.APIWhiteboardMarkdown+"/"+clientTestID, r.URL.EscapedPath())
		require.Empty(t, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resource":{"id":"`+clientTestID+`","type":"markdown","path":"/whiteboards/markdown/`+clientTestID+`","expires_at":null,"permanent":true},"markdown":"# exact\nsource","context":"creator exact\ncontext"}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	got, err := client.GetMarkdown(context.Background(), clientTestID)
	require.NoError(t, err)
	require.Equal(t, httpx.MarkdownResponse{
		Resource: httpx.Resource{ID: clientTestID, Type: "markdown", Path: httpx.PublicMarkdown + clientTestID, Permanent: true},
		Markdown: "# exact\nsource",
		Context:  "creator exact\ncontext",
	}, got)
}

func TestClientGetsMarkdownAcrossDefaultWriteLimits(t *testing.T) {
	t.Parallel()

	markdown := strings.Repeat("m", 10<<20)
	creatorContext := strings.Repeat("c", 1<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, httpx.MarkdownResponse{
			Resource: httpx.Resource{
				ID: clientTestID, Type: "markdown", Path: httpx.PublicMarkdown + clientTestID, Permanent: true,
			},
			Markdown: markdown,
			Context:  creatorContext,
		})
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	got, err := client.GetMarkdown(context.Background(), clientTestID)

	require.NoError(t, err)
	require.Equal(t, markdown, got.Markdown)
	require.Equal(t, creatorContext, got.Context)
}

func TestClientMarkdownGetRejectsErrorsAndUnsafeResponsesWithoutLeakingBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		body     string
		wantCode common.ErrorCode
		wantMsg  string
		hidden   string
	}{
		{
			name: "not found", status: http.StatusNotFound,
			body:     `{"error":{"code":"not_found","message":"resource not found"}}`,
			wantCode: common.CodeNotFound, wantMsg: "resource not found",
		},
		{
			name: "private error body", status: http.StatusBadGateway,
			body: "# source secret\\ncreator context secret", wantCode: common.CodeInternal,
			wantMsg: "server returned an invalid error response", hidden: "secret",
		},
		{
			name: "malformed success", status: http.StatusOK,
			body: `{"resource":`, wantCode: common.CodeInternal,
			wantMsg: "server returned an invalid response", hidden: "resource",
		},
		{
			name: "invalid resource", status: http.StatusOK,
			body:     `{"resource":{"id":"` + clientSecondTestID + `","type":"markdown","path":"/whiteboards/markdown/` + clientSecondTestID + `","permanent":true},"markdown":"private source","context":"private context"}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid response", hidden: "private",
		},
		{
			name: "missing markdown", status: http.StatusOK,
			body:     `{"resource":{"id":"` + clientTestID + `","type":"markdown","path":"/whiteboards/markdown/` + clientTestID + `","permanent":true},"context":"private context"}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid response", hidden: "private",
		},
		{
			name: "empty markdown", status: http.StatusOK,
			body:     `{"resource":{"id":"` + clientTestID + `","type":"markdown","path":"/whiteboards/markdown/` + clientTestID + `","permanent":true},"markdown":"","context":"private context"}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid response", hidden: "private",
		},
		{
			name: "missing context", status: http.StatusOK,
			body:     `{"resource":{"id":"` + clientTestID + `","type":"markdown","path":"/whiteboards/markdown/` + clientTestID + `","permanent":true},"markdown":"private source"}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid response", hidden: "private",
		},
		{
			name: "wrong resource type", status: http.StatusOK,
			body:     `{"resource":{"id":"` + clientTestID + `","type":"html","path":"/whiteboards/markdown/` + clientTestID + `","permanent":true},"markdown":"private source","context":"private context"}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid response", hidden: "private",
		},
		{
			name: "wrong resource path", status: http.StatusOK,
			body:     `{"resource":{"id":"` + clientTestID + `","type":"markdown","path":"/images/` + clientTestID + `","permanent":true},"markdown":"private source","context":"private context"}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid response", hidden: "private",
		},
		{
			name: "unknown response field", status: http.StatusOK,
			body:     `{"resource":{"id":"` + clientTestID + `","type":"markdown","path":"/whiteboards/markdown/` + clientTestID + `","permanent":true},"markdown":"private source","context":"private context","storage_path":"private"}`,
			wantCode: common.CodeInternal, wantMsg: "server returned an invalid response", hidden: "private",
		},
		{
			name: "oversized success", status: http.StatusOK,
			body: strings.Repeat("private", (67<<20)/len("private")+1), wantCode: common.CodeInternal,
			wantMsg: "server response is too large", hidden: "private",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, server.Client())

			got, err := client.GetMarkdown(context.Background(), clientTestID)
			require.Equal(t, httpx.MarkdownResponse{}, got)
			var protocolErr *common.Error
			require.ErrorAs(t, err, &protocolErr)
			require.Equal(t, test.wantCode, protocolErr.Code)
			require.Equal(t, test.wantMsg, protocolErr.Message)
			if test.hidden != "" {
				require.NotContains(t, err.Error(), test.hidden)
			}
		})
	}
}

func TestClientMarkdownGetPreservesCancellationAndDeadline(t *testing.T) {
	t.Parallel()

	for _, contextErr := range []error{context.Canceled, context.DeadlineExceeded} {
		contextErr := contextErr
		t.Run(contextErr.Error(), func(t *testing.T) {
			t.Parallel()
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return nil, contextErr
			})
			client := newTestClient(t, "http://example.test", &http.Client{Transport: transport})

			_, err := client.GetMarkdown(context.Background(), clientTestID)
			require.True(t, err == contextErr)
		})
	}
}

func TestClientRejectsMalformedAndOversizedResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"resource":`},
		{name: "oversized", body: strings.Repeat("x", (1<<20)+1)},
		{name: "trailing JSON", body: `{"resource":{"id":"` + clientTestID + `"}} {}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, server.Client())

			_, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
				Name: "board.html", Reader: strings.NewReader("body"),
			}, nil)
			require.Error(t, err)
			require.NotContains(t, err.Error(), test.body)
		})
	}
}

func TestClientReturnsCapabilityWithUncertainCreateError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorResponse{
			Error: httpx.ErrorBody{Code: common.CodeStorageUnavailable, Message: "storage unavailable"},
			Resource: &httpx.Resource{
				ID: clientTestID, Path: httpx.PublicMarkdown + clientTestID, Permanent: true,
			},
		})
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	resource, err := client.CreateMarkdown(context.Background(),
		httpx.File{Name: "board.md", Reader: strings.NewReader("# board")},
		httpx.File{Name: "context.md", Reader: strings.NewReader("creator context")},
		nil,
	)

	require.Equal(t, clientTestID, resource.ID)
	require.Equal(t, httpx.PublicMarkdown+clientTestID, resource.Path)
	var protocolErr *common.Error
	require.ErrorAs(t, err, &protocolErr)
	require.Equal(t, common.CodeStorageUnavailable, protocolErr.Code)
}

func TestClientAcceptsResponseAtOneMiBLimit(t *testing.T) {
	t.Parallel()

	body := `{"resource":{"id":"` + clientTestID + `","path":"/whiteboards/html/` + clientTestID + `","permanent":true}}`
	body += strings.Repeat(" ", (1<<20)-len(body))
	require.Len(t, body, 1<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, server.Client())

	resource, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
		Name: "board.html", Reader: strings.NewReader("body"),
	}, nil)
	require.NoError(t, err)
	require.Equal(t, clientTestID, resource.ID)
}

func TestClientRejectsInvalidSuccessEnvelopesPrivately(t *testing.T) {
	t.Parallel()

	validResource := `{"id":"` + clientTestID + `","path":"/images/` + clientTestID + `","permanent":true}`
	tests := []struct {
		name      string
		operation string
		body      string
	}{
		{name: "null envelope", operation: "create", body: `null`},
		{name: "missing envelope", operation: "create", body: `{}`},
		{name: "missing resource fields", operation: "create", body: `{"resource":{}}`},
		{name: "invalid resource id", operation: "create", body: `{"resource":{"id":"private-id","path":"/images/private-id","permanent":true}}`},
		{name: "missing resource path", operation: "create", body: `{"resource":{"id":"` + clientTestID + `","permanent":true}}`},
		{name: "relative resource path", operation: "create", body: `{"resource":{"id":"` + clientTestID + `","path":"images/private","permanent":true}}`},
		{name: "cross origin resource path", operation: "create", body: `{"resource":{"id":"` + clientTestID + `","path":"https://private.test/secret","permanent":true}}`},
		{name: "traversing resource path", operation: "create", body: `{"resource":{"id":"` + clientTestID + `","path":"/images/../private","permanent":true}}`},
		{name: "create null expiration not permanent", operation: "create", body: `{"resource":{"id":"` + clientTestID + `","path":"/images/` + clientTestID + `","expires_at":null,"permanent":false}}`},
		{name: "update expiration marked permanent", operation: "update", body: `{"resource":{"id":"` + clientTestID + `","path":"/images/` + clientTestID + `","expires_at":123,"permanent":true}}`},
		{name: "update id mismatch", operation: "update", body: `{"resource":{"id":"` + clientSecondTestID + `","path":"/images/` + clientSecondTestID + `","permanent":true}}`},
		{name: "nil images", operation: "images", body: `{"images":null}`},
		{name: "wrong image cardinality", operation: "images", body: `{"images":[` + validResource + `]}`},
		{name: "duplicate image ids", operation: "images", body: `{"images":[` + validResource + `,` + validResource + `]}`},
		{name: "invalid image resource", operation: "images", body: `{"images":[` + validResource + `,{"id":"private","path":"/images/private","permanent":true}]}`},
		{name: "images null expiration not permanent", operation: "images", body: `{"images":[{"id":"` + clientTestID + `","path":"/images/` + clientTestID + `","expires_at":null,"permanent":false},{"id":"` + clientSecondTestID + `","path":"/images/` + clientSecondTestID + `","expires_at":null,"permanent":true}]}`},
		{name: "images expiration marked permanent", operation: "images", body: `{"images":[{"id":"` + clientTestID + `","path":"/images/` + clientTestID + `","expires_at":123,"permanent":true},{"id":"` + clientSecondTestID + `","path":"/images/` + clientSecondTestID + `","expires_at":null,"permanent":true}]}`},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if test.operation == "update" {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusCreated)
				}
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, server.Client())

			var (
				err       error
				resource  httpx.Resource
				resources []httpx.Resource
			)
			switch test.operation {
			case "create":
				resource, err = client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
					Name: "board.html", Reader: strings.NewReader("body"),
				}, nil)
			case "update":
				resource, err = client.UpdateImage(context.Background(), clientTestID, httpx.File{
					Name: "image.png", Reader: strings.NewReader("body"),
				}, nil)
			case "images":
				resources, err = client.CreateImages(context.Background(), []httpx.File{
					{Name: "one.png", Reader: strings.NewReader("one")},
					{Name: "two.png", Reader: strings.NewReader("two")},
				}, nil)
			default:
				t.Fatalf("unknown operation %q", test.operation)
			}

			var protocolErr *common.Error
			require.ErrorAs(t, err, &protocolErr)
			require.Equal(t, common.CodeInternal, protocolErr.Code)
			require.Equal(t, "server returned an invalid response", protocolErr.Message)
			require.NotContains(t, err.Error(), "private")
			if test.operation == "images" {
				require.Nil(t, resources)
			} else {
				require.Equal(t, httpx.Resource{}, resource)
			}
		})
	}
}

func TestClientPublicURLAcceptsOnlySafeSameOriginAbsolutePaths(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, "https://example.test:8443", &http.Client{})
	for path, want := range map[string]string{
		"/whiteboards/markdown/" + clientTestID: "https://example.test:8443/whiteboards/markdown/" + clientTestID,
		"/images/abc%20def":                     "https://example.test:8443/images/abc%20def",
		"/images/hash%23value":                  "https://example.test:8443/images/hash%23value",
	} {
		got, err := client.PublicURL(path)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	invalid := []string{
		"", "images/id", "//evil.test/images/id", "https://evil.test/images/id",
		"/../admin", "/images/../admin", "/images/%2e%2e/admin", "/images/%2E%2E/admin",
		"/images/id?secret=1", "/images/id#fragment", "/images/id#", "/images/id\\..\\admin", "/images/id%5c..%5cadmin",
	}
	for _, path := range invalid {
		got, err := client.PublicURL(path)
		require.Error(t, err, path)
		require.Empty(t, got)
	}
}

func TestClientStreamsMultipartWithoutReadingFileBeforeTransport(t *testing.T) {
	t.Parallel()

	reader := &transportGatedReader{allowed: make(chan struct{})}
	roundTripper := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(reader.allowed)
		_, err := io.Copy(io.Discard, request.Body)
		require.NoError(t, err)
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"resource":{"id":"` + clientTestID + `","path":"/whiteboards/markdown/` + clientTestID + `","permanent":true}}`,
			)),
			Request: request,
		}, nil
	})
	client := newTestClient(t, "http://example.test", &http.Client{Transport: roundTripper})

	_, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
		Name: "stream.html", Reader: reader,
	}, nil)
	require.NoError(t, err)
}

func TestClientClosesMultipartPipeAfterEarlyResponse(t *testing.T) {
	t.Parallel()

	requestBody := make(chan io.ReadCloser, 1)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var firstByte [1]byte
		_, err := io.ReadFull(request.Body, firstByte[:])
		require.NoError(t, err)
		requestBody <- request.Body
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_request","message":"rejected"}}`)),
			Request:    request,
		}, nil
	})
	client := newTestClient(t, "http://example.test", &http.Client{Transport: transport})
	result := make(chan error, 1)
	go func() {
		_, err := client.CreateWhiteboard(context.Background(), httpx.WhiteboardHTML, httpx.File{
			Name: "large.html", Reader: io.LimitReader(zeroReader{}, 1<<30),
		}, nil)
		result <- err
	}()

	select {
	case err := <-result:
		var protocolErr *common.Error
		require.ErrorAs(t, err, &protocolErr)
		require.Equal(t, common.CodeInvalidRequest, protocolErr.Code)
	case <-time.After(time.Second):
		t.Fatal("client stranded multipart writer after early response")
	}

	body := <-requestBody
	readResult := make(chan error, 1)
	go func() {
		var nextByte [1]byte
		read, err := body.Read(nextByte[:])
		if read != 0 {
			readResult <- errors.New("request body remained readable")
			return
		}
		readResult <- err
	}()
	select {
	case err := <-readResult:
		require.Error(t, err)
		require.NotEqual(t, "request body remained readable", err.Error())
	case <-time.After(time.Second):
		t.Fatal("request body remained open after early response")
	}
}

type multipartPart struct {
	Field    string
	Filename string
	Content  string
}

func readMultipartParts(t *testing.T, request *http.Request) []multipartPart {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(request.Body, params["boundary"])
	var parts []multipartPart
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		content, err := io.ReadAll(part)
		require.NoError(t, err)
		parts = append(parts, multipartPart{Field: part.FormName(), Filename: part.FileName(), Content: string(content)})
	}
	return parts
}

func fileParts(parts []multipartPart) []multipartPart {
	files := make([]multipartPart, 0, len(parts))
	for _, part := range parts {
		if part.Field != "expires_in_seconds" {
			files = append(files, part)
		}
	}
	return files
}

func expirationPart(parts []multipartPart) *string {
	for _, part := range parts {
		if part.Field == "expires_in_seconds" {
			value := part.Content
			return &value
		}
	}
	return nil
}

func newTestClient(t *testing.T, server string, httpClient *http.Client) *httpx.Client {
	t.Helper()
	client, err := httpx.NewClient(httpx.ClientConfig{Server: server, HTTPClient: httpClient})
	require.NoError(t, err)
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type transportGatedReader struct {
	allowed chan struct{}
	read    bool
}

type zeroReader struct{}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type readCloser struct {
	io.Reader
}

func (readCloser) Close() error { return nil }

type nilSafeReader struct{}

func (*nilSafeReader) Read([]byte) (int, error) { return 0, io.EOF }

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("secret panic payload")
}

func (zeroReader) Read(destination []byte) (int, error) {
	for index := range destination {
		destination[index] = 0
	}
	return len(destination), nil
}

func (reader *transportGatedReader) Read(destination []byte) (int, error) {
	select {
	case <-reader.allowed:
	default:
		panic("file was read before the transport received the request")
	}
	if reader.read {
		return 0, io.EOF
	}
	reader.read = true
	return copy(destination, bytes.Repeat([]byte("x"), 128)), nil
}
