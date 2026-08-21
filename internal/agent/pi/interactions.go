//go:build unix

package pi

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

const (
	maxExtensionTimeout      = 24 * time.Hour
	maxExtensionSafetyMargin = 250 * time.Millisecond
)

type realTimerFactory struct{}

func (realTimerFactory) AfterFunc(duration time.Duration, callback func()) OneShotTimer {
	return time.AfterFunc(duration, callback)
}

type interactionOwnership struct{}

type piInteraction struct {
	nativeID string
	method   string
	request  provider.InteractionRequest
	choices  map[string]string
	timer    OneShotTimer
	owner    *interactionOwnership
}

type extensionUIEvent struct {
	ID           string   `json:"id"`
	RequestID    string   `json:"requestId"`
	Method       string   `json:"method"`
	Title        string   `json:"title"`
	Message      string   `json:"message"`
	Options      []string `json:"options"`
	Placeholder  string   `json:"placeholder"`
	DefaultValue string   `json:"defaultValue"`
	Prefill      string   `json:"prefill"`
	Timeout      *int64   `json:"timeout"`
}

func (s *Session) handleExtensionUI(raw json.RawMessage, turn *activeTurn) {
	var header struct {
		ID, RequestID, Method string
	}
	_ = json.Unmarshal(raw, &header)
	trustedID := header.ID
	if trustedID == "" {
		trustedID = header.RequestID
	}
	var native extensionUIEvent
	if json.Unmarshal(raw, &native) != nil || native.Method == "" {
		if blockingExtensionMethod(header.Method) && trustedID != "" && len(trustedID) <= provider.MaxNativeReferenceBytes {
			s.cancelMalformedInteraction(turn, trustedID)
		} else {
			s.failMalformed(turn)
		}
		return
	}
	nativeID := native.ID
	if nativeID == "" {
		nativeID = native.RequestID
	}
	switch native.Method {
	case "setStatus", "setWidget", "setTitle", "set_editor_text":
		return
	case "notify":
		if native.Message == "" || len(native.Message) > provider.MaxSummaryBytes {
			s.failMalformed(turn)
			return
		}
		s.mu.Lock()
		duplicate := s.lastNotification == native.Message
		s.lastNotification = native.Message
		s.mu.Unlock()
		if !duplicate {
			s.emit(provider.NewActivityEvent(turn.request.TurnID, provider.ActivityStatus, native.Message))
		}
		return
	case "select", "confirm", "input", "editor":
	default:
		s.failMalformed(turn)
		return
	}
	if nativeID == "" || len(nativeID) > provider.MaxNativeReferenceBytes {
		s.failMalformed(turn)
		return
	}
	if native.Timeout != nil && *native.Timeout <= 0 {
		s.cancelMalformedInteraction(turn, nativeID)
		return
	}
	title := native.Title
	if title == "" {
		title = "Provider input"
	}
	prompt := native.Message
	if prompt == "" {
		prompt = title
	}
	question := provider.InteractionQuestion{ID: "value", Header: title, Prompt: prompt}
	choices := make(map[string]string)
	switch native.Method {
	case "select":
		if len(native.Options) == 0 || len(native.Options) > provider.MaxInteractionOptions {
			s.cancelMalformedInteraction(turn, nativeID)
			return
		}
		for index, label := range native.Options {
			if label == "" || len(label) > provider.MaxTitleBytes {
				s.cancelMalformedInteraction(turn, nativeID)
				return
			}
			id := "option-" + strconv.Itoa(index+1)
			question.Options = append(question.Options, provider.InteractionOption{ID: id, Label: label})
			choices[id] = label
		}
	case "confirm":
		question.Options = []provider.InteractionOption{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}
		choices["yes"] = "true"
		choices["no"] = "false"
	case "input", "editor":
		question.AllowOther = true
	}
	requestID, err := s.driver.newID()
	if err != nil {
		s.cancelMalformedInteraction(turn, nativeID)
		return
	}
	request := provider.InteractionRequest{ID: requestID, TurnID: turn.request.TurnID, Kind: provider.InteractionUserInput, Title: title, Summary: prompt, Questions: []provider.InteractionQuestion{question}}
	if native.Method == "input" || native.Method == "editor" {
		request.Kind = provider.InteractionMCPElicitation
		request.Questions = nil
		request.Options = []provider.InteractionOption{{ID: "accept", Label: "Submit"}, {ID: "decline", Label: "Decline"}, {ID: "cancel", Label: "Cancel"}}
		request.Fields = []provider.InteractionField{{ID: "value", Label: title, Type: provider.InteractionText, Required: true, Multiline: native.Method == "editor", Description: native.Placeholder}}
	}
	if native.Timeout != nil {
		deadline, ok := conservativeExtensionDeadline(s.driver.config.Clock.Now().UTC(), *native.Timeout)
		if !ok {
			s.cancelMalformedInteraction(turn, nativeID)
			return
		}
		request.LocalDeadline = &deadline
	}
	if request.Validate() != nil {
		s.cancelMalformedInteraction(turn, nativeID)
		return
	}
	s.mu.Lock()
	if s.active != turn || s.interactions == nil || s.shutdownStarted {
		s.mu.Unlock()
		return
	}
	for _, pending := range s.interactions {
		if pending.nativeID == nativeID {
			s.mu.Unlock()
			s.cancelMalformedInteraction(turn, nativeID)
			return
		}
	}
	owner := &interactionOwnership{}
	pending := piInteraction{nativeID: nativeID, method: native.Method, request: request, choices: choices, owner: owner}
	s.interactions[requestID] = pending
	if request.LocalDeadline != nil {
		remaining := request.LocalDeadline.Sub(s.driver.config.Clock.Now().UTC())
		if remaining < 0 {
			remaining = 0
		}
		pending.timer = s.interactionTimerFactory().AfterFunc(remaining, func() { s.expireInteraction(requestID, owner) })
		if common.IsNil(pending.timer) {
			delete(s.interactions, requestID)
			s.mu.Unlock()
			s.failMalformed(turn)
			return
		}
		s.interactions[requestID] = pending
	}
	s.emit(provider.NewInteractionRequestEvent(request))
	s.mu.Unlock()
}

