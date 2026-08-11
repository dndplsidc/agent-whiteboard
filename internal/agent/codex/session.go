package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type nativeTurn struct {
	request  provider.TurnRequest
	nativeID string
	buffered []bufferedNativeEvent
	bytes    int
}

type bufferedNativeEvent struct {
	rpcID  json.RawMessage
	method string
	params json.RawMessage
}

type nativeInteraction struct {
	rpcID                  json.RawMessage
	method                 string
	params                 json.RawMessage
	request                provider.InteractionRequest
	responseKey            map[string]string
	choices                map[string]map[string]string
	fieldTypes             map[string]provider.InteractionFieldType
	permissionChoices      map[string]nativePermissionChoice
	permissionGlobMaxDepth *uint64
}

type nativePermissionChoice struct {
	kind  string
	value json.RawMessage
}

type Session struct {
	driver       *Driver
	runtime      *runtime
	native       provider.NativeSession
	threadID     string
	events       chan provider.Event
	view         *sessionChild
	workspace    string
	capabilities provider.Capabilities

	mu           sync.Mutex
	active       *nativeTurn
	activities   map[string]string
	toolStates   map[string]provider.ToolActivity
	interactions map[string]nativeInteraction
	closed       bool
	closeOnce    sync.Once
	eventMu      sync.Mutex
	eventsClosed bool
}

func newSession(driver *Driver, runtime *runtime, native provider.NativeSession, workspace string, capabilities provider.Capabilities) *Session {
	view := newSessionChild()
	return &Session{
		driver: driver, runtime: runtime, native: native, threadID: native.Ref.Value(), events: make(chan provider.Event, 512), view: view, workspace: workspace, capabilities: capabilities,
		activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
	}
}

func (session *Session) NativeSession() provider.NativeSession { return session.native }
func (session *Session) Model() string                         { return session.native.Model }
func (session *Session) Capabilities() provider.Capabilities   { return session.capabilities }
func (session *Session) Events() <-chan provider.Event         { return session.events }
func (session *Session) Child() provider.ManagedChild          { return session.view }

func (session *Session) Preflight(_ context.Context, request provider.PreflightRequest) (provider.PreflightResult, error) {
	if request.Validate() != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if len(request.Turn.Images) != 0 && !session.capabilities.Images {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorImageInputUnsupported)
	}
	// App Server owns compaction and capacity enforcement. These values satisfy
	// the legacy neutral contract without estimating a Codex context window.
	return provider.PreflightResult{ResolvedModel: session.native.Model, EstimatedInputTokens: 1, EffectiveCapacityTokens: int(^uint(0) >> 1), SafetyMarginTokens: 0}, nil
}

