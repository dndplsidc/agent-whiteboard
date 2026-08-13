package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

func (session *Session) SupportsCompact() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return !session.closed && session.supportsCompact
}

func (session *Session) Compact(ctx context.Context, request provider.CompactRequest) (provider.AcceptedCompact, error) {
	if request.Validate() != nil {
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	compact := &nativeCompact{request: request}
	session.mu.Lock()
	if session.closed || !session.supportsCompact {
		session.mu.Unlock()
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorCompactUnsupported)
	}
	if session.active != nil || session.compact != nil {
		session.mu.Unlock()
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	session.compact = compact
	session.mu.Unlock()
	result, releaseStream, err := session.runtime.callOrdered(ctx, "thread/compact/start", map[string]any{"threadId": session.threadID})
	if err != nil {
		releaseStream()
		session.mu.Lock()
		if session.compact == compact {
			session.compact = nil
		}
		if errors.Is(err, errMethodNotFound) {
			session.supportsCompact = false
		}
		session.mu.Unlock()
		if errors.Is(err, errMethodNotFound) {
			return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorCompactUnsupported)
		}
		return provider.AcceptedCompact{}, err
	}
	defer releaseStream()
	var response map[string]json.RawMessage
	acceptedAt := session.driver.config.Clock.Now().UTC()
	if validateJSONStructure(result) != nil || json.Unmarshal(result, &response) != nil || len(response) != 0 || acceptedAt.IsZero() {
		return provider.AcceptedCompact{}, session.abandonCompact(compact, provider.ErrorAcceptanceUnknown)
	}
	session.mu.Lock()
	if session.compact != compact {
		session.mu.Unlock()
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	compact.accepted = true
	buffered := append([]bufferedNativeEvent(nil), compact.buffered...)
	compact.buffered = nil
	compact.bytes = 0
	deferred := compact.pendingInterrupt && compact.nativeID != "" && !compact.interruptSent
	if deferred {
		compact.interruptSent = true
	}
	session.mu.Unlock()
	for _, event := range buffered {
		session.processCompactNotification(compact, event.method, event.params)
	}
	if deferred {
		go session.sendDeferredCompactInterrupt(compact)
	}
	return provider.AcceptedCompact{WorkID: request.WorkID, AcceptedAt: acceptedAt}, nil
}

func (session *Session) InterruptCompact(ctx context.Context, accepted provider.AcceptedCompact) error {
	if accepted.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	session.mu.Lock()
	compact := session.compact
	if compact == nil || compact.request.WorkID != accepted.WorkID || !compact.accepted {
		session.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if compact.interruptSent || compact.pendingInterrupt {
		session.mu.Unlock()
		return nil
	}
	if compact.nativeID == "" {
		compact.pendingInterrupt = true
		session.mu.Unlock()
		return nil
	}
	compact.interruptSent = true
	nativeID := compact.nativeID
	session.mu.Unlock()
	_, err := session.runtime.call(ctx, "turn/interrupt", map[string]any{"threadId": session.threadID, "turnId": nativeID})
	return err
}

func (session *Session) captureCompactNotification(method string, params json.RawMessage) bool {
	nativeID := notificationTurnID(method, params)
	session.mu.Lock()
	compact := session.compact
	if compact == nil {
		session.mu.Unlock()
		return false
	}
	if nativeID == "" {
		session.mu.Unlock()
		return true
	}
	if compact.nativeID == "" {
		compact.nativeID = nativeID
	} else if compact.nativeID != nativeID {
		session.compact = nil
		workID := compact.request.WorkID
		session.mu.Unlock()
		session.emit(provider.NewCompactEvent(workID, provider.CompactFailed))
		return true
	}
	if !compact.accepted {
		size := len(method) + len(params)
		if len(compact.buffered) >= provider.MaxLaunchItems || compact.bytes+size > maxJSONLMessageBytes {
			session.compact = nil
			workID := compact.request.WorkID
			session.mu.Unlock()
			session.emit(provider.NewCompactEvent(workID, provider.CompactFailed))
			return true
		}
		compact.buffered = append(compact.buffered, bufferedNativeEvent{method: method, params: bytes.Clone(params)})
		compact.bytes += size
		session.mu.Unlock()
		return true
	}
	deferred := compact.pendingInterrupt && !compact.interruptSent
	if deferred {
		compact.interruptSent = true
	}
	session.mu.Unlock()
	if deferred {
		go session.sendDeferredCompactInterrupt(compact)
	}
	session.processCompactNotification(compact, method, params)
	return true
}

func (session *Session) processCompactNotification(compact *nativeCompact, method string, params json.RawMessage) {
	if method != "turn/completed" {
		return
	}
	var notification struct {
		Turn *struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
	}
	status := provider.CompactFailed
	valid := json.Unmarshal(params, &notification) == nil && notification.Turn != nil && notification.Turn.ID != "" && notification.Turn.ID == compact.nativeID
	if valid {
		switch notification.Turn.Status {
		case "completed":
			status = provider.CompactCompleted
		case "interrupted":
			status = provider.CompactInterrupted
		case "failed":
			valid = len(bytes.TrimSpace(notification.Turn.Error)) != 0 && !isJSONNull(notification.Turn.Error)
		default:
			valid = false
		}
	}
	if !valid {
		status = provider.CompactFailed
	}
	session.mu.Lock()
	if session.compact != compact {
		session.mu.Unlock()
		return
	}
	session.compact = nil
	workID := compact.request.WorkID
	session.mu.Unlock()
	session.emit(provider.NewCompactEvent(workID, status))
}

func (session *Session) sendDeferredCompactInterrupt(compact *nativeCompact) {
	_, err := session.runtime.call(context.Background(), "turn/interrupt", map[string]any{"threadId": session.threadID, "turnId": compact.nativeID})
	if err == nil {
		return
	}
	session.mu.Lock()
	if session.compact != compact {
		session.mu.Unlock()
		return
	}
	session.compact = nil
	workID := compact.request.WorkID
	session.mu.Unlock()
	session.emit(provider.NewCompactEvent(workID, provider.CompactFailed))
}

func (session *Session) abandonCompact(compact *nativeCompact, code provider.ProviderErrorCode) error {
	session.mu.Lock()
	if session.compact == compact {
		session.compact = nil
	}
	session.mu.Unlock()
	return provider.NewProviderError(code)
}
