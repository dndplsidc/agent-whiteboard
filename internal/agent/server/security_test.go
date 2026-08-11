package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/broker"
	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	trustedOrigin = "https://whiteboard.example"
	otherOrigin   = "https://other.example"
	clientID      = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	conversation  = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	commandID     = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	eventID       = "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	resourceID    = "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE"
)

type mutableTrust struct {
	mu      sync.Mutex
	origins map[string]struct{}
	err     error
	loads   int
}

func (t *mutableTrust) TrustedOrigins(context.Context) (map[string]struct{}, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loads++
	copy := make(map[string]struct{}, len(t.origins))
	for origin := range t.origins {
		copy[origin] = struct{}{}
	}
	return copy, t.err
}

func (t *mutableTrust) set(origins ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.origins = make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		t.origins[origin] = struct{}{}
	}
}

type fakeBackend struct {
	mu          sync.Mutex
	connections []*fakeConnection
	commands    []protocol.Command
}

func (b *fakeBackend) Connect(_ context.Context, _ string, command protocol.Command) (broker.BrowserConnection, error) {
	connection := &fakeConnection{events: make(chan protocol.Event, 8), closed: make(chan struct{})}
	connection.events <- snapshotEvent()
	b.mu.Lock()
	b.connections = append(b.connections, connection)
	b.commands = append(b.commands, command)
	b.mu.Unlock()
	return connection, nil
}

type fakeConnection struct {
	events    chan protocol.Event
	closed    chan struct{}
	closeOnce sync.Once
}

func (*fakeConnection) ConversationID() string          { return conversation }
func (c *fakeConnection) Events() <-chan protocol.Event { return c.events }
func (c *fakeConnection) Close(context.Context) error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *fakeConnection) Command(_ context.Context, command protocol.Command) (protocol.Event, error) {
	event := resultEvent(command.CommandID)
	select {
	case c.events <- event:
	case <-c.closed:
		return protocol.Event{}, context.Canceled
	}
	return event, nil
}

func connectCommand() protocol.Command {
	created := time.Unix(1, 0).UTC()
	return protocol.Command{
		APIVersion: protocol.APIVersion,
		CommandID:  commandID,
		ClientID:   clientID,
		Type:       protocol.CommandConnect,
		Payload: protocol.ConnectPayload{
			Provider:      protocol.ProviderPi,
			Resource:      protocol.Resource{Kind: protocol.ResourceMarkdown, ID: resourceID, CreatedAt: created, UpdatedAt: created},
			ContextDigest: strings.Repeat("a", 64),
		},
	}
}

func ordinaryCommand() protocol.Command {
	conversationID := conversation
	return protocol.Command{
		APIVersion:     protocol.APIVersion,
		CommandID:      commandID,
		ClientID:       clientID,
		ConversationID: &conversationID,
		Type:           protocol.CommandNew,
		Payload:        protocol.EmptyPayload{},
	}
}

func snapshotEvent() protocol.Event {
	return protocol.Event{
		APIVersion:     protocol.APIVersion,
		EventID:        eventID,
		ConversationID: conversation,
		Type:           protocol.EventSnapshot,
		Timestamp:      time.Unix(2, 0).UTC(),
		Payload:        protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted},
	}
}

func resultEvent(id string) protocol.Event {
	return protocol.Event{
		APIVersion:     protocol.APIVersion,
		EventID:        "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
		ConversationID: conversation,
		Type:           protocol.EventCommandResult,
		Timestamp:      time.Unix(3, 0).UTC(),
		Payload:        protocol.CommandResultPayload{CommandID: id, Status: protocol.CommandSucceeded},
	}
}

type runningServer struct {
	server  *Server
	trust   *mutableTrust
	backend *fakeBackend
	baseURL string
	client  *http.Client
}

func startServer(t *testing.T) *runningServer {
	t.Helper()
	trust := &mutableTrust{}
	trust.set(trustedOrigin)
	backend := &fakeBackend{}
	server, err := Listen(Config{Port: 0, TrustSource: trust, Backend: backend})
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", server.Addr().(*net.TCPAddr).IP.String())
	server.Serve()
	running := &runningServer{server: server, trust: trust, backend: backend, baseURL: "http://" + server.Host(), client: &http.Client{Timeout: 10 * time.Second}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, server.Close(ctx))
	})
	return running
}

func (s *runningServer) request(t *testing.T, method, path, origin string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, s.baseURL+path, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Origin", origin)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(protocol.APIVersionHeader, protocol.APIVersion)
	}
	response, err := s.client.Do(request)
	require.NoError(t, err)
	return response
}

func encodeCommand(t *testing.T, command protocol.Command) []byte {
	t.Helper()
	encoded, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	return encoded
}

func readJSON(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	var value map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&value))
	return value
}

