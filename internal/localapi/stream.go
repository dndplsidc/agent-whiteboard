package localapi

import (
	"context"
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
	cancel     context.CancelFunc
	done       chan struct{}
	doneOnce   sync.Once
	lease      sync.RWMutex
}

func (a *attachment) close(ctx context.Context) error {
	a.doneOnce.Do(func() {
		close(a.done)
		a.cancel()
	})
	return a.connection.Close(ctx)
}

type safeConnection struct {
	Connection
	closeState closeState
}

func newSafeConnection(connection Connection) *safeConnection {
	return &safeConnection{Connection: connection}
}

func (c *safeConnection) Close(ctx context.Context) error {
	return c.closeState.run(ctx, c.Connection.Close)
}

const attachmentLockCount = 64

type attachmentRegistry struct {
	mu    sync.RWMutex
	items map[attachmentKey]*attachment
	locks [attachmentLockCount]sync.Mutex
}

func (r *attachmentRegistry) keyLock(key attachmentKey) *sync.Mutex {
	hash := uint32(2166136261)
	for _, value := range []string{key.origin, key.clientID, key.conversationID} {
		for index := 0; index < len(value); index++ {
			hash ^= uint32(value[index])
			hash *= 16777619
		}
		hash ^= 0
		hash *= 16777619
	}
	return &r.locks[hash%attachmentLockCount]
}

func (r *attachmentRegistry) put(item *attachment) *attachment {
	lock := r.keyLock(item.key)
	lock.Lock()
	defer lock.Unlock()
	return r.replace(item)
}

func (r *attachmentRegistry) replace(item *attachment) *attachment {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = make(map[attachmentKey]*attachment)
	}
	old := r.items[item.key]
	r.items[item.key] = item
	return old
}

func (r *attachmentRegistry) activate(item *attachment, publish func() error) (*attachment, error) {
	lock := r.keyLock(item.key)
	lock.Lock()
	defer lock.Unlock()

	item.lease.Lock()
	defer item.lease.Unlock()
	r.mu.RLock()
	old := r.items[item.key]
	r.mu.RUnlock()
	if old != nil {
		old.lease.Lock()
		defer old.lease.Unlock()
	}
	r.replace(item)
	if err := publish(); err != nil {
		r.mu.Lock()
		if r.items[item.key] == item {
			if old == nil {
				delete(r.items, item.key)
			} else {
				r.items[item.key] = old
			}
		}
		r.mu.Unlock()
		return nil, err
	}
	return old, nil
}