func (s *Session) interactionTimerFactory() TimerFactory {
	if s.driver != nil && !common.IsNil(s.driver.config.Timers) {
		return s.driver.config.Timers
	}
	return realTimerFactory{}
}

func (s *Session) expireInteraction(requestID string, owner *interactionOwnership) {
	s.mu.Lock()
	pending, ok := s.interactions[requestID]
	if !ok || pending.owner != owner {
		s.mu.Unlock()
		return
	}
	delete(s.interactions, requestID)
	s.emit(provider.NewInteractionResolvedEvent(provider.InteractionResolution{RequestID: requestID, Kind: pending.request.Kind}))
	s.mu.Unlock()
}

func blockingExtensionMethod(method string) bool {
	switch method {
	case "select", "confirm", "input", "editor":
		return true
	default:
		return false
	}
}

func (s *Session) cancelMalformedInteraction(turn *activeTurn, nativeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.rpc.notify(ctx, map[string]any{"type": "extension_ui_response", "id": nativeID, "cancelled": true})
	s.failMalformed(turn)
}

func conservativeExtensionDeadline(receivedAt time.Time, timeoutMilliseconds int64) (time.Time, bool) {
	if receivedAt.IsZero() || receivedAt.Location() != time.UTC || timeoutMilliseconds <= 0 || timeoutMilliseconds > int64(maxExtensionTimeout/time.Millisecond) {
		return time.Time{}, false
	}
	duration := time.Duration(timeoutMilliseconds) * time.Millisecond
	margin := duration / 10
	if margin > maxExtensionSafetyMargin {
		margin = maxExtensionSafetyMargin
	}
	if margin < time.Millisecond {
		margin = time.Millisecond
	}
	if margin >= duration {
		margin = duration / 2
	}
	deadline := receivedAt.Add(duration - margin)
	if margin <= 0 || !deadline.After(receivedAt) || !deadline.Before(receivedAt.Add(duration)) {
		return time.Time{}, false
	}
	return deadline, true
}

