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
	closeStarted   chan struct{}
	ignoreCloseCtx bool
	commandBlock   <-chan struct{}
	commandStarted chan struct{}
	commandErr     error
	commandResult  *agentprotocol.Event
	mu             sync.Mutex
	commands       int
	closeCalls     int
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
	c.mu.Lock()
	c.closeCalls++
	c.mu.Unlock()
	if c.closeStarted != nil {
		select {
		case c.closeStarted <- struct{}{}:
		default:
		}
	}
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
func (c *controlledConnection) Command(ctx context.Context, _ agentprotocol.Command) (agentprotocol.Event, error) {
	c.mu.Lock()
	c.commands++
	commandErr := c.commandErr
	commandResult := c.commandResult
	c.mu.Unlock()
	if c.commandStarted != nil {
		select {
		case c.commandStarted <- struct{}{}:
		default:
		}
	}
	if c.commandBlock != nil {
		select {
		case <-c.commandBlock:
		case <-ctx.Done():
			return agentprotocol.Event{}, ctx.Err()
		}
	}
	if commandErr != nil {
		return agentprotocol.Event{}, commandErr
	}
	if commandResult != nil {
		return *commandResult, nil
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
	secondConnection := newControlledConnection(resultEvent(commandID))
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

func TestAttachmentRegistrySerializesActivationWithCommandDispatch(t *testing.T) {
	commandRelease := make(chan struct{})
	commandStarted := make(chan struct{}, 1)
	oldConnection := newControlledConnection()
	oldConnection.commandBlock = commandRelease
	oldConnection.commandStarted = commandStarted
	newConnection := newControlledConnection()
	key := attachmentKey{origin: trustedOrigin, clientID: clientID, conversationID: conversation}
	oldCtx, oldCancel := context.WithCancel(context.Background())
	newCtx, newCancel := context.WithCancel(context.Background())
	defer oldCancel()
	defer newCancel()
	old := &attachment{key: key, connection: newSafeConnection(oldConnection), ctx: oldCtx, cancel: oldCancel, done: make(chan struct{})}
	newItem := &attachment{key: key, connection: newSafeConnection(newConnection), ctx: newCtx, cancel: newCancel, done: make(chan struct{})}
	var registry attachmentRegistry
	registry.put(old)

	commandDone := make(chan error, 1)
	go func() {
		_, attached, err := registry.command(oldCtx, key, ordinaryCommand())
		if !attached && err == nil {
			err = errors.New("command was not attached")
		}
		commandDone <- err
	}()
	select {
	case <-commandStarted:
	case <-time.After(time.Second):
		t.Fatal("old command did not enter dispatch")
	}

	published := make(chan struct{})
	activationDone := make(chan error, 1)
	go func() {
		_, err := registry.activate(newItem, func() error {
			close(published)
			return nil
		})
		activationDone <- err
	}()
	select {
	case <-published:
		close(commandRelease)
		t.Fatal("activation published while the old command was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(commandRelease)
	select {
	case err := <-commandDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("old command did not finish")
	}
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("activation did not publish after old dispatch completed")
	}
	select {
	case err := <-activationDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("activation did not complete")
	}

	_, attached, err := registry.command(newCtx, key, ordinaryCommand())
	require.True(t, attached)
	require.NoError(t, err)
	oldConnection.mu.Lock()
	oldCommands := oldConnection.commands
	oldConnection.mu.Unlock()
	newConnection.mu.Lock()
	newCommands := newConnection.commands
	newConnection.mu.Unlock()
	require.Equal(t, 1, oldCommands)
	require.Equal(t, 1, newCommands)
}

func TestFallbackReconnectHandoffWaitsForOldCommandAndDispatchesNewCommandsToNewConnection(t *testing.T) {
	commandRelease := make(chan struct{})
	oldCommandStarted := make(chan struct{}, 1)
	oldConnection := newControlledConnection(snapshotEvent())
	oldConnection.commandBlock = commandRelease
	oldConnection.commandStarted = oldCommandStarted
	newConnection := newControlledConnection(snapshotEvent())
	backend := &queuedBackend{connections: []*controlledConnection{oldConnection, newConnection}}
	_, baseURL, client := startServerWithBackend(t, backend)

	post := func(ctx context.Context, path string, command agentprotocol.Command) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(encodeCommand(t, command)))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Origin", trustedOrigin)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
		return client.Do(request)
	}

	first, err := post(context.Background(), agentprotocol.ConnectPath, connectCommand())
	require.NoError(t, err)
	_, err = bufio.NewReader(first.Body).ReadBytes('\n')
	require.NoError(t, err)

	oldCommandDone := make(chan error, 1)
	go func() {
		response, requestErr := post(context.Background(), CommandsPath, ordinaryCommand())
		if response != nil {
			response.Body.Close()
		}
		oldCommandDone <- requestErr
	}()
	select {
	case <-oldCommandStarted:
	case <-time.After(time.Second):
		t.Fatal("old command did not reach backend")
	}

	reconnectDone := make(chan *http.Response, 1)
	reconnectErr := make(chan error, 1)
	go func() {
		response, requestErr := post(context.Background(), agentprotocol.ConnectPath, connectCommand())
		if requestErr != nil {
			reconnectErr <- requestErr
			return
		}
		reconnectDone <- response
	}()
	select {
	case response := <-reconnectDone:
		response.Body.Close()
		close(commandRelease)
		t.Fatal("reconnect first event became observable while an old command was still dispatching")
	case err := <-reconnectErr:
		close(commandRelease)
		require.NoError(t, err)
	case <-time.After(100 * time.Millisecond):
	}

	close(commandRelease)
	select {
	case err = <-oldCommandDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("old command did not finish")
	}
	var second *http.Response
	select {
	case second = <-reconnectDone:
	case err = <-reconnectErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("reconnect did not publish after the old command finished")
	}
	_, err = bufio.NewReader(second.Body).ReadBytes('\n')
	require.NoError(t, err)

	response, err := post(context.Background(), CommandsPath, ordinaryCommand())
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	response.Body.Close()
	oldConnection.mu.Lock()
	oldCommands := oldConnection.commands
	oldConnection.mu.Unlock()
	newConnection.mu.Lock()
	newCommands := newConnection.commands
	newConnection.mu.Unlock()
	require.Equal(t, 1, oldCommands)
	require.Equal(t, 1, newCommands)
	first.Body.Close()
	second.Body.Close()
}

func TestFallbackFirstEventRequiresSnapshotUnlessReplayRequested(t *testing.T) {
	for _, test := range []struct {
		name         string
		replay       bool
		firstEventID string
		wantStatus   int
	}{
		{name: "fresh", firstEventID: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", wantStatus: http.StatusServiceUnavailable},
		{name: "replay", replay: true, firstEventID: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", wantStatus: http.StatusOK},
		{name: "replay-cursor-is-not-after", replay: true, firstEventID: eventID, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := resultEvent(commandID)
			first.EventID = test.firstEventID
			connection := newControlledConnection(first)
			_, baseURL, client := startServerWithBackend(t, &queuedBackend{connections: []*controlledConnection{connection}})
			connect := connectCommand()
			if test.replay {
				payload := connect.Payload.(agentprotocol.ConnectPayload)
				payload.ReplayAfter = eventID
				connect.Payload = payload
			}
			request, err := http.NewRequest(http.MethodPost, baseURL+agentprotocol.ConnectPath, bytes.NewReader(encodeCommand(t, connect)))
			require.NoError(t, err)
			request.Header.Set("Origin", trustedOrigin)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
			response, err := client.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, test.wantStatus, response.StatusCode)
			if test.wantStatus == http.StatusOK {
				_, err = bufio.NewReader(response.Body).ReadBytes('\n')
				require.NoError(t, err)
			}
		})
	}
}

func TestWebSocketFirstEventRequiresSnapshotUnlessReplayRequested(t *testing.T) {
	for _, test := range []struct {
		name         string
		replay       bool
		firstEventID string
		accepted     bool
	}{
		{name: "fresh", firstEventID: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"},
		{name: "replay", replay: true, firstEventID: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", accepted: true},
		{name: "replay-cursor-is-not-after", replay: true, firstEventID: eventID},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := resultEvent(commandID)
			first.EventID = test.firstEventID
			connection := newControlledConnection(first)
			server, _, _ := startServerWithBackend(t, &queuedBackend{connections: []*controlledConnection{connection}})
			wsURL := url.URL{Scheme: "ws", Host: server.Host(), Path: agentprotocol.ConnectPath}
			dialer := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
			socket, _, err := dialer.Dial(wsURL.String(), http.Header{"Origin": []string{trustedOrigin}})
			require.NoError(t, err)
			defer socket.Close()
			connect := connectCommand()
			if test.replay {
				payload := connect.Payload.(agentprotocol.ConnectPayload)
				payload.ReplayAfter = eventID
				connect.Payload = payload
			}
			require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connect)))
			_ = socket.SetReadDeadline(time.Now().Add(time.Second))
			_, event, readErr := socket.ReadMessage()
			if test.accepted {
				require.NoError(t, readErr)
				decoded, decodeErr := agentprotocol.DecodeEvent(event)
				require.NoError(t, decodeErr)
				require.Equal(t, agentprotocol.EventCommandResult, decoded.Type)
				return
			}
			require.Error(t, readErr)
		})
	}
}

