package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

const (
	maxNativeUpdateBytes       = 1 << 20
	cursorClosedStreamArtifact = "Error: RetriableError: WritableIterable is closed"
	// A replay notification can JSON-escape every byte of a maximum configured
	// v4 envelope. This remains comfortably below ACP's 128 MiB frame ceiling.
	maxReplayUpdateBytes = 72 << 20
	maxReplayUpdates     = 4096
)

type updateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}
type updateHeader struct {
	Kind string `json:"sessionUpdate"`
}
type contentUpdate struct {
	Kind    string `json:"sessionUpdate"`
	Content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}
type toolUpdate struct {
	Kind       string          `json:"sessionUpdate"`
	ToolCallID string          `json:"toolCallId"`
	Title      *string         `json:"title"`
	Summary    string          `json:"summary"`
	Detail     string          `json:"detail"`
	ToolKind   *string         `json:"kind"`
	Status     *string         `json:"status"`
	Content    json.RawMessage `json:"content"`
	Locations  json.RawMessage `json:"locations"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
}
type planUpdate struct {
	Kind    string `json:"sessionUpdate"`
	Entries []struct {
		Content  string `json:"content"`
		Priority string `json:"priority"`
		Status   string `json:"status"`
	} `json:"entries"`
}
type configUpdate struct {
	Kind          string         `json:"sessionUpdate"`
	ConfigOptions []configOption `json:"configOptions"`
}
type permissionParams struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string `json:"toolCallId"`
		Title      string `json:"title"`
		Kind       string `json:"kind"`
	} `json:"toolCall"`
	Options []struct {
		OptionID string `json:"optionId"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

func (s *Session) handle(ctx context.Context, request acp.Request) {
	s.mu.Lock()
	replaying := s.replayPhase == replayOpening
	s.mu.Unlock()
	switch request.Method {
	case "session/update":
		if request.Responder == nil {
			s.update(request.Params)
			return
		}
		_, _ = request.Responder.Respond(ctx, nil, &acp.RPCError{Code: -32600, Message: "invalid request"})
		if replaying {
			s.markBad()
		}
	case "session/request_permission":
		if request.Responder != nil {
			s.permission(ctx, request.Params, request.Responder)
			if replaying {
				s.markBad()
			}
		}
	default:
		if request.Responder != nil {
			_, _ = request.Responder.Respond(ctx, nil, &acp.RPCError{Code: -32601, Message: "method not found"})
		}
	}
}

func (s *Session) update(raw json.RawMessage) {
	s.mu.Lock()
	replaying := s.replayPhase == replayOpening && s.native.Ref.Value() == ""
	nativeID := s.native.Ref.Value()
	turn := s.active
	s.mu.Unlock()
	limit := maxNativeUpdateBytes
	if replaying {
		limit = maxReplayUpdateBytes
	}
	var params updateParams
	if len(raw) > limit || !validUniqueJSON(raw) || json.Unmarshal(raw, &params) != nil || !bounded(params.SessionID, provider.MaxNativeReferenceBytes, true) || len(params.Update) == 0 || len(params.Update) > limit {
		s.poisonActiveUpdate()
		return
	}
	if replaying {
		s.replayRaw(params.SessionID, params.Update)
		return
	}
	if params.SessionID != nativeID {
		if turn != nil {
			s.poisonPrompt(turn)
		}
		return
	}
	var header updateHeader
	if json.Unmarshal(params.Update, &header) != nil || !bounded(header.Kind, 64, true) {
		if turn != nil {
			s.poisonPrompt(turn)
		}
		return
	}
	if turn == nil {
		if header.Kind == "config_option_update" {
			s.handleConfigUpdate(nil, params.Update)
		}
		return
	}
	s.mu.Lock()
	closedStreamSeen := s.active == turn && turn.closedStreamSeen
	s.mu.Unlock()
	if closedStreamSeen {
		return
	}
	switch header.Kind {
	case "agent_message_chunk":
		var update contentUpdate
		if json.Unmarshal(params.Update, &update) != nil || update.Content.Type != "text" || !bounded(update.Content.Text, provider.MaxDeltaBytes, true) {
			s.poisonPrompt(turn)
			return
		}
		if cursorClosedStreamFailure(update.Content.Text) {
			// Cursor ACP 2026.08.11 can expose this adapter-runtime artifact as
			// assistant text after its upstream turn has ended. Give session/prompt
			// one brief chance to settle normally; if it remains stuck, cancel only
			// that call and retain the conversation's process and native session.
			s.observeClosedStreamArtifact(turn)
			return
		}
		s.mu.Lock()
		if s.active != turn || turn.poisoned || len(turn.assistant)+len(update.Content.Text) > provider.MaxEventTextBytes {
			bad := s.active == turn && !turn.poisoned
			s.mu.Unlock()
			if bad {
				s.poisonPrompt(turn)
			}
			return
		}
		turn.assistant += update.Content.Text
		s.mu.Unlock()
		s.queue(turn, provider.NewAssistantDeltaEvent(turn.request.TurnID, turn.assistantID, update.Content.Text))
	case "agent_thought_chunk":
		var update contentUpdate
		if json.Unmarshal(params.Update, &update) != nil || update.Content.Type != "text" || !bounded(update.Content.Text, provider.MaxDeltaBytes, true) {
			s.poisonPrompt(turn)
			return
		}
		// Thought text proves native admission but is deliberately discarded.
		s.accept(turn)
	case "tool_call", "tool_call_update":
		s.handleToolUpdate(turn, params.Update)
	case "plan":
		s.handlePlanUpdate(turn, params.Update)
	case "config_option_update":
		s.handleConfigUpdate(turn, params.Update)
	default:
		// Required notification fields and aggregate bounds were validated above.
		// Unknown standard-compatible updates are ignored and do not prove admission.
	}
}

func cursorClosedStreamFailure(text string) bool {
	return strings.Trim(text, " \t\r\n") == cursorClosedStreamArtifact
}

func (s *Session) poisonActiveUpdate() {
	s.mu.Lock()
	turn := s.active
	if turn == nil && s.replayPhase == replayOpening {
		s.replayPhase = replayFailed
		s.replayBad = true
	}
	s.mu.Unlock()
	if turn != nil {
		s.poisonPrompt(turn)
	}
}

func (s *Session) handleToolUpdate(turn *activePrompt, raw json.RawMessage) {
	var update toolUpdate
	if json.Unmarshal(raw, &update) != nil {
		s.poisonPrompt(turn)
		return
	}
	textBytes := len(update.Summary) + len(update.Detail) + len(update.Content) + len(update.Locations) + len(update.RawInput) + len(update.RawOutput)
	if update.Title != nil {
		textBytes += len(*update.Title)
	}
	if update.ToolKind != nil {
		textBytes += len(*update.ToolKind)
	}
	if update.Status != nil {
		textBytes += len(*update.Status)
	}
	if !bounded(update.ToolCallID, 256, true) || update.Title != nil && !bounded(*update.Title, provider.MaxTitleBytes, true) ||
		!bounded(update.Summary, provider.MaxSummaryBytes, false) || !bounded(update.Detail, provider.MaxInteractionTextBytes, false) || update.ToolKind != nil && !bounded(*update.ToolKind, 64, true) || update.Status != nil && !bounded(*update.Status, 64, false) || textBytes > provider.MaxInteractionTextBytes ||
		!validDiscardedJSON(update.Content) || !validDiscardedJSON(update.Locations) || !validDiscardedJSON(update.RawInput) || !validDiscardedJSON(update.RawOutput) {
		s.poisonPrompt(turn)
		return
	}
	s.mu.Lock()
	if s.active != turn || turn.poisoned || turn.terminal {
		s.mu.Unlock()
		return
	}
	if turn.tools == nil {
		turn.tools = make(map[string]cachedTool)
	}
	if turn.toolBytes > provider.MaxInteractionTextBytes-textBytes {
		s.mu.Unlock()
		s.poisonPrompt(turn)
		return
	}
	turn.toolBytes += textBytes
	state, exists := turn.tools[update.ToolCallID]
	if update.Kind == "tool_call" {
		if exists || update.Title == nil || update.ToolKind == nil || len(turn.tools) >= maxCachedToolsPerTurn {
			s.mu.Unlock()
			s.poisonPrompt(turn)
			return
		}
		state.title = *update.Title
		kind, ok := toolKind(*update.ToolKind)
		if !ok {
			s.mu.Unlock()
			s.poisonPrompt(turn)
			return
		}
		state.kind = kind
		state.status = provider.ToolRunning
	} else if !exists {
		s.mu.Unlock()
		s.poisonPrompt(turn)
		return
	}
	if update.Title != nil {
		state.title = *update.Title
	}
	if update.ToolKind != nil {
		kind, ok := toolKind(*update.ToolKind)
		if !ok || exists && kind != state.kind {
			s.mu.Unlock()
			s.poisonPrompt(turn)
			return
		}
		state.kind = kind
	}
	if update.Status != nil {
		status, ok := toolStatus(*update.Status)
		if !ok || exists && !validToolTransition(state.status, status) {
			s.mu.Unlock()
			s.poisonPrompt(turn)
			return
		}
		state.status = status
	}
	turn.tools[update.ToolCallID] = state
	s.mu.Unlock()
	event := provider.NewToolActivityEvent(provider.ToolActivity{ID: safeID("tool", turn.request.TurnID+"\x00"+update.ToolCallID), TurnID: turn.request.TurnID, Kind: state.kind, Status: state.status, Title: state.title})
	s.queue(turn, event)
}

func validDiscardedJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	items := 0
	var walk func(any, int) bool
	walk = func(current any, depth int) bool {
		if depth > 8 {
			return false
		}
		items++
		if items > 256 {
			return false
		}
		switch typed := current.(type) {
		case string:
			return bounded(typed, provider.MaxInteractionTextBytes, false)
		case []any:
			for _, item := range typed {
				if !walk(item, depth+1) {
					return false
				}
			}
		case map[string]any:
			for key, item := range typed {
				if !bounded(key, 256, true) || !walk(item, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return walk(value, 0)
}

func (s *Session) handlePlanUpdate(turn *activePrompt, raw json.RawMessage) {
	var update planUpdate
	if json.Unmarshal(raw, &update) != nil || len(update.Entries) == 0 || len(update.Entries) > 64 {
		s.poisonPrompt(turn)
		return
	}
	total := 0
	events := make([]provider.Event, 0, len(update.Entries))
	for index, entry := range update.Entries {
		if !bounded(entry.Content, provider.MaxTitleBytes, true) || !bounded(entry.Priority, 64, false) || !bounded(entry.Status, 64, false) {
			s.poisonPrompt(turn)
			return
		}
		total += len(entry.Content) + len(entry.Priority) + len(entry.Status)
		if total > provider.MaxInteractionTextBytes {
			s.poisonPrompt(turn)
			return
		}
		status, ok := toolStatus(entry.Status)
		if !ok {
			s.poisonPrompt(turn)
			return
		}
		events = append(events, provider.NewToolActivityEvent(provider.ToolActivity{ID: safeID("plan", fmt.Sprintf("%s:%d", turn.request.TurnID, index)), TurnID: turn.request.TurnID, Kind: provider.ToolPlan, Status: status, Title: entry.Content}))
	}
	for _, event := range events {
		s.queue(turn, event)
	}
}

func (s *Session) handleConfigUpdate(turn *activePrompt, raw json.RawMessage) {
	var update configUpdate
	if json.Unmarshal(raw, &update) != nil {
		s.poisonPrompt(turn)
		return
	}
	s.mu.Lock()
	if turn != nil && (s.active != turn || turn.poisoned) || s.phase != sessionRunning {
		s.mu.Unlock()
		return
	}
	settings, presentation, _, err := s.updateAuthoritativeLocked(update.ConfigOptions)
	if err != nil {
		s.mu.Unlock()
		s.poisonPrompt(turn)
		return
	}
	s.mu.Unlock()
	turnID := ""
	if turn != nil {
		turnID = turn.request.TurnID
	}
	event := provider.NewVerifiedSettingsEvent(turnID, settings, presentation)
	if turn != nil {
		s.queue(turn, event)
	} else {
		s.emit(event)
	}
}

func toolKind(kind string) (provider.ToolKind, bool) {
	switch kind {
	case "execute":
		return provider.ToolCommand, true
	case "edit", "delete", "move":
		return provider.ToolFileChange, true
	case "fetch":
		return provider.ToolWeb, true
	case "mcp":
		return provider.ToolMCP, true
	case "search", "read", "think", "other":
		return provider.ToolOther, true
	case "plan":
		return provider.ToolPlan, true
	case "collaboration":
		return provider.ToolCollaboration, true
	case "switch_mode":
		return provider.ToolOther, true
	default:
		return "", false
	}
}
func validToolTransition(from, to provider.ToolStatus) bool {
	if from == provider.ToolRunning {
		return true
	}
	return from == to
}

func toolStatus(status string) (provider.ToolStatus, bool) {
	switch status {
	case "", "pending", "in_progress", "running":
		return provider.ToolRunning, true
	case "completed":
		return provider.ToolCompleted, true
	case "failed":
		return provider.ToolFailed, true
	case "interrupted", "cancelled", "canceled":
		return provider.ToolInterrupted, true
	default:
		return "", false
	}
}

func (s *Session) permission(ctx context.Context, raw json.RawMessage, responder *acp.Responder) {
	s.mu.Lock()
	turn := s.active
	s.mu.Unlock()
	var params permissionParams
	if len(raw) > maxNativeUpdateBytes || json.Unmarshal(raw, &params) != nil || !validPermission(params) {
		s.cancelInvalidPermission(ctx, responder, turn)
		return
	}
	s.mu.Lock()
	turn = s.active
	validSession := turn != nil && !turn.poisoned && params.SessionID == s.native.Ref.Value()
	s.mu.Unlock()
	if !validSession {
		s.cancelInvalidPermission(ctx, responder, turn)
		return
	}
	requestID := safeID("permission", turn.request.TurnID+"\x00"+params.ToolCall.ToolCallID)
	pending := &pendingPermission{responder: responder, native: make(map[string]string, len(params.Options)), changed: make(chan struct{})}
	options := make([]provider.InteractionOption, 0, len(params.Options))
	seenNative := make(map[string]struct{}, len(params.Options))
	for index, option := range params.Options {
		if _, duplicate := seenNative[option.OptionID]; duplicate {
			s.cancelInvalidPermission(ctx, responder, turn)
			return
		}
		seenNative[option.OptionID] = struct{}{}
		safe := permissionOption(option.Kind, index)
		if _, duplicate := pending.native[safe]; duplicate {
			s.cancelInvalidPermission(ctx, responder, turn)
			return
		}
		pending.native[safe] = option.OptionID
		options = append(options, provider.InteractionOption{ID: safe, Label: option.Name, Description: permissionDescription(option.Kind)})
	}
	interactionKind := provider.InteractionCommandApproval
	if kind, ok := toolKind(params.ToolCall.Kind); ok && kind == provider.ToolFileChange {
		interactionKind = provider.InteractionFileApproval
	}
	interaction := provider.InteractionRequest{ID: requestID, TurnID: turn.request.TurnID, Kind: interactionKind, Title: "Permission requested", Summary: params.ToolCall.Title, Options: options}
	if deadline, ok := ctx.Deadline(); ok {
		deadline = deadline.UTC()
		interaction.LocalDeadline = &deadline
	}
	if interaction.Validate() != nil {
		s.cancelInvalidPermission(ctx, responder, turn)
		return
	}
	pending.request = interaction
	s.mu.Lock()
	if s.active != turn || turn.poisoned || s.phase != sessionRunning || !turn.permissionGateOpen {
		s.mu.Unlock()
		_, _ = responder.Respond(ctx, cancelledOutcome(), nil)
		return
	}
	_, resolved := s.permissionOutcomes[requestID]
	if _, duplicate := s.permissions[requestID]; duplicate || resolved || len(s.permissions)+len(s.permissionOutcomes) >= maxPermissionsPerTurn {
		s.mu.Unlock()
		s.cancelInvalidPermission(ctx, responder, turn)
		return
	}
	s.permissions[requestID] = pending
	s.permissionOrder = append(s.permissionOrder, requestID)
	s.acceptLocked(turn)
	if s.active == turn && !turn.poisoned {
		pending.published = s.publishLocked(provider.NewInteractionRequestEvent(interaction))
	}
	s.mu.Unlock()
	go s.watchPermission(requestID, pending)
}

func (s *Session) cancelInvalidPermission(ctx context.Context, responder *acp.Responder, turn *activePrompt) {
	delivery, _ := responder.Respond(ctx, cancelledOutcome(), nil)
	if turn != nil {
		s.poisonPromptWithInbound(turn, delivery == acp.Complete)
	}
}

func (s *Session) watchPermission(id string, pending *pendingPermission) {
	<-pending.responder.Done()
	outcome := pending.responder.Outcome()
	s.mu.Lock()
	failed := s.recordPermissionOutcomeLocked(id, pending, outcome.Delivery)
	s.mu.Unlock()
	if failed {
		select {
		case <-s.rt.client.Done():
			return
		default:
			s.failTransportAsync()
		}
	}
}

func (s *Session) recordPermissionOutcomeLocked(id string, pending *pendingPermission, delivery acp.Delivery) bool {
	if s.permissions[id] != pending {
		return false
	}
	delete(s.permissions, id)
	s.permissionOutcomes[id] = delivery
	pending.mu.Lock()
	pending.state = permissionSettled
	optionID := pending.optionID
	browserClaimed := pending.browserClaimed
	pending.signalLocked()
	pending.mu.Unlock()
	// The broker resolves browser-owned claims for every delivery outcome; this
	// event is only for native resolution or expiry without a browser response.
	if pending.published && !browserClaimed {
		s.publishLocked(provider.NewInteractionResolvedEvent(provider.InteractionResolution{RequestID: id, Kind: pending.request.Kind, OptionID: optionID}))
	}
	return delivery != acp.Complete
}

func validPermission(params permissionParams) bool {
	if !bounded(params.SessionID, provider.MaxNativeReferenceBytes, true) || !bounded(params.ToolCall.ToolCallID, 256, true) ||
		!bounded(params.ToolCall.Title, provider.MaxSummaryBytes, true) || !bounded(params.ToolCall.Kind, 64, true) ||
		len(params.Options) == 0 || len(params.Options) > provider.MaxInteractionOptions {
		return false
	}
	total := len(params.SessionID) + len(params.ToolCall.ToolCallID) + len(params.ToolCall.Title) + len(params.ToolCall.Kind)
	for _, option := range params.Options {
		if !bounded(option.OptionID, 256, true) || !bounded(option.Name, provider.MaxTitleBytes, true) || !validPermissionKind(option.Kind) {
			return false
		}
		total += len(option.OptionID) + len(option.Name) + len(option.Kind)
		if total > provider.MaxInteractionTextBytes {
			return false
		}
	}
	_, kindOK := toolKind(params.ToolCall.Kind)
	return kindOK
}
func validPermissionKind(kind string) bool {
	switch kind {
	case "allow_once", "allow_always", "reject_once", "reject_always":
		return true
	default:
		return false
	}
}
func permissionOption(kind string, index int) string {
	switch kind {
	case "allow_once":
		return "allowOnce"
	case "allow_always":
		return "allowSession"
	case "reject_once":
		return "rejectOnce"
	case "reject_always":
		return "rejectSession"
	default:
		return fmt.Sprintf("option%d", index+1)
	}
}
func permissionDescription(kind string) string {
	switch kind {
	case "allow_once":
		return "Allow for this request"
	case "allow_always":
		return "Allow for this session"
	case "reject_once":
		return "Reject this request"
	case "reject_always":
		return "Reject for this session"
	default:
		return ""
	}
}
func selectedOutcome(native string) any {
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": native}}
}
func cancelledOutcome() any {
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

type permissionClaim struct {
	id      string
	pending *pendingPermission
}

func claimPermissionLocked(pending *pendingPermission, optionID string, browser bool) bool {
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if pending.state != permissionOpen {
		return false
	}
	pending.state = permissionWriting
	pending.optionID = optionID
	pending.browserClaimed = pending.browserClaimed || browser
	pending.signalLocked()
	return true
}

func (s *Session) writePermissionClaim(ctx context.Context, claim permissionClaim, value any) error {
	delivery, err := claim.pending.responder.Respond(ctx, value, nil)
	s.mu.Lock()
	if delivery == acp.NotWritten && s.active != nil && s.active.permissionGateOpen {
		claim.pending.mu.Lock()
		claim.pending.state = permissionOpen
		claim.pending.signalLocked()
		claim.pending.mu.Unlock()
	} else {
		s.recordPermissionOutcomeLocked(claim.id, claim.pending, delivery)
	}
	s.mu.Unlock()
	if err != nil || delivery != acp.Complete {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return nil
}

func (s *Session) Respond(ctx context.Context, response provider.InteractionResponse) error {
	if response.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	pending := s.permissions[response.RequestID]
	if pending == nil || s.active == nil || !s.active.permissionGateOpen || response.Kind != pending.request.Kind {
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	native, ok := pending.native[response.OptionID]
	if !ok || !claimPermissionLocked(pending, response.OptionID, true) {
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Unlock()
	err := s.writePermissionClaim(ctx, permissionClaim{response.RequestID, pending}, selectedOutcome(native))
	if err != nil && pending.responder.Outcome().Delivery == acp.Indeterminate {
		s.failTransportAsync()
	}
	return err
}

func (s *Session) CancelInteraction(ctx context.Context, id string) error {
	if !bounded(id, 128, true) {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	pending := s.permissions[id]
	if pending == nil {
		outcome, known := s.permissionOutcomes[id]
		s.mu.Unlock()
		if known && outcome != acp.Complete {
			return provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		return nil
	}
	if s.active == nil || !s.active.permissionGateOpen || !claimPermissionLocked(pending, "", false) {
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Unlock()
	return s.writePermissionClaim(ctx, permissionClaim{id, pending}, cancelledOutcome())
}

func (s *Session) settlePermissions(ctx context.Context, turn *activePrompt) error {
	var failures []error
	for {
		s.mu.Lock()
		turn.permissionGateOpen = false
		claims := make([]permissionClaim, 0, len(s.permissions))
		var wait <-chan struct{}
		for _, id := range s.permissionOrder {
			pending := s.permissions[id]
			if pending == nil {
				continue
			}
			if claimPermissionLocked(pending, "", false) {
				claims = append(claims, permissionClaim{id, pending})
				continue
			}
			pending.mu.Lock()
			if pending.state == permissionWriting && wait == nil {
				wait = pending.changed
			}
			pending.mu.Unlock()
		}
		live := len(s.permissions)
		for _, delivery := range s.permissionOutcomes {
			if delivery != acp.Complete {
				failures = append(failures, provider.NewProviderError(provider.ErrorProtocolFailure))
				break
			}
		}
		s.mu.Unlock()
		for _, claim := range claims {
			if err := s.writePermissionClaim(ctx, claim, cancelledOutcome()); err != nil {
				failures = append(failures, err)
			}
		}
		if len(claims) > 0 {
			continue
		}
		if live == 0 {
			return errors.Join(failures...)
		}
		if wait == nil {
			return errors.Join(append(failures, provider.NewProviderError(provider.ErrorProtocolFailure))...)
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return errors.Join(append(failures, provider.NewProviderError(provider.ErrorProtocolFailure))...)
		}
	}
}

func (s *Session) cancelPermissions(ctx context.Context) error {
	s.mu.Lock()
	turn := s.active
	s.mu.Unlock()
	if turn == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, delivery := range s.permissionOutcomes {
			if delivery != acp.Complete {
				return provider.NewProviderError(provider.ErrorProtocolFailure)
			}
		}
		return nil
	}
	return s.settlePermissions(ctx, turn)
}

func (s *Session) replayRaw(sessionID string, raw json.RawMessage) {
	if !validUniqueJSON(raw) {
		s.markBad()
		return
	}
	var header updateHeader
	if json.Unmarshal(raw, &header) != nil || !bounded(header.Kind, 64, true) {
		s.markBad()
		return
	}
	s.mu.Lock()
	if s.replayPhase != replayOpening {
		s.mu.Unlock()
		return
	}
	s.replayUpdates++
	if s.replayUpdates > maxReplayUpdates || s.replaySession != "" && s.replaySession != sessionID {
		s.replayPhase = replayFailed
	}
	if s.replaySession == "" {
		s.replaySession = sessionID
	}
	bad := s.replayPhase == replayFailed
	s.mu.Unlock()
	if bad {
		return
	}
	switch header.Kind {
	case "user_message_chunk":
		s.replayUser(raw)
	case "agent_message_chunk":
		s.replayAssistant(raw)
	case "agent_thought_chunk":
		s.replayThought(raw)
	case "tool_call", "tool_call_update":
		s.replayTool(raw)
	case "plan":
		s.replayPlan(raw)
	case "config_option_update":
		s.replayConfig(raw)
	default:
		// A structurally valid, bounded, standard-compatible future update does
		// not prove acceptance and is deliberately not retained.
	}
}

func (s *Session) replayConfig(raw json.RawMessage) {
	var update configUpdate
	if json.Unmarshal(raw, &update) != nil || update.Kind != "config_option_update" {
		s.markBad()
		return
	}
	if _, _, _, err := catalogFromOptions(update.ConfigOptions, s.rt.caps.Image); err != nil {
		s.markBad()
	}
}

func (s *Session) replayUser(raw json.RawMessage) {
	var update contentUpdate
	if json.Unmarshal(raw, &update) != nil || update.Content.Type != "text" || !bounded(update.Content.Text, maxReplayUpdateBytes, true) {
		s.markBad()
		return
	}
	encoded := []byte(update.Content.Text)
	if !bytes.HasPrefix(encoded, []byte(provider.Header)) {
		s.markBad()
		return
	}
	envelope, err := provider.Parse(encoded)
	if err != nil || envelope.Policy != provider.PolicyConfigured {
		if err == nil {
			wipeReplayEnvelope(&envelope)
		}
		s.markBad()
		return
	}
	canonical, err := json.Marshal(envelope.ReaderContent)
	framed, frameErr := replayReaderField(encoded)
	if err != nil || frameErr != nil || !bytes.Equal(framed, canonical) {
		wipeReplayEnvelope(&envelope)
		s.markBad()
		return
	}
	content := envelope.ReaderContent.Clone()
	turnID, messageID := envelope.TurnID, envelope.MessageID
	wipeReplayEnvelope(&envelope)
	semantic := content.SemanticBytes()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayPhase != replayOpening {
		return
	}
	_, turnSeen := s.replayIDs[turnID]
	_, messageSeen := s.replayIDs[messageID]
	if turnSeen || messageSeen || turnID == messageID || len(s.history) >= provider.MaxHistoryItems || s.historyBytes > provider.MaxHistoryBytes-semantic {
		s.replayPhase = replayFailed
		return
	}
	s.replayIDs[turnID] = struct{}{}
	s.replayIDs[messageID] = struct{}{}
	s.history = append(s.history, provider.HistoryItem{TurnID: turnID, MessageID: messageID, Role: provider.HistoryUser, Content: content, CreatedAt: s.replayAnchor.Add(timeNanos(len(s.history)))})
	s.historyBytes += semantic
	s.replayTurn = &replayTurn{turnID: turnID, assistant: -1, tools: map[string]cachedTool{}}
}

func (s *Session) replayAssistant(raw json.RawMessage) {
	var update contentUpdate
	if json.Unmarshal(raw, &update) != nil || update.Content.Type != "text" || !bounded(update.Content.Text, provider.MaxDeltaBytes, true) {
		s.markBad()
		return
	}
	if cursorClosedStreamFailure(update.Content.Text) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turn := s.replayTurn
	if s.replayPhase != replayOpening || turn == nil || s.historyBytes > provider.MaxHistoryBytes-len(update.Content.Text) {
		s.replayPhase = replayFailed
		return
	}
	if turn.assistant < 0 {
		if len(s.history) >= provider.MaxHistoryItems {
			s.replayPhase = replayFailed
			return
		}
		id := safeID("history-assistant", turn.turnID)
		if _, exists := s.replayIDs[id]; exists {
			s.replayPhase = replayFailed
			return
		}
		s.replayIDs[id] = struct{}{}
		turn.assistant = len(s.history)
		s.history = append(s.history, provider.HistoryItem{TurnID: turn.turnID, MessageID: id, Role: provider.HistoryAssistant, Text: update.Content.Text, CreatedAt: s.replayAnchor.Add(timeNanos(len(s.history)))})
	} else {
		item := &s.history[turn.assistant]
		if len(item.Text) > provider.MaxHistoryItemBytes-len(update.Content.Text) {
			s.replayPhase = replayFailed
			return
		}
		item.Text += update.Content.Text
	}
	s.historyBytes += len(update.Content.Text)
}

func (s *Session) replayThought(raw json.RawMessage) {
	var update contentUpdate
	if json.Unmarshal(raw, &update) != nil || update.Content.Type != "text" || !bounded(update.Content.Text, provider.MaxDeltaBytes, true) {
		s.markBad()
		return
	}
	s.mu.Lock()
	if s.replayTurn == nil {
		s.replayPhase = replayFailed
	}
	s.mu.Unlock()
}

func (s *Session) replayTool(raw json.RawMessage) {
	var update toolUpdate
	if json.Unmarshal(raw, &update) != nil {
		s.markBad()
		return
	}
	textBytes := len(update.Summary) + len(update.Detail) + len(update.Content) + len(update.Locations) + len(update.RawInput) + len(update.RawOutput)
	if update.Title != nil {
		textBytes += len(*update.Title)
	}
	if update.ToolKind != nil {
		textBytes += len(*update.ToolKind)
	}
	if update.Status != nil {
		textBytes += len(*update.Status)
	}
	valid := bounded(update.ToolCallID, 256, true) && (update.Title == nil || bounded(*update.Title, provider.MaxTitleBytes, true)) && bounded(update.Summary, provider.MaxSummaryBytes, false) && bounded(update.Detail, provider.MaxInteractionTextBytes, false) && (update.ToolKind == nil || bounded(*update.ToolKind, 64, true)) && (update.Status == nil || bounded(*update.Status, 64, false)) && textBytes <= provider.MaxInteractionTextBytes && validDiscardedJSON(update.Content) && validDiscardedJSON(update.Locations) && validDiscardedJSON(update.RawInput) && validDiscardedJSON(update.RawOutput)
	if !valid {
		s.markBad()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turn := s.replayTurn
	if turn == nil || turn.toolBytes > provider.MaxInteractionTextBytes-textBytes {
		s.replayPhase = replayFailed
		return
	}
	state, exists := turn.tools[update.ToolCallID]
	if update.Kind == "tool_call" {
		if exists || update.Title == nil || update.ToolKind == nil || len(turn.tools) >= maxCachedToolsPerTurn {
			s.replayPhase = replayFailed
			return
		}
		kind, ok := toolKind(*update.ToolKind)
		if !ok {
			s.replayPhase = replayFailed
			return
		}
		state = cachedTool{title: *update.Title, kind: kind, status: provider.ToolRunning}
	} else if !exists {
		s.replayPhase = replayFailed
		return
	}
	if update.Title != nil {
		state.title = *update.Title
	}
	if update.ToolKind != nil {
		kind, ok := toolKind(*update.ToolKind)
		if !ok || exists && kind != state.kind {
			s.replayPhase = replayFailed
			return
		}
		state.kind = kind
	}
	if update.Status != nil {
		status, ok := toolStatus(*update.Status)
		if !ok || exists && !validToolTransition(state.status, status) {
			s.replayPhase = replayFailed
			return
		}
		state.status = status
	}
	turn.toolBytes += textBytes
	turn.tools[update.ToolCallID] = state
}

func (s *Session) replayPlan(raw json.RawMessage) {
	var update planUpdate
	if json.Unmarshal(raw, &update) != nil || len(update.Entries) == 0 || len(update.Entries) > 64 {
		s.markBad()
		return
	}
	total := 0
	for _, entry := range update.Entries {
		if !bounded(entry.Content, provider.MaxTitleBytes, true) || !bounded(entry.Priority, 64, false) || !bounded(entry.Status, 64, false) {
			s.markBad()
			return
		}
		total += len(entry.Content) + len(entry.Priority) + len(entry.Status)
		if total > provider.MaxInteractionTextBytes {
			s.markBad()
			return
		}
		if _, ok := toolStatus(entry.Status); !ok {
			s.markBad()
			return
		}
	}
	s.mu.Lock()
	if s.replayTurn == nil {
		s.replayPhase = replayFailed
	}
	s.mu.Unlock()
}

func replayReaderField(encoded []byte) ([]byte, error) {
	cursor := len(provider.Header)
	var value []byte
	for _, label := range provider.Labels() {
		prefix := []byte(label + " ")
		if cursor+len(prefix) > len(encoded) || !bytes.Equal(encoded[cursor:cursor+len(prefix)], prefix) {
			return nil, errors.New("frame")
		}
		cursor += len(prefix)
		end := bytes.IndexByte(encoded[cursor:], '\n')
		if end < 0 {
			return nil, errors.New("frame")
		}
		n, err := strconv.Atoi(string(encoded[cursor : cursor+end]))
		if err != nil || n < 0 {
			return nil, errors.New("frame")
		}
		cursor += end + 1
		if n > len(encoded)-cursor || cursor+n >= len(encoded) || encoded[cursor+n] != '\n' {
			return nil, errors.New("frame")
		}
		value = encoded[cursor : cursor+n]
		cursor += n + 1
	}
	if cursor != len(encoded)-len(provider.Footer) || !bytes.Equal(encoded[cursor:], []byte(provider.Footer)) {
		return nil, errors.New("frame")
	}
	return value, nil
}

func wipeReplayEnvelope(envelope *provider.Envelope) {
	for i := range envelope.Source {
		envelope.Source[i] = 0
	}
	for i := range envelope.CreatorContext {
		envelope.CreatorContext[i] = 0
	}
	envelope.Source = nil
	envelope.CreatorContext = nil
}

func validUniqueJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	items := 0
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 16 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		items++
		if items > 8192 {
			return false
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return false
				}
				key, ok := keyToken.(string)
				if !ok {
					return false
				}
				if _, duplicate := seen[key]; duplicate {
					return false
				}
				seen[key] = struct{}{}
				if !walk(depth + 1) {
					return false
				}
			}
		case '[':
			for decoder.More() {
				if !walk(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		_, err = decoder.Token()
		return err == nil
	}
	if !walk(0) {
		return false
	}
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}

func timeNanos(n int) time.Duration { return time.Duration(n) * time.Nanosecond }
func (s *Session) markBad() {
	s.mu.Lock()
	s.replayBad = true
	s.replayPhase = replayFailed
	s.mu.Unlock()
}
