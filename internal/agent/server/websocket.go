package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	Subprotocols:      []string{protocol.WebSocketSubprotocol},
	EnableCompression: false,
	CheckOrigin:       func(*http.Request) bool { return true }, // checked canonically before upgrade
}

func (s *Server) websocket(response http.ResponseWriter, request *http.Request) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, protocol.ErrorUntrustedOrigin)
		return
	}
	if !s.trusted(request.Context(), origin) {
		safeHTTPError(response, http.StatusForbidden, protocol.ErrorUntrustedOrigin)
		return
	}
	if !websocket.IsWebSocketUpgrade(request) || !offersRequiredSubprotocol(request) {
		allowOrigin(response.Header(), origin)
		safeHTTPError(response, http.StatusUpgradeRequired, protocol.ErrorIncompatibleAPI)
		return
	}

	socket, err := upgrader.Upgrade(response, request, nil)
	if err != nil {
		return
	}
	defer socket.Close()
	socket.EnableWriteCompression(false)
	socket.SetReadLimit(int64(protocol.MaxContextCommandBytes))
	_ = socket.SetReadDeadline(time.Now().Add(firstFrameTimeout))

	messageType, data, err := socket.ReadMessage()
	if err != nil {
		closeWebSocket(socket, closeCode(err), "connect frame rejected")
		return
	}
	if messageType != websocket.TextMessage {
		closeWebSocket(socket, websocket.CloseUnsupportedData, "text frames required")
		return
	}
	connect, err := protocol.DecodeCommand(data)
	if err != nil || connect.Type != protocol.CommandConnect {
		closeWebSocket(socket, closeCode(err), "invalid connect frame")
		return
	}
	_ = socket.SetReadDeadline(time.Time{})

	ctx, cancel := context.WithCancel(request.Context())
	connection, err := s.backend.Connect(ctx, origin, connect)
	if err != nil || !validConnection(connection) {
		cancel()
		closeRejectedConnection(request.Context(), connection)
		closeWebSocket(socket, websocket.CloseInternalServerErr, "broker unavailable")
		return
	}
	safe := newSafeConnection(connection)
	item := &browserAttachment{
		key:        attachmentKey{origin: origin, clientID: connect.ClientID, conversationID: connection.ConversationID()},
		connection: safe,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	tracked := s.track(func(closeCtx context.Context) error {
		closeWebSocket(socket, websocket.CloseGoingAway, "broker shutting down")
		socketErr := socket.Close()
		if isClosedError(socketErr) {
			socketErr = nil
		}
		return errors.Join(socketErr, item.close(closeCtx))
	})
	defer func() {
		s.attachments.remove(item)
		closeCtx, closeCancel := context.WithTimeout(context.Background(), transportCleanupTimeout)
		defer closeCancel()
		if tracked.close(closeCtx) == nil {
			s.untrack(tracked)
		}
	}()

	commands := make(chan protocol.Command, 1)
	readDone := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			kind, frame, readErr := socket.ReadMessage()
			if readErr != nil {
				readDone <- readErr
				return
			}
			if kind != websocket.TextMessage {
				closeWebSocket(socket, websocket.CloseUnsupportedData, "text frames required")
				readDone <- errors.New("binary frame")
				return
			}
			command, decodeErr := protocol.DecodeCommand(frame)
			if decodeErr != nil || command.Type == protocol.CommandConnect || command.ConversationID == nil || command.ClientID != connect.ClientID || *command.ConversationID != safe.ConversationID() {
				closeWebSocket(socket, closeCode(decodeErr), "invalid command frame")
				readDone <- errors.New("invalid command")
				return
			}
			select {
			case commands <- command:
			case <-ctx.Done():
				readDone <- ctx.Err()
				return
			default:
				closeWebSocket(socket, websocket.ClosePolicyViolation, "command already pending")
				readDone <- errors.New("command already pending")
				return
			}
		}
	}()

	var first protocol.Event
	select {
	case event, open := <-safe.Events():
		if !open {
			closeWebSocket(socket, websocket.CloseInternalServerErr, "broker unavailable")
			cancel()
			_ = socket.Close()
			workers.Wait()
			return
		}
		first = event
	case <-readDone:
		cancel()
		_ = socket.Close()
		workers.Wait()
		return
	case <-ctx.Done():
		_ = socket.Close()
		workers.Wait()
		return
	}
	encodedFirst, err := encodeFirstConnectionEvent(safe, connect, first)
	if err != nil {
		closeWebSocket(socket, websocket.CloseInternalServerErr, "broker protocol failure")
		cancel()
		_ = socket.Close()
		workers.Wait()
		return
	}
	old, err := s.attachments.activate(item, func() error {
		return socket.WriteMessage(websocket.TextMessage, encodedFirst)
	})
	if err != nil {
		cancel()
		_ = socket.Close()
		workers.Wait()
		return
	}
	if old != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), transportCleanupTimeout)
		_ = old.close(closeCtx)
		closeCancel()
	}

	writerDone := make(chan error, 1)
	commandDone := make(chan error, 1)
	workers.Add(2)
	go func() {
		defer workers.Done()
		events := safe.Events()
		for {
			select {
			case <-ctx.Done():
				writerDone <- ctx.Err()
				return
			case event, open := <-events:
				if !open {
					writerDone <- nil
					return
				}
				encoded, encodeErr := encodeConnectionEvent(safe, event)
				if encodeErr != nil {
					writerDone <- encodeErr
					return
				}
				if writeErr := socket.WriteMessage(websocket.TextMessage, encoded); writeErr != nil {
					writerDone <- writeErr
					return
				}
			}
		}
	}()

	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				commandDone <- ctx.Err()
				return
			case command := <-commands:
				result, commandErr := safe.Command(ctx, command)
				if commandErr != nil {
					closeWebSocket(socket, websocket.ClosePolicyViolation, "command rejected")
					commandDone <- commandErr
					return
				}
				if !matchingCommandResult(result, command) {
					closeWebSocket(socket, websocket.CloseInternalServerErr, "broker protocol failure")
					commandDone <- errors.New("invalid command result")
					return
				}
			}
		}
	}()

	select {
	case <-writerDone:
	case <-readDone:
	case <-commandDone:
	case <-ctx.Done():
	}
	cancel()
	_ = socket.Close()
	workers.Wait()
}

func offersRequiredSubprotocol(request *http.Request) bool {
	protocols := websocket.Subprotocols(request)
	for _, offered := range protocols {
		if offered == protocol.WebSocketSubprotocol {
			return true
		}
	}
	return false
}

func closeCode(err error) int {
	if errors.Is(err, protocol.ErrMessageTooLarge) {
		return websocket.CloseMessageTooBig
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig {
		return websocket.CloseMessageTooBig
	}
	return websocket.ClosePolicyViolation
}

func closeWebSocket(socket *websocket.Conn, code int, reason string) {
	_ = socket.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}
