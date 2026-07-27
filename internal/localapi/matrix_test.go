package localapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rawLocalAPIResponse(t *testing.T, server *Server, request string) *http.Response {
	t.Helper()
	connection, err := net.Dial("tcp4", server.Host())
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	_, err = io.WriteString(connection, request)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	return response
}

func TestRawHostOriginAndAuthorityMatrix(t *testing.T) {
	running := startServer(t)
	validRequest := func(headers string) string {
		return fmt.Sprintf("GET %s HTTP/1.1\r\n%s\r\nConnection: close\r\n\r\n", agentprotocol.StatusPath, headers)
	}
	cases := []struct {
		name    string
		headers string
		status  int
	}{
		{"missing-host", "Origin: " + trustedOrigin, http.StatusBadRequest},
		{"duplicate-host", "Host: " + running.server.Host() + "\r\nHost: " + running.server.Host() + "\r\nOrigin: " + trustedOrigin, http.StatusBadRequest},
		{"malformed-host", "Host: bad host\r\nOrigin: " + trustedOrigin, http.StatusBadRequest},
		{"malformed-port", "Host: 127.0.0.1:not-a-port\r\nOrigin: " + trustedOrigin, http.StatusForbidden},
		{"malformed-authority", "Host: user@" + running.server.Host() + "\r\nOrigin: " + trustedOrigin, http.StatusBadRequest},
		{"missing-origin", "Host: " + running.server.Host(), http.StatusForbidden},
		{"duplicate-origin", "Host: " + running.server.Host() + "\r\nOrigin: " + trustedOrigin + "\r\nOrigin: " + otherOrigin, http.StatusForbidden},
		{"combined-origin", "Host: " + running.server.Host() + "\r\nOrigin: " + trustedOrigin + ", " + otherOrigin, http.StatusForbidden},
		{"malformed-origin", "Host: " + running.server.Host() + "\r\nOrigin: https://whiteboard.example:bad", http.StatusForbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := rawLocalAPIResponse(t, running.server, validRequest(test.headers))
			defer response.Body.Close()
			assert.Equal(t, test.status, response.StatusCode)
			assert.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
		})
	}
}

func TestInvalidAndMultiplePrivateNetworkHeadersAreNeverGranted(t *testing.T) {
	running := startServer(t)
	cases := []struct {
		name   string
		values []string
	}{
		{"invalid", []string{"TRUE"}},
		{"empty", []string{""}},
		{"multiple", []string{"true", "true"}},
		{"conflicting", []string{"true", "false"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodOptions, running.baseURL+agentprotocol.StatusPath, nil)
			require.NoError(t, err)
			request.Header.Set("Origin", trustedOrigin)
			request.Header.Set("Access-Control-Request-Method", http.MethodGet)
			for _, value := range test.values {
				request.Header.Add("Access-Control-Request-Private-Network", value)
			}
			response, err := running.client.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			assert.Equal(t, http.StatusNoContent, response.StatusCode)
			assert.Empty(t, response.Header.Get("Access-Control-Allow-Private-Network"))
		})
	}
}

func TestFragmentedOversizedWebSocketMessageIsRejected(t *testing.T) {
	running := startServer(t)
	wsURL := url.URL{Scheme: "ws", Host: running.server.Host(), Path: agentprotocol.ConnectPath}
	dialer := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
	socket, _, err := dialer.Dial(wsURL.String(), http.Header{"Origin": []string{trustedOrigin}})
	require.NoError(t, err)
	defer socket.Close()
	writer, err := socket.NextWriter(websocket.TextMessage)
	require.NoError(t, err)
	chunk := bytes.Repeat([]byte{'x'}, 64<<10)
	remaining := agentprotocol.MaxContextCommandBytes + 1
	for remaining > 0 {
		size := min(remaining, len(chunk))
		if _, err = writer.Write(chunk[:size]); err != nil {
			break
		}
		remaining -= size
	}
	_ = writer.Close()
	_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = socket.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseMessageTooBig, closeErr.Code)
}

