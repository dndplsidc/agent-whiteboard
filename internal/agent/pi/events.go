//go:build unix

package pi

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type nativeEvent struct {
	Type                  string          `json:"type"`
	Message               json.RawMessage `json:"message"`
	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent"`
	RequestID             string          `json:"requestId"`
	ID                    string          `json:"id"`
	Status                string          `json:"status"`
}
type nativeMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Timestamp  any             `json:"timestamp"`
	StopReason string          `json:"stopReason"`
}
type messageUpdate struct {
	Type   string `json:"type"`
	Delta  string `json:"delta"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

func (s *Session) handleNativeEvent(raw json.RawMessage) {
	var event nativeEvent
	if json.Unmarshal(raw, &event) != nil || event.Type == "" {
		s.rpc.finish(provider.NewProviderError(provider.ErrorMalformedStream))
		return
	}
	s.mu.Lock()
	turn := s.active
	if turn != nil {
		turn.nativeSeen = true
	}
	s.mu.Unlock()
	switch event.Type {
	case "message_start":
		if turn == nil {
			return
		}
		msg, ok := parseNativeMessage(event.Message)
		if !ok {
			s.failMalformed(turn)
			return
		}
		if msg.Role == "user" {
			text, ok := messageText(msg.Content)
			if !ok {
				s.failMalformed(turn)
				return
			}
			envelope, err := ParseEnvelope([]byte(text))
			if err != nil || envelope.TurnID != turn.request.TurnID || envelope.MessageID != turn.request.MessageID || !bytes.Equal([]byte(text), turn.envelope) {
				s.failMalformed(turn)
				return
			}
			s.mu.Lock()
			first := !turn.userEmitted
			turn.userEmitted = true
			s.mu.Unlock()
			if first {
				message := provider.NewUserMessageEvent(turn.request.TurnID, turn.request.MessageID, envelope.ReaderContent, eventTime(msg.Timestamp, s.driver.config.Clock.Now()))
				if message.Validate() != nil {
					s.failMalformed(turn)
					return
				}
				s.emit(message)
			}
		}
	case "message_update":
		if turn == nil {
			return
		}
		if !s.turnHasValidatedUser(turn) {
			s.failMalformed(turn)
			return
		}
		var update messageUpdate
		if json.Unmarshal(event.AssistantMessageEvent, &update) != nil || update.Type == "" {
			s.failMalformed(turn)
			return
		}
		switch update.Type {
		case "text_delta":
			candidate := provider.NewAssistantDeltaEvent(turn.request.TurnID, turn.assistantID, update.Delta)
			if update.Delta == "" || candidate.Validate() != nil {
				s.failMalformed(turn)
				return
			}
			s.emit(candidate)
		case "start", "text_start", "text_end", "done", "thinking_delta", "thinking_start", "thinking_end":
			return
		case "error":
			s.mu.Lock()
			interrupted := turn.interruptRequested || turn.abortSent || update.Reason == "aborted"
			if !interrupted {
				turn.providerFailed = true
			}
			s.mu.Unlock()
			return
		case "toolcall_start", "toolcall_delta", "toolcall_end":
			s.emitConfiguredActivity(turn, event.Type)
		default:
			if strings.HasPrefix(update.Type, "tool") {
				s.emitConfiguredActivity(turn, update.Type)
				return
			}
			s.failMalformed(turn)
		}
	case "message_end":
		if turn == nil {
			return
		}
		msg, ok := parseNativeMessage(event.Message)
		if !ok {
			s.failMalformed(turn)
			return
		}
		if msg.Role != "assistant" {
			return
		}
		if !s.turnHasValidatedUser(turn) {
			s.failMalformed(turn)
			return
		}
		if msg.StopReason == "error" {
			s.mu.Lock()
			turn.providerFailed = true
			s.mu.Unlock()
			return
		}
		if msg.StopReason == "aborted" {
			return
		}
		text, ok := messageText(msg.Content)
		if !ok {
			s.mu.Lock()
			blocked := turn.abortSent
			s.mu.Unlock()
			if blocked {
				return
			}
			s.failMalformed(turn)
			return
		}
		if text != "" {
			message := provider.NewAssistantMessageEvent(turn.request.TurnID, turn.assistantID, text, eventTime(msg.Timestamp, s.driver.config.Clock.Now()))
			if message.Validate() != nil {
				s.failMalformed(turn)
				return
			}
			s.mu.Lock()
			duplicate := turn.assistantEmitted
			turn.assistantEmitted = true
			s.mu.Unlock()
			if duplicate {
				s.failMalformed(turn)
				return
			}
			s.emit(message)
		}
	case "agent_settled":
		if turn == nil {
			return
		}
		if !s.turnHasValidatedUser(turn) {
			s.failMalformed(turn)
			return
		}
		s.mu.Lock()
		requested := turn.interruptRequested
		failed := turn.providerFailed
		s.mu.Unlock()
		if requested {
			s.emit(provider.NewInterruptionEvent(turn.request.TurnID, provider.InterruptionRequested))
		} else if failed {
			s.emit(provider.NewTerminalFailureEvent(turn.request.TurnID, provider.NewProviderError(provider.ErrorProtocolFailure)))
		} else {
			s.emit(provider.NewCompletionEvent(turn.request.TurnID))
		}
		s.finishTurn(turn, requested)
	case "agent_start", "agent_end", "turn_start", "turn_end", "entry_appended", "session_info_changed", "thinking_level_changed":
		return
	case "status", "queue_update", "retry", "compaction", "compaction_start", "compaction_end", "auto_compaction_start", "auto_compaction_end", "auto_retry_start", "auto_retry_end", "summarization_retry_scheduled", "summarization_retry_attempt_start", "summarization_retry_finished":
		if turn == nil {
			return
		}
		if !s.turnHasValidatedUser(turn) {
			return
		}
		summary := normalizedSummary(event.Type, event.Status)
		if summary != "" {
			kind := provider.ActivityStatus
			if strings.Contains(event.Type, "retry") {
				kind = provider.ActivityRetry
			}
			if strings.Contains(event.Type, "compaction") {
				kind = provider.ActivityCompaction
			}
			s.emit(provider.NewActivityEvent(turn.request.TurnID, kind, summary))
		}
	case "extension_ui_request", "permission_request", "approval_request":
		if turn != nil && s.turnHasValidatedUser(turn) {
			s.emitConfiguredActivity(turn, event.Type)
		}
	default:
		if configuredActivityType(event.Type) {
			if turn != nil && s.turnHasValidatedUser(turn) {
				s.emitConfiguredActivity(turn, event.Type)
			}
		} else if authorityType(event.Type) {
			if turn != nil {
				s.failMalformed(turn)
			} else {
				s.rpc.finish(provider.NewProviderError(provider.ErrorMalformedStream))
			}
		}
	}
}

func (s *Session) turnHasValidatedUser(turn *activeTurn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active == turn && turn.userEmitted
}

func parseNativeMessage(raw json.RawMessage) (nativeMessage, bool) {
	var m nativeMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m.Role == "" {
		return m, false
	}
	return m, true
}
func messageText(raw json.RawMessage) (string, bool) {
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		return direct, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		} else if p.Type == "thinking" {
			continue
		} else {
			return "", false
		}
	}
	return b.String(), true
}
func eventTime(value any, fallback time.Time) time.Time {
	switch v := value.(type) {
	case float64:
		return time.UnixMilli(int64(v)).UTC()
	case string:
		if parsed, e := time.Parse(time.RFC3339Nano, v); e == nil {
			return parsed.UTC()
		}
	}
	return fallback.UTC()
}
func configuredActivityType(t string) bool {
	return strings.HasPrefix(t, "tool") || strings.HasPrefix(t, "toolcall") || strings.HasPrefix(t, "extension_ui_") || strings.Contains(t, "permission") || strings.Contains(t, "approval")
}
func authorityType(t string) bool {
	return strings.HasPrefix(t, "tool_") || strings.HasPrefix(t, "toolcall") || strings.HasPrefix(t, "extension_ui_") || strings.HasPrefix(t, "permission") || strings.HasPrefix(t, "approval") || strings.HasPrefix(t, "message_") || strings.HasPrefix(t, "agent_")
}
func normalizedSummary(kind, status string) string {
	switch {
	case strings.Contains(kind, "retry"):
		return "The provider is retrying."
	case strings.Contains(kind, "compaction"):
		return "The provider compacted conversation context."
	case status != "":
		return "The provider status changed."
	default:
		return ""
	}
}
func (s *Session) emitConfiguredActivity(turn *activeTurn, kind string) {
	summary := "The provider used configured capabilities."
	if strings.Contains(kind, "permission") || strings.Contains(kind, "approval") || strings.HasPrefix(kind, "extension_ui_") {
		summary = "The provider requested configured interaction."
	}
	s.emit(provider.NewActivityEvent(turn.request.TurnID, provider.ActivityStatus, summary))
}
func (s *Session) failMalformed(turn *activeTurn) {
	s.mu.Lock()
	if turn.terminalEmitted {
		s.mu.Unlock()
		return
	}
	turn.terminalEmitted = true
	s.mu.Unlock()
	s.emit(provider.NewTerminalFailureEvent(turn.request.TurnID, provider.NewProviderError(provider.ErrorMalformedStream)))
	s.rpc.finish(provider.NewProviderError(provider.ErrorMalformedStream))
}