func TestListenerHostOriginAndMinimalStatus(t *testing.T) {
	running := startServer(t)

	response := running.request(t, http.MethodGet, protocol.StatusPath, trustedOrigin, nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, trustedOrigin, response.Header.Get("Access-Control-Allow-Origin"))
	assert.Empty(t, response.Header.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	assert.Equal(t, map[string]any{"available": true, "api_version": "3", "origin_trusted": true}, readJSON(t, response))

	response = running.request(t, http.MethodGet, protocol.StatusPath, otherOrigin, nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, map[string]any{"available": true, "api_version": "3", "origin_trusted": false}, readJSON(t, response))

	for _, origin := range []string{"", "null", "http://whiteboard.example", "https://whiteboard.example/", "HTTPS://whiteboard.example", "https://whiteboard.example:443", "https://user@whiteboard.example"} {
		request, err := http.NewRequest(http.MethodGet, running.baseURL+protocol.StatusPath, nil)
		require.NoError(t, err)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response, err = running.client.Do(request)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, response.StatusCode, origin)
		assert.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
		response.Body.Close()
	}

	request, err := http.NewRequest(http.MethodGet, running.baseURL+protocol.StatusPath, nil)
	require.NoError(t, err)
	request.Host = "localhost:" + fmt.Sprint(running.server.Addr().(*net.TCPAddr).Port)
	request.Header.Set("Origin", trustedOrigin)
	response, err = running.client.Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()
}

func TestAutomaticLiteralLoopbackHTTPOrigin(t *testing.T) {
	const origin = "http://127.0.0.1:4321"

	t.Run("status preflight and fallback", func(t *testing.T) {
		running := startServer(t)

		response := running.request(t, http.MethodGet, protocol.StatusPath, origin, nil)
		require.Equal(t, http.StatusOK, response.StatusCode)
		assert.Equal(t, origin, response.Header.Get("Access-Control-Allow-Origin"))
		assert.Equal(t, map[string]any{"available": true, "api_version": "3", "origin_trusted": true}, readJSON(t, response))

		withoutPort := "http://127.0.0.1"
		response = running.request(t, http.MethodGet, protocol.StatusPath, withoutPort, nil)
		require.Equal(t, http.StatusOK, response.StatusCode)
		assert.Equal(t, withoutPort, response.Header.Get("Access-Control-Allow-Origin"))
		assert.Equal(t, map[string]any{"available": true, "api_version": "3", "origin_trusted": true}, readJSON(t, response))

		preflight, err := http.NewRequest(http.MethodOptions, running.baseURL+protocol.ConnectPath, nil)
		require.NoError(t, err)
		preflight.Header.Set("Origin", origin)
		preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
		preflight.Header.Set("Access-Control-Request-Headers", strings.Join(mutationHeaders, ", "))
		preflight.Header.Set("Access-Control-Request-Private-Network", "true")
		response, err = running.client.Do(preflight)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, response.StatusCode)
		assert.Equal(t, origin, response.Header.Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "true", response.Header.Get("Access-Control-Allow-Private-Network"))
		response.Body.Close()

		stream := running.request(t, http.MethodPost, protocol.ConnectPath, origin, encodeCommand(t, connectCommand()))
		require.Equal(t, http.StatusOK, stream.StatusCode)
		_, err = bufio.NewReader(stream.Body).ReadBytes('\n')
		require.NoError(t, err)
		stream.Body.Close()
	})

	t.Run("websocket", func(t *testing.T) {
		running := startServer(t)
		wsURL := url.URL{Scheme: "ws", Host: running.server.Host(), Path: protocol.ConnectPath}
		dialer := websocket.Dialer{Subprotocols: []string{protocol.WebSocketSubprotocol}}
		socket, _, err := dialer.Dial(wsURL.String(), http.Header{"Origin": []string{origin}})
		require.NoError(t, err)
		defer socket.Close()
		require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
		_, _, err = socket.ReadMessage()
		require.NoError(t, err)
	})
}

func TestAutomaticLoopbackHTTPRejectsNearMatches(t *testing.T) {
	running := startServer(t)
	for _, origin := range []string{
		"HTTP://127.0.0.1:4321",
		"http://127.0.0.1:08080",
		"http://127.0.0.1:80",
		"http://127.0.0.1/",
		"http://user@127.0.0.1:4321",
		"http://localhost:4321",
		"http://127.0.0.2:4321",
		"http://127.1:4321",
		"http://[::1]:4321",
	} {
		t.Run(origin, func(t *testing.T) {
			response := running.request(t, http.MethodGet, protocol.StatusPath, origin, nil)
			assert.Equal(t, http.StatusForbidden, response.StatusCode)
			assert.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
			response.Body.Close()
		})
	}
}