func TestConnectionCloseDeadlineIsBoundedAndLaterHealthyCloseRetries(t *testing.T) {
	release := make(chan struct{})
	connection := newControlledConnection()
	connection.closeBlock = release
	safe := newSafeConnection(connection)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	started := time.Now()
	err := safe.Close(ctx)
	cancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 250*time.Millisecond)

	close(release)
	require.NoError(t, safe.Close(context.Background()))
	select {
	case <-connection.closed:
	case <-time.After(time.Second):
		t.Fatal("healthy close retry did not close the connection")
	}
	connection.mu.Lock()
	require.Equal(t, 2, connection.closeCalls)
	connection.mu.Unlock()
}

func TestNoncooperativeConnectionCloseRunsSynchronouslyWithoutLocalAPIGoroutine(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	connection := newControlledConnection()
	connection.closeBlock = release
	connection.closeStarted = started
	connection.ignoreCloseCtx = true
	safe := newSafeConnection(connection)

	returned := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		returned <- safe.Close(ctx)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("backend close did not start")
	}
	select {
	case err := <-returned:
		close(release)
		t.Fatalf("noncooperative close returned before backend release: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-returned:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("synchronous close did not return after backend release")
	}
}

func TestServerCloseCanRetryAfterCanceledAttempt(t *testing.T) {
	release := make(chan struct{})
	connection := newControlledConnection(snapshotEvent())
	connection.closeBlock = release
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

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err = server.Close(canceled)
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	ctx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	require.NoError(t, server.Close(ctx))
	select {
	case <-connection.closed:
	case <-time.After(time.Second):
		t.Fatal("retry did not close backend connection")
	}
	response.Body.Close()
}

