package localapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

type attachmentKey struct {
	origin, clientID, conversationID string
}

type attachment struct {
	key        attachmentKey
	connection *safeConnection
	done       chan struct{}
	doneOnce   sync.Once
}

func (a *attachment) close() error {
	a.doneOnce.Do(func() { close(a.done) })
	return a.connection.Close()
}

type safeConnection struct {
	Connection
	once sync.Once
	err  error
}

func (c *safeConnection) Close() error {
	c.once.Do(func() { c.err = c.Connection.Close() })
	return c.err
}

type attachmentRegistry struct {
	mu    sync.RWMutex
	items map[attachmentKey]*attachment
}

func (r *attachmentRegistry) put(item *attachment) *attachment {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = make(map[attachmentKey]*attachment)
	}
	old := r.items[item.key]
	r.items[item.key] = item
	return old
}

func (r *attachmentRegistry) remove(item *attachment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items[item.key] == item {
		delete(r.items, item.key)
	}
}

func (r *attachmentRegistry) get(key attachmentKey) *attachment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.items[key]
}

func (r *attachmentRegistry) hasOrigin(origin string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key := range r.items {
		if key.origin == origin {
			return true
		}
	}
	return false
}

func (s *Server) stream(response http.ResponseWriter, request *http.Request) {
	origin, ok := s.authorizeMutation(response, request, true)
	if !ok {
		return
	}
	command, err := decodeCommandBody(request)
	if err != nil || command.Type != agentprotocol.CommandConnect {
		safeHTTPError(response, statusForDecodeError(err), agentprotocol.ErrorInvalidCommand)
		return
	}
	connection, err := s.backend.Connect(request.Context(), origin, command)
	if err != nil || !validConnection(connection) {
		closeRejectedConnection(connection)
		safeHTTPError(response, http.StatusServiceUnavailable, backendErrorCode(err, agentprotocol.ErrorBrokerUnavailable))
		return
	}
	safe := &safeConnection{Connection: connection}
	item := &attachment{
		key:        attachmentKey{origin: origin, clientID: command.ClientID, conversationID: connection.ConversationID()},
		connection: safe,
		done:       make(chan struct{}),
	}
	if old := s.attachments.put(item); old != nil {
		_ = old.close()
	}
	tracked := s.track(item.close)
	defer func() {
		s.attachments.remove(item)
		_ = tracked.close()
		s.untrack(tracked)
	}()

	flusher, ok := response.(http.Flusher)
	if !ok {
		safeHTTPError(response, http.StatusInternalServerError, agentprotocol.ErrorBrokerUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/x-ndjson")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case event, open := <-safe.Events():
			if !open {
				return
			}
			encoded, encodeErr := agentprotocol.EncodeEvent(event)
			if encodeErr != nil || event.ConversationID != safe.ConversationID() {
				return
			}
			if _, writeErr := response.Write(append(encoded, '\n')); writeErr != nil {
				return
			}
			flusher.Flush()
		case <-item.done:
			return
		case <-request.Context().Done():
			return
		}
	}
}

func (s *Server) command(response http.ResponseWriter, request *http.Request) {
	origin, ok := s.authorizeMutation(response, request, false)
	if !ok {
		return
	}
	command, err := decodeCommandBody(request)
	if err != nil || command.Type == agentprotocol.CommandConnect || command.ConversationID == nil {
		safeHTTPError(response, statusForDecodeError(err), agentprotocol.ErrorInvalidCommand)
		return
	}
	item := s.attachments.get(attachmentKey{origin: origin, clientID: command.ClientID, conversationID: *command.ConversationID})
	if item == nil {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}
	event, err := item.connection.Command(request.Context(), command)
	if err != nil {
		safeHTTPError(response, http.StatusConflict, backendErrorCode(err, agentprotocol.ErrorInvalidState))
		return
	}
	if !matchingCommandResult(event, command) {
		safeHTTPError(response, http.StatusServiceUnavailable, agentprotocol.ErrorBrokerUnavailable)
		return
	}
	encoded, err := agentprotocol.EncodeEvent(event)
	if err != nil {
		safeHTTPError(response, http.StatusServiceUnavailable, agentprotocol.ErrorBrokerUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusAccepted)
	_, _ = response.Write(encoded)
}

func (s *Server) authorizeMutation(response http.ResponseWriter, request *http.Request, newAdmission bool) (string, bool) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return "", false
	}
	if status, code, ok := validateMutationHeaders(request); !ok {
		if (!newAdmission && s.attachments.hasOrigin(origin)) || (newAdmission && s.trusted(request.Context(), origin)) {
			allowOrigin(response.Header(), origin)
		}
		safeHTTPError(response, status, code)
		return "", false
	}
	if newAdmission && !s.trusted(request.Context(), origin) {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return "", false
	}
	if !newAdmission && !s.attachments.hasOrigin(origin) && !s.trusted(request.Context(), origin) {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return "", false
	}
	allowOrigin(response.Header(), origin)
	return origin, true
}

func validConnection(connection Connection) bool {
	return !nilInterface(connection) && common.ValidateID(connection.ConversationID()) == nil && connection.Events() != nil
}

func closeRejectedConnection(connection Connection) {
	if !nilInterface(connection) {
		_ = connection.Close()
	}
}

func matchingCommandResult(event agentprotocol.Event, command agentprotocol.Command) bool {
	if event.Type != agentprotocol.EventCommandResult || command.ConversationID == nil || event.ConversationID != *command.ConversationID {
		return false
	}
	payload, ok := event.Payload.(agentprotocol.CommandResultPayload)
	return ok && payload.CommandID == command.CommandID
}

type browserErrorCoder interface {
	BrowserErrorCode() agentprotocol.BrowserErrorCode
}

func backendErrorCode(err error, fallback agentprotocol.BrowserErrorCode) agentprotocol.BrowserErrorCode {
	var coded browserErrorCoder
	if errors.As(err, &coded) {
		code := coded.BrowserErrorCode()
		candidate := agentprotocol.NewBrowserError(code)
		if _, marshalErr := json.Marshal(candidate); marshalErr == nil {
			return code
		}
	}
	return fallback
}