type recordedRequests struct {
	mu      sync.Mutex
	records []RequestRecord
}

func (r *recordedRequests) Record(record RequestRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
}

func TestRequestRecorderContainsOnlyClassifiedRequestOutcome(t *testing.T) {
	recorder := &recordedRequests{}
	trust := &mutableTrust{}
	trust.set(trustedOrigin)
	server, err := Listen(Config{Port: 0, TrustSource: trust, Backend: &fakeBackend{}, Recorder: recorder})
	require.NoError(t, err)
	server.Serve()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	}()
	client := &http.Client{Timeout: 5 * time.Second}

	request, err := http.NewRequest(http.MethodPost, "http://"+server.Host()+"/native/secret", strings.NewReader("board-native-secret"))
	require.NoError(t, err)
	request.Header.Set("Origin", otherOrigin)
	response, err := client.Do(request)
	require.NoError(t, err)
	response.Body.Close()

	request, err = http.NewRequest(http.MethodGet, "http://"+server.Host()+agentprotocol.StatusPath, nil)
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	response, err = client.Do(request)
	require.NoError(t, err)
	response.Body.Close()

	wsURL := url.URL{Scheme: "ws", Host: server.Host(), Path: agentprotocol.ConnectPath}
	dialer := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
	socket, _, err := dialer.Dial(wsURL.String(), http.Header{"Origin": []string{trustedOrigin}})
	require.NoError(t, err)
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
	_, _, err = socket.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, socket.Close())

	var records []RequestRecord
	require.Eventually(t, func() bool {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		records = append([]RequestRecord(nil), recorder.records...)
		return len(records) == 3
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, []RequestRecord{
		{Route: "unknown", Method: http.MethodPost, Status: http.StatusNotFound, Code: agentprotocol.ErrorInvalidCommand},
		{Route: agentprotocol.StatusPath, Method: http.MethodGet, Status: http.StatusOK},
		{Route: agentprotocol.ConnectPath, Method: http.MethodGet, Status: http.StatusSwitchingProtocols},
	}, records)
	representation := fmt.Sprintf("%#v", records)
	assert.NotContains(t, representation, trustedOrigin)
	assert.NotContains(t, representation, otherOrigin)
	assert.NotContains(t, representation, clientID)
	assert.NotContains(t, representation, conversation)
	assert.NotContains(t, representation, "board-native-secret")
}

type codedBackendError struct {
	code agentprotocol.BrowserErrorCode
	raw  string
}

func (e codedBackendError) Error() string                                    { return e.raw }
func (e codedBackendError) BrowserErrorCode() agentprotocol.BrowserErrorCode { return e.code }

type failingBackend struct{ err error }

func (b failingBackend) Connect(context.Context, string, agentprotocol.Command) (Connection, error) {
	return nil, b.err
}

func TestBackendErrorsAreClassifiedWithoutRawDisclosure(t *testing.T) {
	for _, test := range []struct {
		name string
		code agentprotocol.BrowserErrorCode
		want agentprotocol.BrowserErrorCode
	}{
		{"valid-code", agentprotocol.ErrorProviderMissing, agentprotocol.ErrorProviderMissing},
		{"invalid-code", agentprotocol.BrowserErrorCode("native_secret_code"), agentprotocol.ErrorBrokerUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, baseURL, client := startServerWithBackend(t, failingBackend{err: codedBackendError{code: test.code, raw: "native /Users/private credential"}})
			_ = server
			request, err := http.NewRequest(http.MethodPost, baseURL+agentprotocol.ConnectPath, bytes.NewReader(encodeCommand(t, connectCommand())))
			require.NoError(t, err)
			request.Header.Set("Origin", trustedOrigin)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
			response, err := client.Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			response.Body.Close()
			assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
			assert.Contains(t, string(body), `"code":"`+string(test.want)+`"`)
			assert.NotContains(t, string(body), "native")
			assert.NotContains(t, string(body), "/Users/private")
			assert.NotContains(t, string(body), "credential")
		})
	}
}