func (r *attachmentRegistry) remove(item *attachment) {
	lock := r.keyLock(item.key)
	lock.Lock()
	defer lock.Unlock()
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

func (r *attachmentRegistry) command(ctx context.Context, key attachmentKey, command agentprotocol.Command) (agentprotocol.Event, bool, error) {
	for {
		r.mu.RLock()
		item := r.items[key]
		r.mu.RUnlock()
		if item == nil {
			return agentprotocol.Event{}, false, nil
		}
		item.lease.RLock()
		r.mu.RLock()
		current := r.items[key] == item
		r.mu.RUnlock()
		if !current {
			item.lease.RUnlock()
			continue
		}
		event, err := item.connection.Command(ctx, command)
		item.lease.RUnlock()
		return event, true, err
	}
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
	ctx, cancel := context.WithCancel(request.Context())
	connection, err := s.backend.Connect(ctx, origin, command)
	if err != nil || !validConnection(connection) {
		cancel()
		closeRejectedConnection(request.Context(), connection)
		safeHTTPError(response, http.StatusServiceUnavailable, backendErrorCode(err, agentprotocol.ErrorBrokerUnavailable))
		return
	}
	safe := newSafeConnection(connection)
	item := &attachment{
		key:        attachmentKey{origin: origin, clientID: command.ClientID, conversationID: connection.ConversationID()},
		connection: safe,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	tracked := s.track(item.close)
	defer func() {
		s.attachments.remove(item)
		closeCtx, closeCancel := context.WithTimeout(context.Background(), transportCleanupTimeout)
		defer closeCancel()
		if tracked.close(closeCtx) == nil {
			s.untrack(tracked)
		}
	}()

	if !supportsResponseFlush(response) {
		safeHTTPError(response, http.StatusInternalServerError, agentprotocol.ErrorBrokerUnavailable)
		return
	}

	var first agentprotocol.Event
	select {
	case event, open := <-safe.Events():
		if !open {
			safeHTTPError(response, http.StatusServiceUnavailable, agentprotocol.ErrorBrokerUnavailable)
			return
		}
		first = event
	case <-item.done:
		safeHTTPError(response, http.StatusServiceUnavailable, agentprotocol.ErrorBrokerShuttingDown)
		return
	case <-request.Context().Done():
		return
	}
	encoded, err := encodeFirstConnectionEvent(safe, command, first)
	if err != nil {
		safeHTTPError(response, http.StatusServiceUnavailable, agentprotocol.ErrorBrokerUnavailable)
		return
	}
	old, err := s.attachments.activate(item, func() error {
		response.Header().Set("Content-Type", "application/x-ndjson")
		response.WriteHeader(http.StatusOK)
		if _, writeErr := response.Write(append(encoded, '\n')); writeErr != nil {
			return writeErr
		}
		return http.NewResponseController(response).Flush()
	})
	if err != nil {
		return
	}
	if old != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), transportCleanupTimeout)
		_ = old.close(closeCtx)
		closeCancel()
	}

	for {
		select {
		case event, open := <-safe.Events():
			if !open {
				return
			}
			encoded, err := encodeConnectionEvent(safe, event)
			if err != nil {
				return
			}
			if _, err = response.Write(append(encoded, '\n')); err != nil {
				return
			}
			if err = http.NewResponseController(response).Flush(); err != nil {
				return
			}
		case <-item.done:
			return
		case <-request.Context().Done():
			return
		}
	}
}

func supportsResponseFlush(response http.ResponseWriter) bool {
	for {
		if unwrapper, ok := response.(interface{ Unwrap() http.ResponseWriter }); ok {
			underlying := unwrapper.Unwrap()
			if underlying != nil && underlying != response {
				response = underlying
				continue
			}
		}
		if _, ok := response.(interface{ FlushError() error }); ok {
			return true
		}
		_, ok := response.(http.Flusher)
		return ok
	}
}

func encodeConnectionEvent(connection *safeConnection, event agentprotocol.Event) ([]byte, error) {
	if event.ConversationID != connection.ConversationID() {
		return nil, errors.New("event conversation mismatch")
	}
	return agentprotocol.EncodeEvent(event)
}

func encodeFirstConnectionEvent(connection *safeConnection, connect agentprotocol.Command, event agentprotocol.Event) ([]byte, error) {
	payload, ok := connect.Payload.(agentprotocol.ConnectPayload)
	if !ok {
		return nil, errors.New("connect payload mismatch")
	}
	if payload.ReplayAfter == "" {
		if event.Type != agentprotocol.EventSnapshot {
			return nil, errors.New("fresh connection must begin with snapshot")
		}
	} else if event.EventID == payload.ReplayAfter {
		return nil, errors.New("replay must begin after requested event")
	}
	return encodeConnectionEvent(connection, event)
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
	event, attached, err := s.attachments.command(request.Context(), attachmentKey{origin: origin, clientID: command.ClientID, conversationID: *command.ConversationID}, command)
	if !attached {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}
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

func closeRejectedConnection(ctx context.Context, connection Connection) {
	if nilInterface(connection) {
		return
	}
	closeCtx, cancel := context.WithTimeout(ctx, transportCleanupTimeout)
	defer cancel()
	_ = newSafeConnection(connection).Close(closeCtx)
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