func TestServerCloseCancelsCooperativeFallbackCommand(t *testing.T) {
	commandBlock := make(chan struct{})
	commandStarted := make(chan struct{}, 1)
	connection := newControlledConnection(snapshotEvent())
	connection.commandBlock = commandBlock
	connection.commandStarted = commandStarted
	server, baseURL, client := startServerWithBackend(t, &queuedBackend{connections: []*controlledConnection{connection}})

	streamRequest, err := http.NewRequest(http.MethodPost, baseURL+agentprotocol.ConnectPath, bytes.NewReader(encodeCommand(t, connectCommand())))
	require.NoError(t, err)
	streamRequest.Header.Set("Origin", trustedOrigin)
	streamRequest.Header.Set("Content-Type", "application/json")
	streamRequest.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	stream, err := client.Do(streamRequest)
	require.NoError(t, err)
	_, err = bufio.NewReader(stream.Body).ReadBytes('\n')
	require.NoError(t, err)

	commandDone := make(chan error, 1)
	go func() {
		request, requestErr := http.NewRequest(http.MethodPost, baseURL+CommandsPath, bytes.NewReader(encodeCommand(t, ordinaryCommand())))
		if requestErr == nil {
			request.Header.Set("Origin", trustedOrigin)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
			var response *http.Response
			response, requestErr = client.Do(request)
			if response != nil {
				response.Body.Close()
			}
		}
		commandDone <- requestErr
	}()
	select {
	case <-commandStarted:
	case <-time.After(time.Second):
		t.Fatal("fallback command did not reach backend")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, server.Close(ctx))
	select {
	case <-commandDone:
	case <-time.After(time.Second):
		t.Fatal("fallback command handler remained blocked after server close")
	}
	stream.Body.Close()
}

func TestWebSocketDisconnectBeforeFirstEventClosesConnection(t *testing.T) {
	connection := newControlledConnection()
	connected := make(chan struct{}, 1)
	server, _, _ := startServerWithBackend(t, &queuedBackend{
		connections: []*controlledConnection{connection},
		connected:   connected,
	})
	wsURL := url.URL{Scheme: "ws", Host: server.Host(), Path: agentprotocol.ConnectPath}
	dialer := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
	socket, _, err := dialer.Dial(wsURL.String(), http.Header{"Origin": []string{trustedOrigin}})
	require.NoError(t, err)
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("WebSocket connect did not reach backend")
	}
	require.NoError(t, socket.Close())

	select {
	case <-connection.closed:
	case <-time.After(time.Second):
		t.Fatal("peer disconnect before first event did not close backend connection")
	}
	require.Eventually(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		return len(server.transports) == 0
	}, time.Second, time.Millisecond)
}

func TestServerCloseCancelsCooperativeWebSocketCommand(t *testing.T) {
	commandBlock := make(chan struct{})
	commandStarted := make(chan struct{}, 1)
	connection := newControlledConnection(snapshotEvent())
	connection.commandBlock = commandBlock
	connection.commandStarted = commandStarted
	server, _, _ := startServerWithBackend(t, &queuedBackend{connections: []*controlledConnection{connection}})
	wsURL := url.URL{Scheme: "ws", Host: server.Host(), Path: agentprotocol.ConnectPath}
	dialer := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
	socket, _, err := dialer.Dial(wsURL.String(), http.Header{"Origin": []string{trustedOrigin}})
	require.NoError(t, err)
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
	_, _, err = socket.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, ordinaryCommand())))
	select {
	case <-commandStarted:
	case <-time.After(time.Second):
		t.Fatal("WebSocket command did not reach backend")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, server.Close(ctx))
	require.NoError(t, socket.Close())
}
