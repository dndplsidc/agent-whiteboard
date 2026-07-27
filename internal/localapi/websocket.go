package localapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	Subprotocols:      []string{agentprotocol.WebSocketSubprotocol},
	EnableCompression: false,
	CheckOrigin:       func(*http.Request) bool { return true }, // checked canonically before upgrade
}

func (s *Server) websocket(response http.ResponseWriter, request *http.Request) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}
	if !s.trusted(request.Context(), origin) {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}
	if !websocket.IsWebSocketUpgrade(request) || !offersRequiredSubprotocol(request) {
		allowOrigin(response.Header(), origin)
		safeHTTPError(response, http.StatusUpgradeRequired, agentprotocol.ErrorIncompatibleAPI)
		return
	}

	socket, err := upgrader.Upgrade(response, request, nil)
	if err != nil {
		return
	}
	defer socket.Close()
	socket.EnableWriteCompression(false)
	socket.SetReadLimit(int64(agentprotocol.MaxContextCommandBytes))
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
	connect, err := agentprotocol.DecodeCommand(data)
	if err != nil || connect.Type != agentprotocol.CommandConnect {
		closeWebSocket(socket, closeCode(err), "invalid connect frame")
		return
	}
	_ = socket.SetReadDeadline(time.Time{})

	ctx, cancel := context.WithCancel(request.Context())
	connection, err := s.backend.Connect(ctx, origin, connect)
	if err != nil || !validConnection(connection) {
		cancel()
		closeRejectedConnection(connection)
		closeWebSocket(socket, websocket.CloseInternalServerErr, "broker unavailable")
		return
	}
	safe := &safeConnection{Connection: connection}
	tracked := s.track(func() error {
		cancel()
		closeWebSocket(socket, websocket.CloseGoingAway, "broker shutting down")
		return errors.Join(socket.Close(), safe.Close())
	})
	defer func() {
		_ = tracked.close()
		s.untrack(tracked)
	}()

	writerDone := make(chan error, 1)
	go func() {
		for event := range safe.Events() {
			if event.ConversationID != safe.ConversationID() {
				writerDone <- errors.New("event conversation mismatch")
				return
			}
			encoded, encodeErr := agentprotocol.EncodeEvent(event)
			if encodeErr != nil {
				writerDone <- encodeErr
				return
			}
			if writeErr := socket.WriteMessage(websocket.TextMessage, encoded); writeErr != nil {
				writerDone <- writeErr
				return
			}
		}
		writerDone <- nil
	}()

	readDone := make(chan error, 1)
	go func() {
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
			command, decodeErr := agentprotocol.DecodeCommand(frame)
			if decodeErr != nil || command.Type == agentprotocol.CommandConnect || command.ConversationID == nil || command.ClientID != connect.ClientID || *command.ConversationID != safe.ConversationID() {
				closeWebSocket(socket, closeCode(decodeErr), "invalid command frame")
				readDone <- errors.New("invalid command")
				return
			}
			result, commandErr := safe.Command(ctx, command)
			if commandErr != nil {
				closeWebSocket(socket, websocket.ClosePolicyViolation, "command rejected")
				readDone <- commandErr
				return
			}
			if !matchingCommandResult(result, command) {
				closeWebSocket(socket, websocket.CloseInternalServerErr, "broker protocol failure")
				readDone <- errors.New("invalid command result")
				return
			}
		}
	}()

	select {
	case <-writerDone:
	case <-readDone:
	case <-ctx.Done():
	}
}

func offersRequiredSubprotocol(request *http.Request) bool {
	protocols := websocket.Subprotocols(request)
	for _, protocol := range protocols {
		if protocol == agentprotocol.WebSocketSubprotocol {
			return true
		}
	}
	return false
}

func closeCode(err error) int {
	if errors.Is(err, agentprotocol.ErrMessageTooLarge) {
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