func TestCommandBackendErrorAndMismatchedResultAreRedacted(t *testing.T) {
	cases := []struct {
		name       string
		configure  func(*controlledConnection)
		wantStatus int
		wantCode   agentprotocol.BrowserErrorCode
	}{
		{
			name: "coded-backend-error",
			configure: func(connection *controlledConnection) {
				connection.commandErr = codedBackendError{code: agentprotocol.ErrorProviderCrashed, raw: "native session /Users/private"}
			},
			wantStatus: http.StatusConflict,
			wantCode:   agentprotocol.ErrorProviderCrashed,
		},
		{
			name: "mismatched-result",
			configure: func(connection *controlledConnection) {
				event := resultEvent(commandID)
				event.ConversationID = "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
				connection.commandResult = &event
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   agentprotocol.ErrorBrokerUnavailable,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			connection := newControlledConnection(snapshotEvent())
			test.configure(connection)
			_, baseURL, client := startServerWithBackend(t, &queuedBackend{connections: []*controlledConnection{connection}})
			post := func(path string, command agentprotocol.Command) *http.Response {
				request, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(encodeCommand(t, command)))
				require.NoError(t, err)
				request.Header.Set("Origin", trustedOrigin)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
				response, err := client.Do(request)
				require.NoError(t, err)
				return response
			}
			stream := post(agentprotocol.ConnectPath, connectCommand())
			_, err := bufio.NewReader(stream.Body).ReadBytes('\n')
			require.NoError(t, err)
			response := post(CommandsPath, ordinaryCommand())
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			response.Body.Close()
			assert.Equal(t, test.wantStatus, response.StatusCode)
			assert.Contains(t, string(body), `"code":"`+string(test.wantCode)+`"`)
			assert.NotContains(t, string(body), "native session")
			assert.NotContains(t, string(body), "/Users/private")
			stream.Body.Close()
		})
	}
}

type flushFailureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *flushFailureWriter) Header() http.Header            { return w.header }
func (w *flushFailureWriter) WriteHeader(status int)         { w.status = status }
func (w *flushFailureWriter) Write(data []byte) (int, error) { return w.body.Write(data) }
func (*flushFailureWriter) Flush()                           {}
func (*flushFailureWriter) FlushError() error                { return errors.New("flush failed") }

func TestFallbackFlushFailureDoesNotReplaceOldAttachment(t *testing.T) {
	oldConnection := newControlledConnection()
	newConnection := newControlledConnection(snapshotEvent())
	backend := &queuedBackend{connections: []*controlledConnection{newConnection}}
	trust := &mutableTrust{}
	trust.set(trustedOrigin)
	server, err := Listen(Config{Port: 0, TrustSource: trust, Backend: backend})
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	}()
	oldCtx, oldCancel := context.WithCancel(context.Background())
	old := &attachment{
		key:        attachmentKey{origin: trustedOrigin, clientID: clientID, conversationID: conversation},
		connection: newSafeConnection(oldConnection),
		cancel:     oldCancel,
		done:       make(chan struct{}),
	}
	server.attachments.put(old)

	request, err := http.NewRequest(http.MethodPost, "http://"+server.Host()+agentprotocol.ConnectPath, bytes.NewReader(encodeCommand(t, connectCommand())))
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	writer := &flushFailureWriter{header: make(http.Header)}
	server.stream(writer, request)

	assert.Same(t, old, server.attachments.get(old.key))
	select {
	case <-oldConnection.closed:
		t.Fatal("flush failure closed old attachment")
	default:
	}
	closeCtx, closeCancel := context.WithTimeout(oldCtx, time.Second)
	_ = old.close(closeCtx)
	closeCancel()
	server.attachments.remove(old)
}