func (s *Session) Respond(ctx context.Context, response provider.InteractionResponse) error {
	if response.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	pending, ok := s.interactions[response.RequestID]
	if !ok || response.Kind != pending.request.Kind {
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	cancelled := false
	if pending.request.Kind == provider.InteractionMCPElicitation {
		switch response.OptionID {
		case "accept":
			if len(response.Answers) != 1 || len(response.Answers["value"]) != 1 {
				s.mu.Unlock()
				return provider.NewProviderError(provider.ErrorProtocolFailure)
			}
		case "decline", "cancel":
			if len(response.Answers) != 0 {
				s.mu.Unlock()
				return provider.NewProviderError(provider.ErrorProtocolFailure)
			}
			cancelled = true
		default:
			s.mu.Unlock()
			return provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	} else if len(response.Answers) != 1 || len(response.Answers["value"]) != 1 {
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if pending.request.LocalDeadline != nil && !s.driver.config.Clock.Now().UTC().Before(*pending.request.LocalDeadline) {
		delete(s.interactions, response.RequestID)
		stopInteractionTimer(pending.timer)
		s.emit(provider.NewInteractionResolvedEvent(provider.InteractionResolution{RequestID: response.RequestID, Kind: response.Kind}))
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	answer := ""
	if !cancelled {
		answer = response.Answers["value"][0]
	}
	value := any(answer)
	if pending.method == "select" || pending.method == "confirm" {
		mapped, exists := pending.choices[answer]
		if !exists {
			s.mu.Unlock()
			return provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		if pending.method == "confirm" {
			value = mapped == "true"
		} else {
			value = mapped
		}
	}
	delete(s.interactions, response.RequestID)
	stopInteractionTimer(pending.timer)
	s.mu.Unlock()
	wire := map[string]any{"type": "extension_ui_response", "id": pending.nativeID, "value": value}
	if cancelled {
		delete(wire, "value")
		wire["cancelled"] = true
	} else if pending.method == "confirm" {
		delete(wire, "value")
		wire["confirmed"] = value
	}
	writeContext := ctx
	cancelWrite := func() {}
	if pending.request.LocalDeadline != nil {
		remaining := pending.request.LocalDeadline.Sub(s.driver.config.Clock.Now().UTC())
		if remaining <= 0 {
			return provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		writeContext, cancelWrite = context.WithTimeout(ctx, remaining)
	}
	defer cancelWrite()
	if err := s.rpc.notify(writeContext, wire); err != nil {
		return err
	}
	if pending.request.LocalDeadline != nil && !s.driver.config.Clock.Now().UTC().Before(*pending.request.LocalDeadline) {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return nil
}

func (s *Session) CancelInteraction(ctx context.Context, requestID string) error {
	s.mu.Lock()
	pending, ok := s.interactions[requestID]
	if ok {
		delete(s.interactions, requestID)
		stopInteractionTimer(pending.timer)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if err := s.rpc.notify(ctx, map[string]any{"type": "extension_ui_response", "id": pending.nativeID, "cancelled": true}); err != nil {
		return err
	}
	return nil
}

func stopInteractionTimer(timer OneShotTimer) {
	if !common.IsNil(timer) {
		timer.Stop()
	}
}

func (s *Session) claimShutdownInteractionsLocked() []piInteraction {
	claimed := make([]piInteraction, 0, len(s.interactions))
	for requestID, interaction := range s.interactions {
		delete(s.interactions, requestID)
		stopInteractionTimer(interaction.timer)
		claimed = append(claimed, interaction)
		s.emit(provider.NewInteractionResolvedEvent(provider.InteractionResolution{RequestID: requestID, Kind: interaction.request.Kind}))
	}
	return claimed
}

func (s *Session) resolveInteractions() {
	s.mu.Lock()
	pending := s.interactions
	s.interactions = make(map[string]piInteraction)
	s.lastNotification = ""
	for _, interaction := range pending {
		stopInteractionTimer(interaction.timer)
	}
	s.mu.Unlock()
	for id, interaction := range pending {
		s.emit(provider.NewInteractionResolvedEvent(provider.InteractionResolution{RequestID: id, Kind: interaction.request.Kind}))
	}
}

var _ provider.InteractiveSession = (*Session)(nil)