func (session *Session) Submit(ctx context.Context, request provider.TurnRequest) (provider.AcceptedTurn, error) {
	if request.Validate() != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if len(request.Images) != 0 && !session.capabilities.Images {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorImageInputUnsupported)
	}
	envelope, err := provider.Build(request, provider.PolicyConfigured)
	if err != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	defer wipe(envelope)
	turn := &nativeTurn{request: request}
	session.mu.Lock()
	if session.closed || session.active != nil {
		session.mu.Unlock()
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	session.active = turn
	session.mu.Unlock()
	input, err := buildTurnInput(session.workspace, envelope, request.Images)
	if err != nil {
		session.mu.Lock()
		if session.active == turn {
			session.active = nil
		}
		session.mu.Unlock()
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorImageStorageFailure)
	}
	result, releaseStream, err := session.runtime.callOrdered(ctx, "turn/start", map[string]any{
		"threadId": session.threadID,
		"input":    input,
	})
	if err != nil {
		session.mu.Lock()
		if session.active == turn {
			session.active = nil
		}
		session.mu.Unlock()
		return provider.AcceptedTurn{}, err
	}
	defer releaseStream()
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(result, &response) != nil || response.Turn.ID == "" {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	acceptedAt := session.driver.config.Clock.Now().UTC()
	if acceptedAt.IsZero() {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	session.mu.Lock()
	if session.active != turn {
		session.mu.Unlock()
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	turn.nativeID = response.Turn.ID
	buffered := append([]bufferedNativeEvent(nil), turn.buffered...)
	turn.buffered = nil
	turn.bytes = 0
	session.mu.Unlock()
	session.emit(provider.NewUserMessageEvent(request.TurnID, request.MessageID, request.Content, acceptedAt))
	for _, event := range buffered {
		if len(event.rpcID) != 0 {
			if err := session.handleServerRequest(event.rpcID, event.method, event.params); err != nil {
				_ = session.runtime.respond(context.Background(), event.rpcID, nil, &rpcError{Code: -32601, Message: "unsupported request"})
				session.fail(provider.ErrorProtocolIncompatible)
			}
			continue
		}
		session.handleNotification(event.method, event.params)
	}
	return provider.AcceptedTurn{TurnID: request.TurnID, AcceptedAt: acceptedAt}, nil
}

func (session *Session) Interrupt(ctx context.Context, accepted provider.AcceptedTurn) error {
	if accepted.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	session.mu.Lock()
	turn := session.active
	if turn == nil || turn.request.TurnID != accepted.TurnID || turn.nativeID == "" {
		session.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	nativeID := turn.nativeID
	session.mu.Unlock()
	_, err := session.runtime.call(ctx, "turn/interrupt", map[string]any{"threadId": session.threadID, "turnId": nativeID})
	return err
}

func (session *Session) History(ctx context.Context, request provider.HistoryRequest) (provider.HistoryPage, error) {
	if request.Validate() != nil {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	result, err := session.runtime.call(ctx, "thread/read", map[string]any{"threadId": session.threadID, "includeTurns": true})
	if err != nil {
		return provider.HistoryPage{}, err
	}
	items, err := projectHistory(result, session.threadID)
	if err != nil {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	return historyPage(items, request)
}

func historyPage(items []provider.HistoryItem, request provider.HistoryRequest) (provider.HistoryPage, error) {
	end := len(items)
	if request.BeforeMessageID != "" {
		found := false
		for index, item := range items {
			if item.MessageID == request.BeforeMessageID {
				end = index
				found = true
				break
			}
		}
		if !found {
			return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	limit := request.Limit
	if limit == 0 {
		limit = provider.MaxHistoryItems
	}
	start := max(0, end-limit)
	page := provider.HistoryPage{Items: make([]provider.HistoryItem, 0, end-start)}
	for index := end - 1; index >= start; index-- {
		page.Items = append(page.Items, items[index])
	}
	if start > 0 && len(page.Items) != 0 {
		page.NextCursor = page.Items[len(page.Items)-1].MessageID
	}
	if page.Validate() != nil {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	return page, nil
}

func (session *Session) Reconcile(ctx context.Context, reference provider.TurnReference) (provider.TurnState, error) {
	if reference.Validate() != nil {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	result, err := session.runtime.call(ctx, "thread/read", map[string]any{"threadId": session.threadID, "includeTurns": true})
	if err != nil {
		return provider.TurnUnknown, err
	}
	state, err := reconcileHistory(result, session.threadID, reference.TurnID)
	if err != nil {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	return state, nil
}

func (session *Session) Respond(ctx context.Context, response provider.InteractionResponse) error {
	if response.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	session.mu.Lock()
	pending, ok := session.interactions[response.RequestID]
	if !ok || !validInteractionResponse(pending.request, response) {
		session.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	result, err := interactionRPCResult(pending, response)
	if err != nil {
		session.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	encoded, err := encodeRPCResponse(pending.rpcID, result, nil)
	if err != nil {
		session.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if !session.runtime.claimInbound(pending.rpcID) {
		session.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	delete(session.interactions, response.RequestID)
	session.mu.Unlock()
	if err := session.runtime.writeEncoded(ctx, encoded); err != nil {
		return err
	}
	return nil
}

func (session *Session) CancelInteraction(ctx context.Context, requestID string) error {
	session.mu.Lock()
	pending, ok := session.interactions[requestID]
	if ok && session.runtime.claimInbound(pending.rpcID) {
		delete(session.interactions, requestID)
	} else {
		ok = false
	}
	session.mu.Unlock()
	if !ok {
		return nil
	}
	result := cancellationRPCResult(pending.method)
	if err := session.runtime.respondClaimed(ctx, pending.rpcID, result, nil); err != nil {
		return err
	}
	return nil
}

func (session *Session) Shutdown(ctx context.Context) error {
	stopRuntime := false
	session.closeOnce.Do(func() {
		session.mu.Lock()
		session.closed = true
		active := session.active != nil
		nativeID := ""
		if active {
			nativeID = session.active.nativeID
		}
		pending := make([]nativeInteraction, 0, len(session.interactions))
		for requestID, interaction := range session.interactions {
			if session.runtime.claimInbound(interaction.rpcID) {
				pending = append(pending, interaction)
				delete(session.interactions, requestID)
			}
		}
		session.mu.Unlock()
		if active {
			confirmed := false
			if nativeID != "" {
				_, interruptErr := session.runtime.call(ctx, "turn/interrupt", map[string]any{"threadId": session.threadID, "turnId": nativeID})
				confirmed = interruptErr == nil
			}
			if !confirmed {
				stopRuntime = true
			}
		}
		for _, interaction := range pending {
			_ = session.runtime.respondClaimed(ctx, interaction.rpcID, cancellationRPCResult(interaction.method), nil)
		}
		session.driver.detach(session)
		session.closeEvents()
		session.view.close()
	})
	if stopRuntime {
		session.runtime.close()
	}
	return nil
}

func validInteractionResponse(request provider.InteractionRequest, response provider.InteractionResponse) bool {
	if request.ID != response.RequestID || request.Kind != response.Kind {
		return false
	}
	if response.OptionID != "" {
		found := false
		for _, option := range request.Options {
			if option.ID == response.OptionID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(request.Options) != 0 && response.OptionID == "" {
		return false
	}
	if request.Kind != provider.InteractionMCPElicitation && request.Kind != provider.InteractionPermissionApproval && response.OptionID != "" && len(response.Answers) != 0 {
		return false
	}
	if request.Kind == provider.InteractionMCPElicitation && response.OptionID != "accept" && len(response.Answers) != 0 {
		return false
	}
	type answerSurface struct {
		required   bool
		allowOther bool
		options    []provider.InteractionOption
	}
	allowed := make(map[string]answerSurface, len(request.Questions)+len(request.Fields))
	for _, question := range request.Questions {
		allowed[question.ID] = answerSurface{allowOther: question.AllowOther, options: question.Options}
	}
	for _, field := range request.Fields {
		allowed[field.ID] = answerSurface{required: field.Required, allowOther: field.Type == provider.InteractionText || field.Type == provider.InteractionNumber || field.Type == provider.InteractionBoolean, options: field.Options}
	}
	for key, values := range response.Answers {
		surface, exists := allowed[key]
		if !exists {
			return false
		}
		for _, value := range values {
			found := false
			for _, option := range surface.options {
				if option.ID == value {
					found = true
					break
				}
			}
			if !found && !surface.allowOther {
				return false
			}
		}
	}
	if request.Kind == provider.InteractionMCPElicitation && response.OptionID == "accept" {
		for key, surface := range allowed {
			if surface.required && len(response.Answers[key]) == 0 {
				return false
			}
		}
	}
	return true
}

func (session *Session) runtimeStopped(cause error) {
	session.closeOnce.Do(func() {
		session.mu.Lock()
		turnID := ""
		if session.active != nil {
			turnID = session.active.request.TurnID
		}
		session.closed = true
		session.mu.Unlock()
		if turnID != "" {
			session.emit(provider.NewTerminalFailureEvent(turnID, providerRuntimeCause(cause)))
		} else {
			session.emit(provider.NewTerminalFailureEvent("", providerRuntimeCause(cause)))
		}
		session.closeEvents()
		session.view.close()
	})
}

func (session *Session) fail(code provider.ProviderErrorCode) {
	session.mu.Lock()
	turnID := ""
	if session.active != nil {
		turnID = session.active.request.TurnID
	}
	session.mu.Unlock()
	session.emit(provider.NewTerminalFailureEvent(turnID, provider.NewProviderError(code)))
}

func (session *Session) emit(event provider.Event) {
	if event.Validate() != nil {
		return
	}
	session.eventMu.Lock()
	defer session.eventMu.Unlock()
	if session.eventsClosed {
		return
	}
	select {
	case session.events <- event:
		return
	default:
	}
	if !importantEvent(event.Kind) {
		return
	}
	select {
	case <-session.events:
	default:
	}
	select {
	case session.events <- event:
	default:
	}
}

func (session *Session) closeEvents() {
	session.eventMu.Lock()
	defer session.eventMu.Unlock()
	if !session.eventsClosed {
		session.eventsClosed = true
		close(session.events)
	}
}

func importantEvent(kind provider.EventKind) bool {
	switch kind {
	case provider.EventInteractionRequest, provider.EventInteractionResolved, provider.EventCompletion, provider.EventInterruption, provider.EventTerminalFailure:
		return true
	default:
		return false
	}
}

type sessionChild struct {
	done chan struct{}
	once sync.Once
}

func newSessionChild() *sessionChild         { return &sessionChild{done: make(chan struct{})} }
func (child *sessionChild) close()           { child.once.Do(func() { close(child.done) }) }
func (*sessionChild) Input() io.WriteCloser  { return discardWriteCloser{} }
func (*sessionChild) Output() io.Reader      { return bytes.NewReader(nil) }
func (*sessionChild) Errors() io.Reader      { return bytes.NewReader(nil) }
func (child *sessionChild) Wait() error      { <-child.done; return nil }
func (child *sessionChild) Terminate() error { child.close(); return nil }
func (child *sessionChild) Kill() error      { child.close(); return nil }

type discardWriteCloser struct{}

func (discardWriteCloser) Write(value []byte) (int, error) { return len(value), nil }
func (discardWriteCloser) Close() error                    { return nil }

func projectHistory(raw json.RawMessage, expectedThreadID string) ([]provider.HistoryItem, error) {
	turns, err := decodeTurns(raw, expectedThreadID)
	if err != nil {
		return nil, err
	}
	items := make([]provider.HistoryItem, 0, len(turns)*2)
	for _, turn := range turns {
		at := turnTime(turn)
		for _, item := range turn.Items {
			switch item.Type {
			case "userMessage":
				text := firstTextInput(item.Content)
				envelope, parseErr := provider.Parse([]byte(text))
				if parseErr != nil || envelope.Policy != provider.PolicyConfigured {
					continue
				}
				items = append(items, provider.HistoryItem{TurnID: envelope.TurnID, MessageID: envelope.MessageID, Role: provider.HistoryUser, Content: envelope.ReaderContent.Clone(), CreatedAt: at})
			case "agentMessage":
				if len(items) == 0 || items[len(items)-1].Role != provider.HistoryUser || item.Text == "" {
					continue
				}
				user := items[len(items)-1]
				items = append(items, provider.HistoryItem{TurnID: user.TurnID, MessageID: provider.AssistantMessageID(user.TurnID), Role: provider.HistoryAssistant, Text: bounded(cleanText(item.Text), provider.MaxHistoryItemBytes), CreatedAt: at})
			}
		}
	}
	if len(items) > provider.MaxHistoryItems {
		items = items[len(items)-provider.MaxHistoryItems:]
	}
	total := 0
	for _, item := range items {
		total += len(item.Text)
	}
	for total > provider.MaxHistoryBytes && len(items) != 0 {
		remove := 1
		if len(items) > 1 && items[0].Role == provider.HistoryUser && items[1].Role == provider.HistoryAssistant {
			remove = 2
		}
		for _, item := range items[:remove] {
			total -= len(item.Text)
		}
		items = items[remove:]
	}
	return items, nil
}

func reconcileHistory(raw json.RawMessage, expectedThreadID, brokerTurnID string) (provider.TurnState, error) {
	turns, err := decodeTurns(raw, expectedThreadID)
	if err != nil {
		return provider.TurnUnknown, err
	}
	for _, turn := range turns {
		if turn.ID == "" || turn.Status == "" || turn.Items == nil {
			return provider.TurnUnknown, errors.New("invalid thread turn")
		}
		for _, item := range turn.Items {
			if item.Type == "" {
				return provider.TurnUnknown, errors.New("invalid thread item")
			}
			if item.Type != "userMessage" {
				continue
			}
			text, textErr := strictFirstTextInput(item.Content)
			if textErr != nil {
				return provider.TurnUnknown, textErr
			}
			envelope, parseErr := provider.Parse([]byte(text))
			if parseErr != nil || envelope.TurnID != brokerTurnID {
				continue
			}
			switch turn.Status {
			case "completed":
				return provider.TurnCompleted, nil
			case "interrupted":
				return provider.TurnInterrupted, nil
			case "inProgress":
				return provider.TurnRunning, nil
			case "failed":
				return provider.TurnAccepted, nil
			default:
				return provider.TurnUnknown, nil
			}
		}
	}
	return provider.TurnNotAccepted, nil
}

func strictFirstTextInput(content []json.RawMessage) (string, error) {
	if content == nil {
		return "", errors.New("invalid user message content")
	}
	for _, raw := range content {
		var input struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &input) != nil || input.Type == "" {
			return "", errors.New("invalid user message input")
		}
		if input.Type == "text" {
			return input.Text, nil
		}
	}
	return "", nil
}

type historyTurn struct {
	ID          string        `json:"id"`
	Status      string        `json:"status"`
	StartedAt   *int64        `json:"startedAt"`
	CompletedAt *int64        `json:"completedAt"`
	Items       []historyItem `json:"items"`
}
type historyItem struct {
	Type    string            `json:"type"`
	Text    string            `json:"text"`
	Content []json.RawMessage `json:"content"`
}

func decodeTurns(raw json.RawMessage, expectedThreadID string) ([]historyTurn, error) {
	var response struct {
		Thread *struct {
			ID    string        `json:"id"`
			Turns []historyTurn `json:"turns"`
		} `json:"thread"`
	}
	if expectedThreadID == "" || json.Unmarshal(raw, &response) != nil || response.Thread == nil || response.Thread.ID != expectedThreadID || response.Thread.Turns == nil {
		return nil, errors.New("invalid thread history")
	}
	return response.Thread.Turns, nil
}
func firstTextInput(content []json.RawMessage) string {
	for _, raw := range content {
		var input struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &input) == nil && input.Type == "text" {
			return input.Text
		}
	}
	return ""
}
func turnTime(turn historyTurn) time.Time {
	seconds := turn.StartedAt
	if turn.CompletedAt != nil {
		seconds = turn.CompletedAt
	}
	if seconds == nil {
		return time.Unix(1, 0).UTC()
	}
	return time.Unix(*seconds, 0).UTC()
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}
func stringify(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	var compact bytes.Buffer
	if json.Compact(&compact, value) == nil {
		return compact.String()
	}
	return ""
}
func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}
func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ provider.Session = (*Session)(nil)
var _ provider.InteractiveSession = (*Session)(nil)
