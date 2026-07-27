package localapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type queuedBackend struct {
	mu          sync.Mutex
	connections []*controlledConnection
	connected   chan struct{}
}

func (b *queuedBackend) Connect(context.Context, string, agentprotocol.Command) (Connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	connection := b.connections[0]
	b.connections = b.connections[1:]
	if b.connected != nil {
		select {
		case b.connected <- struct{}{}:
		default:
		}
	}
	return connection, nil
}

type controlledConnection struct {
	events         chan agentprotocol.Event
	closed         chan struct{}
	closeOnce      sync.Once
	closeBlock     <-chan struct{}
	ignoreCloseCtx bool
	commandErr     error
	commandResult  *agentprotocol.Event
	mu             sync.Mutex
	commands       int
}

func newControlledConnection(events ...agentprotocol.Event) *controlledConnection {
	channel := make(chan agentprotocol.Event, len(events))
	for _, event := range events {
		channel <- event
	}
	return &controlledConnection{events: channel, closed: make(chan struct{})}
}

func (*controlledConnection) ConversationID() string               { return conversation }
func (c *controlledConnection) Events() <-chan agentprotocol.Event { return c.events }
func (c *controlledConnection) Close(ctx context.Context) error {
	if c.closeBlock != nil {
		if c.ignoreCloseCtx {
			<-c.closeBlock
		} else {
			select {
			case <-c.closeBlock:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *controlledConnection) Command(context.Context, agentprotocol.Command) (agentprotocol.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands++
	if c.commandErr != nil {
		return agentprotocol.Event{}, c.commandErr
	}
	if c.commandResult != nil {
		return *c.commandResult, nil
	}
	return resultEvent(commandID), nil
}

func startServerWithBackend(t *testing.T, backend Backend) (*Server, string, *http.Client) {
	t.Helper()
	trust := &mutableTrust{}
	trust.set(trustedOrigin)
	server, err := Listen(Config{Port: 0, TrustSource: trust, Backend: backend})
	require.NoError(t, err)
	server.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	return server, "http://" + server.Host(), &http.Client{Timeout: 5 * time.Second}
}

func TestWebSocketDisconnectStopsWriterWithoutEventsClosure(t *testing.T) {
	connection := newControlledConnection(snapshotEvent())
	backend := &queuedBackend{connections: []*controlledConnection{connection}}
	server, _, _ := startServerWithBackend(t, backend)
	wsURL := url.URL{Scheme: "ws", Host: server.Host(), Path: agentprotocol.ConnectPath}
	dialer := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
	socket, _, err := dialer.Dial(wsURL.String(), http.Header{"Origin": []string{trustedOrigin}})
	require.NoError(t, err)
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
	_, _, err = socket.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, socket.Close())

	require.Eventually(t, func() bool {
		select {
		case <-connection.closed:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	connection.events <- snapshotEvent()
	select {
	case connection.events <- snapshotEvent():
		t.Fatal("disconnected WebSocket writer remained subscribed")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFallbackReconnectDoesNotReplaceBeforeFirstEvent(t *testing.T) {
	firstConnection := newControlledConnection(snapshotEvent())
	secondConnection := newControlledConnection()
	connected := make(chan struct{}, 2)
	backend := &queuedBackend{connections: []*controlledConnection{firstConnection, secondConnection}, connected: connected}
	_, baseURL, client := startServerWithBackend(t, backend)
	body := encodeCommand(t, connectCommand())

	request := func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+agentprotocol.ConnectPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Origin", trustedOrigin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
		return client.Do(req)
	}

	first, err := request(context.Background())
	require.NoError(t, err)
	_, err = bufio.NewReader(first.Body).ReadBytes('\n')
	require.NoError(t, err)

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		response, requestErr := request(secondCtx)
		if response != nil {
			response.Body.Close()
		}
		secondDone <- requestErr
	}()
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not reach backend")
	}
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("second reconnect did not reach backend")
	}

	select {
	case <-firstConnection.closed:
		t.Fatal("working attachment closed before reconnect produced its first event")
	default:
	}

	commandResponse, err := http.NewRequest(http.MethodPost, baseURL+CommandsPath, bytes.NewReader(encodeCommand(t, ordinaryCommand())))
	require.NoError(t, err)
	commandResponse.Header.Set("Origin", trustedOrigin)
	commandResponse.Header.Set("Content-Type", "application/json")
	commandResponse.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	response, err := client.Do(commandResponse)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	firstConnection.mu.Lock()
	require.Equal(t, 1, firstConnection.commands)
	firstConnection.mu.Unlock()

	cancelSecond()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("canceled reconnect did not return")
	}
	first.Body.Close()
}

func TestFallbackInvalidFirstEventLeavesOldAttachmentUsable(t *testing.T) {
	firstConnection := newControlledConnection(snapshotEvent())
	invalid := snapshotEvent()
	invalid.ConversationID = "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	secondConnection := newControlledConnection(invalid)
	backend := &queuedBackend{connections: []*controlledConnection{firstConnection, secondConnection}}
	_, baseURL, client := startServerWithBackend(t, backend)

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

	first := post(agentprotocol.ConnectPath, connectCommand())
	_, err := bufio.NewReader(first.Body).ReadBytes('\n')
	require.NoError(t, err)
	failed := post(agentprotocol.ConnectPath, connectCommand())
	require.Equal(t, http.StatusServiceUnavailable, failed.StatusCode)
	failed.Body.Close()

	select {
	case <-firstConnection.closed:
		t.Fatal("invalid reconnect event closed the working attachment")
	default:
	}
	commandResponse := post(CommandsPath, ordinaryCommand())
	require.Equal(t, http.StatusAccepted, commandResponse.StatusCode)
	commandResponse.Body.Close()
	firstConnection.mu.Lock()
	require.Equal(t, 1, firstConnection.commands)
	firstConnection.mu.Unlock()
	first.Body.Close()
}

func TestRemovedTrustPreservesAttachmentAcrossCommandStreamRace(t *testing.T) {
	running := startServer(t)
	streamCtx, cancelStream := context.WithCancel(context.Background())
	streamRequest, err := http.NewRequestWithContext(streamCtx, http.MethodPost, running.baseURL+agentprotocol.ConnectPath, bytes.NewReader(encodeCommand(t, connectCommand())))
	require.NoError(t, err)
	streamRequest.Header.Set("Origin", trustedOrigin)
	streamRequest.Header.Set("Content-Type", "application/json")
	streamRequest.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	stream, err := running.client.Do(streamRequest)
	require.NoError(t, err)
	_, err = bufio.NewReader(stream.Body).ReadBytes('\n')
	require.NoError(t, err)
	running.trust.set()

	preserved := running.request(t, http.MethodPost, CommandsPath, trustedOrigin, encodeCommand(t, ordinaryCommand()))
	require.Equal(t, http.StatusAccepted, preserved.StatusCode)
	preserved.Body.Close()

	const requests = 24
	results := make(chan error, requests)
	var workers sync.WaitGroup
	for index := 0; index < requests; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			request, requestErr := http.NewRequest(http.MethodPost, running.baseURL+CommandsPath, bytes.NewReader(encodeCommand(t, ordinaryCommand())))
			if requestErr != nil {
				results <- requestErr
				return
			}
			request.Header.Set("Origin", trustedOrigin)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
			response, requestErr := running.client.Do(request)
			if requestErr != nil {
				results <- requestErr
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusConflict {
				results <- errors.New(response.Status)
				return
			}
			results <- nil
		}()
	}
	go func() {
		time.Sleep(time.Millisecond)
		cancelStream()
		stream.Body.Close()
	}()
	workers.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result)
	}
	running.client.CloseIdleConnections()
	require.Eventually(t, func() bool {
		running.server.mu.Lock()
		defer running.server.mu.Unlock()
		return len(running.server.transports) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestServerCloseDeadlineBoundsMisbehavingConnection(t *testing.T) {
	release := make(chan struct{})
	connection := newControlledConnection(snapshotEvent())
	connection.closeBlock = release
	connection.ignoreCloseCtx = true
	backend := &queuedBackend{connections: []*controlledConnection{connection}}
	server, baseURL, client := startServerWithBackend(t, backend)

	request, err := http.NewRequest(http.MethodPost, baseURL+agentprotocol.ConnectPath, bytes.NewReader(encodeCommand(t, connectCommand())))
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	response, err := client.Do(request)
	require.NoError(t, err)
	_, err = bufio.NewReader(response.Body).ReadBytes('\n')
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	started := time.Now()
	err = server.Close(ctx)
	cancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	close(release)
	response.Body.Close()
}