func TestCORSAndLegacyPNAExactMatrix(t *testing.T) {
	running := startServer(t)

	request, err := http.NewRequest(http.MethodOptions, running.baseURL+protocol.StatusPath, nil)
	require.NoError(t, err)
	request.Header.Set("Origin", otherOrigin)
	request.Header.Set("Access-Control-Request-Method", "GET")
	request.Header.Set("Access-Control-Request-Private-Network", "true")
	response, err := running.client.Do(request)
	require.NoError(t, err)
	response.Body.Close()
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
	assert.Equal(t, "true", response.Header.Get("Access-Control-Allow-Private-Network"))
	assert.Equal(t, "GET", response.Header.Get("Access-Control-Allow-Methods"))
	assert.Empty(t, response.Header.Get("Access-Control-Allow-Credentials"))

	request, err = http.NewRequest(http.MethodOptions, running.baseURL+protocol.ConnectPath, nil)
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "X-Agent-Whiteboard-API-Version, Content-Type")
	request.Header.Set("Access-Control-Request-Private-Network", "false")
	response, err = running.client.Do(request)
	require.NoError(t, err)
	response.Body.Close()
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
	assert.Equal(t, "content-type, x-agent-whiteboard-api-version", response.Header.Get("Access-Control-Allow-Headers"))
	assert.Empty(t, response.Header.Get("Access-Control-Allow-Private-Network"))

	request.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
	response, err = running.client.Do(request)
	require.NoError(t, err)
	response.Body.Close()
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	assert.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
}

func TestHeaderLimitAndExactHostOnRawListener(t *testing.T) {
	running := startServer(t)
	connection, err := net.Dial("tcp4", running.server.Host())
	require.NoError(t, err)
	defer connection.Close()
	_, err = fmt.Fprintf(connection, "GET %s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nX-Fill: %s\r\n\r\n", protocol.StatusPath, running.server.Host(), trustedOrigin, strings.Repeat("x", 40<<10))
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, response.StatusCode)
	response.Body.Close()
}

func TestWebSocketAndFallbackParityTrustRemovalAndShutdown(t *testing.T) {
	running := startServer(t)
	wsURL := url.URL{Scheme: "ws", Host: running.server.Host(), Path: protocol.ConnectPath}
	dialer := websocket.Dialer{Subprotocols: []string{protocol.WebSocketSubprotocol}, EnableCompression: false}
	header := http.Header{"Origin": []string{trustedOrigin}}
	socket, response, err := dialer.Dial(wsURL.String(), header)
	require.NoError(t, err)
	if response != nil {
		response.Body.Close()
	}
	require.Equal(t, protocol.WebSocketSubprotocol, socket.Subprotocol())
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
	_, eventBytes, err := socket.ReadMessage()
	require.NoError(t, err)
	wsEvent, err := protocol.DecodeEvent(eventBytes)
	require.NoError(t, err)
	assert.Equal(t, protocol.EventSnapshot, wsEvent.Type)
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, ordinaryCommand())))
	_, eventBytes, err = socket.ReadMessage()
	require.NoError(t, err)
	wsResult, err := protocol.DecodeEvent(eventBytes)
	require.NoError(t, err)
	assert.Equal(t, protocol.EventCommandResult, wsResult.Type)

	streamClientID := strings.Repeat("I", 32)
	streamConnect := connectCommand()
	streamConnect.ClientID = streamClientID
	streamResponse := running.request(t, http.MethodPost, protocol.ConnectPath, trustedOrigin, encodeCommand(t, streamConnect))
	require.Equal(t, http.StatusOK, streamResponse.StatusCode)
	reader := bufio.NewReader(streamResponse.Body)
	line, err := reader.ReadBytes('\n')
	require.NoError(t, err)
	streamEvent, err := protocol.DecodeEvent(bytes.TrimSpace(line))
	require.NoError(t, err)
	assert.Equal(t, wsEvent.Type, streamEvent.Type)

	running.trust.set()
	preservedCommand := ordinaryCommand()
	preservedCommand.CommandID = "HHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH"
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, preservedCommand)))
	_, eventBytes, err = socket.ReadMessage()
	require.NoError(t, err)
	preservedResult, err := protocol.DecodeEvent(eventBytes)
	require.NoError(t, err)
	assert.Equal(t, preservedCommand.CommandID, preservedResult.Payload.(protocol.CommandResultPayload).CommandID)

	streamCommand := ordinaryCommand()
	streamCommand.ClientID = streamClientID
	commandResponse := running.request(t, http.MethodPost, CommandsPath, trustedOrigin, encodeCommand(t, streamCommand))
	require.Equal(t, http.StatusAccepted, commandResponse.StatusCode)
	commandBytes, err := io.ReadAll(commandResponse.Body)
	require.NoError(t, err)
	commandResponse.Body.Close()
	commandEvent, err := protocol.DecodeEvent(commandBytes)
	require.NoError(t, err)
	assert.Equal(t, wsResult.Type, commandEvent.Type)

	denied := running.request(t, http.MethodPost, protocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	assert.Equal(t, http.StatusForbidden, denied.StatusCode)
	denied.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, running.server.Close(ctx))
	_ = socket.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err = socket.ReadMessage()
	require.Error(t, err)
	_, err = io.ReadAll(reader)
	require.NoError(t, err)
}
