package broker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

type turnSession struct {
	*hardeningSession
	mu                     sync.Mutex
	preflightResult        provider.PreflightResult
	preflightErr           error
	preflightEntered       chan struct{}
	preflightGate          chan struct{}
	preflightIgnoreContext bool
	submitErr              error
	submitEvent            *provider.Event
	submitGate             chan struct{}
	interruptErr           error
	interruptEvent         *provider.Event
	interruptGate          chan struct{}
	historyPage            provider.HistoryPage
	historyErr             error
	historyEntered         chan struct{}
	historyGate            chan struct{}
	providerCalls          atomic.Int32
	maximumProviderCalls   atomic.Int32
	historyCalls           atomic.Int32
	submitted              chan provider.TurnRequest
	interrupted            chan provider.AcceptedTurn
	responded              chan provider.InteractionResponse
	respondGate            chan struct{}
	respondErr             error
	cancelled              chan string
}

func newTurnSession(ref string) *turnSession {
	return &turnSession{
		hardeningSession: newHardeningSession(ref),
		preflightResult:  provider.PreflightResult{ResolvedModel: "model", EstimatedInputTokens: 10, EffectiveCapacityTokens: 100, SafetyMarginTokens: 10},
		submitted:        make(chan provider.TurnRequest, 32),
		interrupted:      make(chan provider.AcceptedTurn, 8),
		responded:        make(chan provider.InteractionResponse, 8),
		cancelled:        make(chan string, 8),
	}
}
func (session *turnSession) enterProviderCall() func() {
	current := session.providerCalls.Add(1)
	for observed := session.maximumProviderCalls.Load(); current > observed && !session.maximumProviderCalls.CompareAndSwap(observed, current); observed = session.maximumProviderCalls.Load() {
	}
	return func() { session.providerCalls.Add(-1) }
}

func (session *turnSession) History(ctx context.Context, request provider.HistoryRequest) (provider.HistoryPage, error) {
	session.historyCalls.Add(1)
	leave := session.enterProviderCall()
	defer leave()
	if request.Validate() != nil {
		return provider.HistoryPage{}, errors.New("invalid history request")
	}
	session.mu.Lock()
	page, err, entered, gate := session.historyPage, session.historyErr, session.historyEntered, session.historyGate
	session.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return provider.HistoryPage{}, ctx.Err()
		}
	}
	return page, err
}

func (session *turnSession) Preflight(ctx context.Context, request provider.PreflightRequest) (provider.PreflightResult, error) {
	leave := session.enterProviderCall()
	defer leave()
	if request.Validate() != nil {
		return provider.PreflightResult{}, errors.New("invalid preflight")
	}
	session.mu.Lock()
	result, err := session.preflightResult, session.preflightErr
	entered, gate, ignoreContext := session.preflightEntered, session.preflightGate, session.preflightIgnoreContext
	session.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		if ignoreContext {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return provider.PreflightResult{}, ctx.Err()
			}
		}
	}
	return result, err
}
func (session *turnSession) Submit(ctx context.Context, request provider.TurnRequest) (provider.AcceptedTurn, error) {
	leave := session.enterProviderCall()
	defer leave()
	if err := ctx.Err(); err != nil {
		return provider.AcceptedTurn{}, err
	}
	session.submissions.Add(1)
	captured := request
	captured.Context = cloneProviderContext(request.Context)
	select {
	case session.submitted <- captured:
	case <-ctx.Done():
		return provider.AcceptedTurn{}, ctx.Err()
	}
	session.mu.Lock()
	err, event, gate := session.submitErr, session.submitEvent, session.submitGate
	session.mu.Unlock()
	if event != nil {
		select {
		case session.events <- *event:
		case <-ctx.Done():
			return provider.AcceptedTurn{}, ctx.Err()
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return provider.AcceptedTurn{}, ctx.Err()
		}
	}
	if err != nil {
		return provider.AcceptedTurn{}, err
	}
	return provider.AcceptedTurn{TurnID: request.TurnID, AcceptedAt: testTime()}, nil
}
func (session *turnSession) Interrupt(ctx context.Context, accepted provider.AcceptedTurn) error {
	leave := session.enterProviderCall()
	defer leave()
	select {
	case session.interrupted <- accepted:
	case <-ctx.Done():
		return ctx.Err()
	}
	session.mu.Lock()
	err, event, gate := session.interruptErr, session.interruptEvent, session.interruptGate
	session.mu.Unlock()
	if event != nil {
		select {
		case session.events <- *event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (session *turnSession) Respond(ctx context.Context, response provider.InteractionResponse) error {
	if response.Validate() != nil {
		return errors.New("invalid interaction response")
	}
	select {
	case session.responded <- response:
	case <-ctx.Done():
		return ctx.Err()
	}
	session.mu.Lock()
	gate := session.respondGate
	session.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	session.mu.Lock()
	err := session.respondErr
	session.mu.Unlock()
	return err
}

func (session *turnSession) CancelInteraction(ctx context.Context, requestID string) error {
	select {
	case session.cancelled <- requestID:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestInteractiveRequestFirstResponseWinsAcrossAttachments(t *testing.T) {
	broker, _, session, first, firstClientID, identity, resource, page := turnFixture(t, 7050)
	defer broker.Close(context.Background())
	secondClientID := sequenceID(7051)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(secondClientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	second := connected.(*Connection)
	defer second.Close(context.Background())
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, second.Events()).Type)

	conversationID := first.ConversationID()
	turnID := sequenceID(7052)
	submit := submitCommand(sequenceID(7053), firstClientID, conversationID, turnID, sequenceID(7054), "run the tests", &page)
	result, err := first.Command(context.Background(), submit)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, first.Events(), 3)
	drainEvents(t, second.Events(), 2)

	requestID := sequenceID(7055)
	nativeRequest := provider.InteractionRequest{
		ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Approve command", Summary: "Run tests?",
		Command: "go test ./internal/codex", WorkingDirectory: "/workspace",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow once", Description: "Run it."}, {ID: "decline", Label: "Decline", Description: "Skip it."}},
	}
	session.events <- provider.NewInteractionRequestEvent(nativeRequest)
	for _, connection := range []*Connection{first, second} {
		event := receiveLifecycle(t, connection.Events())
		require.Equal(t, agentprotocol.EventInteractionRequest, event.Type)
		require.Equal(t, requestID, event.Payload.(agentprotocol.InteractionRequestPayload).RequestID)
	}

	gate := make(chan struct{})
	session.mu.Lock()
	session.respondGate = gate
	session.mu.Unlock()
	firstCommand := interactionResponseCommand(sequenceID(7056), firstClientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "accept")
	firstResult := make(chan agentprotocol.Event, 1)
	firstError := make(chan error, 1)
	go func() {
		event, commandErr := first.Command(context.Background(), firstCommand)
		firstResult <- event
		firstError <- commandErr
	}()
	require.Equal(t, requestID, receiveLifecycle(t, session.responded).RequestID)

	secondCommand := interactionResponseCommand(sequenceID(7057), secondClientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "decline")
	secondResult, err := second.Command(context.Background(), secondCommand)
	require.NoError(t, err)
	requireCommandResult(t, secondResult, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidState)
	require.Equal(t, secondResult, receiveLifecycle(t, second.Events()))

	close(gate)
	require.NoError(t, <-firstError)
	requireCommandResult(t, <-firstResult, agentprotocol.CommandSucceeded, "")
	for _, connection := range []*Connection{first, second} {
		resolved := receiveLifecycle(t, connection.Events())
		require.Equal(t, agentprotocol.EventInteractionResolved, resolved.Type)
		require.Equal(t, "accept", resolved.Payload.(agentprotocol.InteractionResolvedPayload).OptionID)
	}
	require.Equal(t, agentprotocol.EventCommandResult, receiveLifecycle(t, first.Events()).Type)
	select {
	case duplicate := <-session.responded:
		t.Fatalf("unexpected duplicate response: %#v", duplicate)
	default:
	}
}

func TestTerminalTurnExpiresPendingInteraction(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7060)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7061)
	submit := submitCommand(sequenceID(7062), clientID, conversationID, turnID, sequenceID(7063), "run the tests", &page)
	result, err := connection.Command(context.Background(), submit)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	requestID := sequenceID(7064)
	request := provider.InteractionRequest{
		ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval,
		Title: "Approve command", Summary: "Run tests?", Command: "go test ./...", WorkingDirectory: "/workspace",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow once", Description: "Run it."}, {ID: "decline", Label: "Decline", Description: "Skip it."}},
	}
	session.events <- provider.NewInteractionRequestEvent(request)
	require.Equal(t, agentprotocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)

	session.events <- provider.NewCompletionEvent(turnID)
	resolved := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventInteractionResolved, resolved.Type)
	require.Equal(t, requestID, resolved.Payload.(agentprotocol.InteractionResolvedPayload).RequestID)
	require.Empty(t, resolved.Payload.(agentprotocol.InteractionResolvedPayload).OptionID)
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)

	response := interactionResponseCommand(sequenceID(7065), clientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "accept")
	rejected, err := connection.Command(context.Background(), response)
	require.NoError(t, err)
	requireCommandResult(t, rejected, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidState)
	require.Equal(t, rejected, receiveLifecycle(t, connection.Events()))
	select {
	case unexpected := <-session.responded:
		t.Fatalf("terminal interaction reached provider: %#v", unexpected)
	default:
	}
}

func TestProviderResolutionConsumesPendingInteractionAndIgnoresStaleDuplicate(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7090)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7091)
	submit := submitCommand(sequenceID(7092), clientID, conversationID, turnID, sequenceID(7093), "run the tests", &page)
	result, err := connection.Command(context.Background(), submit)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	requestID := sequenceID(7094)
	request := provider.InteractionRequest{
		ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval,
		Title: "Approve command", Summary: "Run tests?", Command: "go test ./...", WorkingDirectory: "/workspace",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow once", Description: "Run it."}, {ID: "decline", Label: "Decline", Description: "Skip it."}},
	}
	session.events <- provider.NewInteractionRequestEvent(request)
	require.Equal(t, agentprotocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)

	resolution := provider.InteractionResolution{RequestID: requestID, Kind: request.Kind}
	session.events <- provider.NewInteractionResolvedEvent(resolution)
	resolved := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventInteractionResolved, resolved.Type)
	require.Equal(t, requestID, resolved.Payload.(agentprotocol.InteractionResolvedPayload).RequestID)
	require.Empty(t, resolved.Payload.(agentprotocol.InteractionResolvedPayload).OptionID)

	// A duplicate notification from the provider is stale and has no browser effect.
	session.events <- provider.NewInteractionResolvedEvent(resolution)
	session.events <- provider.NewActivityEvent(turnID, provider.ActivityStatus, "Still working.")
	require.Equal(t, agentprotocol.EventActivity, receiveLifecycle(t, connection.Events()).Type)
	response := interactionResponseCommand(sequenceID(7095), clientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "accept")
	rejected, err := connection.Command(context.Background(), response)
	require.NoError(t, err)
	requireCommandResult(t, rejected, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidState)
	require.Equal(t, rejected, receiveLifecycle(t, connection.Events()))
	select {
	case unexpected := <-session.responded:
		t.Fatalf("resolved interaction reached provider: %#v", unexpected)
	default:
	}
}

func TestFailedNativeInteractionDeliveryStillConsumesTheFirstValidResponse(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7080)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7081)
	submit := submitCommand(sequenceID(7082), clientID, conversationID, turnID, sequenceID(7083), "run the tests", &page)
	result, err := connection.Command(context.Background(), submit)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	requestID := sequenceID(7084)
	request := provider.InteractionRequest{
		ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval,
		Title: "Approve command", Summary: "Run tests?", Command: "go test ./...", WorkingDirectory: "/workspace",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow once", Description: "Run it."}, {ID: "decline", Label: "Decline", Description: "Skip it."}},
	}
	session.events <- provider.NewInteractionRequestEvent(request)
	require.Equal(t, agentprotocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)
	session.mu.Lock()
	session.respondErr = provider.NewProviderError(provider.ErrorChildExited)
	session.mu.Unlock()

	response := interactionResponseCommand(sequenceID(7085), clientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "accept")
	rejected, err := connection.Command(context.Background(), response)
	require.NoError(t, err)
	requireCommandResult(t, rejected, agentprotocol.CommandRejected, agentprotocol.ErrorProviderCrashed)
	require.Equal(t, requestID, receiveLifecycle(t, session.responded).RequestID)
	resolved := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventInteractionResolved, resolved.Type)
	require.Equal(t, "accept", resolved.Payload.(agentprotocol.InteractionResolvedPayload).OptionID)
	require.Equal(t, rejected, receiveLifecycle(t, connection.Events()))

	second := interactionResponseCommand(sequenceID(7086), clientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "decline")
	secondResult, err := connection.Command(context.Background(), second)
	require.NoError(t, err)
	requireCommandResult(t, secondResult, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidState)
	require.Equal(t, secondResult, receiveLifecycle(t, connection.Events()))
	select {
	case duplicate := <-session.responded:
		t.Fatalf("unexpected second native response: %#v", duplicate)
	default:
	}
}

func TestInteractionPendingStateRejectsCountAndAggregateByteOverflow(t *testing.T) {
	actor := &conversation{pendingInteractions: make(map[string]*pendingInteraction)}
	request := provider.InteractionRequest{
		Kind: provider.InteractionCommandApproval, Title: "Approve command",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow"}},
	}
	for index := 0; index < provider.MaxInteractionAnswers; index++ {
		request.ID = sequenceID(7400 + uint64(index))
		require.NoError(t, actor.rememberInteraction(request))
	}
	request.ID = sequenceID(7500)
	require.Error(t, actor.rememberInteraction(request))
	require.Len(t, actor.pendingInteractions, provider.MaxInteractionAnswers)

	large := provider.InteractionRequest{
		ID: sequenceID(7501), Kind: provider.InteractionMCPElicitation, Title: "MCP input",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}, {ID: "cancel", Label: "Cancel"}},
	}
	for fieldIndex := 0; fieldIndex < provider.MaxInteractionAnswers; fieldIndex++ {
		field := provider.InteractionField{ID: "field" + strconv.Itoa(fieldIndex), Label: "Field", Type: provider.InteractionMultiSelect}
		for optionIndex := 0; optionIndex < provider.MaxInteractionOptions; optionIndex++ {
			field.Options = append(field.Options, provider.InteractionOption{
				ID: "option" + strconv.Itoa(optionIndex), Label: "Option", Description: strings.Repeat("x", provider.MaxSummaryBytes),
			})
		}
		large.Fields = append(large.Fields, field)
	}
	require.NoError(t, large.Validate())
	actor = &conversation{pendingInteractions: make(map[string]*pendingInteraction)}
	require.NoError(t, actor.rememberInteraction(large))
	large.ID = sequenceID(7502)
	require.Error(t, actor.rememberInteraction(large))
	require.Len(t, actor.pendingInteractions, 1)
}

func TestInteractionProviderStreamFailsClosedAtPendingCountLimit(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7550)
	defer broker.Close(context.Background())
	session.cancelled = make(chan string, provider.MaxInteractionAnswers)
	conversationID := connection.ConversationID()
	turnID := sequenceID(7551)
	result, err := connection.Command(context.Background(), submitCommand(sequenceID(7552), clientID, conversationID, turnID, sequenceID(7553), "run", &page))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	pending := make(map[string]struct{}, provider.MaxInteractionAnswers)
	for index := 0; index < provider.MaxInteractionAnswers; index++ {
		requestID := sequenceID(7560 + uint64(index))
		pending[requestID] = struct{}{}
		session.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{
			ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Approve command",
			Options: []provider.InteractionOption{{ID: "accept", Label: "Allow"}},
		})
		event := receiveLifecycle(t, connection.Events())
		require.Equal(t, agentprotocol.EventInteractionRequest, event.Type)
		require.Equal(t, requestID, event.Payload.(agentprotocol.InteractionRequestPayload).RequestID)
	}
	overflowID := sequenceID(7599)
	session.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{
		ID: overflowID, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Approve command",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow"}},
	})
	failure := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventError, failure.Type)
	require.Equal(t, agentprotocol.ErrorProviderMalformedStream, failure.Payload.(agentprotocol.ErrorPayload).Error.Code())

	require.NoError(t, connection.Close(context.Background()))
	for index := 0; index < provider.MaxInteractionAnswers; index++ {
		cancelled := receiveLifecycle(t, session.cancelled)
		_, exists := pending[cancelled]
		require.True(t, exists, "unexpected cancellation %q", cancelled)
		delete(pending, cancelled)
	}
	require.Empty(t, pending)
	select {
	case unexpected := <-session.cancelled:
		t.Fatalf("overflow interaction entered pending state: %q", unexpected)
	default:
	}
}

func TestInteractionAfterFinalDetachIsCancelled(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7510)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7511)
	result, err := connection.Command(context.Background(), submitCommand(sequenceID(7512), clientID, conversationID, turnID, sequenceID(7513), "run", &page))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)
	require.NoError(t, connection.Close(context.Background()))

	requestID := sequenceID(7514)
	session.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{
		ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Approve command",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow"}, {ID: "decline", Label: "Decline"}},
	})
	require.Equal(t, requestID, receiveLifecycle(t, session.cancelled))
	select {
	case unexpected := <-session.responded:
		t.Fatalf("detached interaction reached provider response path: %#v", unexpected)
	default:
	}
}

func TestInteractionResponseValidationMatchesRequestSurface(t *testing.T) {
	option := func(id string) provider.InteractionOption { return provider.InteractionOption{ID: id, Label: id} }
	command := provider.InteractionRequest{
		ID: sequenceID(7520), Kind: provider.InteractionCommandApproval, Title: "Command",
		Options: []provider.InteractionOption{option("accept"), option("decline")},
	}
	permission := provider.InteractionRequest{
		ID: sequenceID(7521), Kind: provider.InteractionPermissionApproval, Title: "Permissions",
		Options: []provider.InteractionOption{option("grantTurn"), option("grantSession"), option("decline")},
		Fields:  []provider.InteractionField{{ID: "permissions", Label: "Permissions", Type: provider.InteractionMultiSelect, Options: []provider.InteractionOption{option("read"), option("write")}}},
	}
	question := provider.InteractionRequest{
		ID: sequenceID(7522), Kind: provider.InteractionUserInput, Title: "Question",
		Questions: []provider.InteractionQuestion{{ID: "target", Header: "Target", Prompt: "Choose", Options: []provider.InteractionOption{option("local"), option("remote")}}},
	}
	questionOther := question
	questionOther.Questions = append([]provider.InteractionQuestion(nil), question.Questions...)
	questionOther.Questions[0].AllowOther = true
	questionMultiple := question
	questionMultiple.Questions = append([]provider.InteractionQuestion(nil), question.Questions...)
	questionMultiple.Questions[0].Multiple = true
	questionPair := question
	questionPair.Questions = append(append([]provider.InteractionQuestion(nil), question.Questions...), provider.InteractionQuestion{ID: "profile", Header: "Profile", Prompt: "Which profile?", AllowOther: true})
	mcp := provider.InteractionRequest{
		ID: sequenceID(7523), Kind: provider.InteractionMCPElicitation, Title: "MCP",
		Options: []provider.InteractionOption{option("accept"), option("decline"), option("cancel")},
		Fields: []provider.InteractionField{
			{ID: "name", Label: "Name", Type: provider.InteractionText, Required: true},
			{ID: "count", Label: "Count", Type: provider.InteractionNumber},
			{ID: "enabled", Label: "Enabled", Type: provider.InteractionBoolean},
			{ID: "target", Label: "Target", Type: provider.InteractionSelect, Options: []provider.InteractionOption{option("local"), option("remote")}},
			{ID: "tags", Label: "Tags", Type: provider.InteractionMultiSelect, Options: []provider.InteractionOption{option("fast"), option("safe")}},
		},
	}
	tests := []struct {
		name     string
		request  provider.InteractionRequest
		optionID string
		answers  map[string][]string
		valid    bool
	}{
		{name: "offered command option", request: command, optionID: "accept", valid: true},
		{name: "unknown command option", request: command, optionID: "other"},
		{name: "command requires option", request: command, answers: map[string][]string{"target": {"local"}}},
		{name: "command rejects answers", request: command, optionID: "accept", answers: map[string][]string{"target": {"local"}}},
		{name: "permission grant selected subset", request: permission, optionID: "grantTurn", answers: map[string][]string{"permissions": {"read"}}, valid: true},
		{name: "permission grant requires selection", request: permission, optionID: "grantSession"},
		{name: "permission grant rejects unoffered selection", request: permission, optionID: "grantTurn", answers: map[string][]string{"permissions": {"admin"}}},
		{name: "permission decline has no answers", request: permission, optionID: "decline", valid: true},
		{name: "permission decline rejects answers", request: permission, optionID: "decline", answers: map[string][]string{"permissions": {"read"}}},
		{name: "single question accepts offered option", request: question, answers: map[string][]string{"target": {"local"}}, valid: true},
		{name: "single question rejects multiple answers", request: question, answers: map[string][]string{"target": {"local", "remote"}}},
		{name: "question rejects other by default", request: question, answers: map[string][]string{"target": {"custom"}}},
		{name: "question allows other", request: questionOther, answers: map[string][]string{"target": {"custom"}}, valid: true},
		{name: "multiple question accepts multiple answers", request: questionMultiple, answers: map[string][]string{"target": {"local", "remote"}}, valid: true},
		{name: "every question requires an answer", request: questionPair, answers: map[string][]string{"target": {"local"}}},
		{name: "all questions answered", request: questionPair, answers: map[string][]string{"target": {"local"}, "profile": {"default"}}, valid: true},
		{name: "MCP accept enforces required field", request: mcp, optionID: "accept"},
		{name: "MCP decline omits fields", request: mcp, optionID: "decline", valid: true},
		{name: "MCP cancel omits fields", request: mcp, optionID: "cancel", valid: true},
		{name: "MCP decline rejects fields", request: mcp, optionID: "decline", answers: map[string][]string{"name": {"value"}}},
		{name: "MCP accepts typed fields", request: mcp, optionID: "accept", answers: map[string][]string{"name": {"value"}, "count": {"1.5"}, "enabled": {"true"}, "target": {"local"}, "tags": {"fast", "safe"}}, valid: true},
		{name: "number must be canonical", request: mcp, optionID: "accept", answers: map[string][]string{"name": {"value"}, "count": {"1.50"}}},
		{name: "number must be finite", request: mcp, optionID: "accept", answers: map[string][]string{"name": {"value"}, "count": {"NaN"}}},
		{name: "boolean must be canonical", request: mcp, optionID: "accept", answers: map[string][]string{"name": {"value"}, "enabled": {"TRUE"}}},
		{name: "select requires offered option", request: mcp, optionID: "accept", answers: map[string][]string{"name": {"value"}, "target": {"other"}}},
		{name: "multi select requires offered options", request: mcp, optionID: "accept", answers: map[string][]string{"name": {"value"}, "tags": {"fast", "other"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := agentprotocol.InteractionResponsePayload{RequestID: test.request.ID, Kind: agentprotocol.InteractionKind(test.request.Kind), OptionID: test.optionID, Answers: test.answers}
			require.Equal(t, test.valid, validInteractionResponseForRequest(test.request, response))
		})
	}
}

func TestInvalidInteractionResponseDoesNotReachOrConsumeProviderRequest(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7540)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7541)
	result, err := connection.Command(context.Background(), submitCommand(sequenceID(7542), clientID, conversationID, turnID, sequenceID(7543), "run", &page))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	requestID := sequenceID(7544)
	session.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{
		ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Approve command",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Allow"}, {ID: "decline", Label: "Decline"}},
	})
	require.Equal(t, agentprotocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)

	invalid := interactionResponseCommand(sequenceID(7545), clientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "other")
	rejected, err := connection.Command(context.Background(), invalid)
	require.NoError(t, err)
	requireCommandResult(t, rejected, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidState)
	require.Equal(t, rejected, receiveLifecycle(t, connection.Events()))
	select {
	case unexpected := <-session.responded:
		t.Fatalf("invalid interaction reached provider: %#v", unexpected)
	default:
	}

	valid := interactionResponseCommand(sequenceID(7546), clientID, conversationID, requestID, agentprotocol.InteractionCommandApproval, "accept")
	accepted, err := connection.Command(context.Background(), valid)
	require.NoError(t, err)
	requireCommandResult(t, accepted, agentprotocol.CommandSucceeded, "")
	require.Equal(t, "accept", receiveLifecycle(t, session.responded).OptionID)
	require.Equal(t, agentprotocol.EventInteractionResolved, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, accepted, receiveLifecycle(t, connection.Events()))
}

func TestBufferedStructuredProviderEventsRespectReplayByteBound(t *testing.T) {
	actor := &conversation{active: &activeTurn{}}
	activity := provider.ToolActivity{
		ID: sequenceID(7070), TurnID: sequenceID(7071), Kind: provider.ToolCommand, Status: provider.ToolRunning,
		Title: "Command", Summary: "Running", Detail: strings.Repeat("x", provider.MaxInteractionTextBytes),
	}
	event := provider.NewToolActivityEvent(activity)
	require.NoError(t, event.Validate())
	buffered := 0
	for !actor.active.pendingOverflow && buffered <= MaxReplayEvents {
		actor.bufferProviderEvent(event)
		buffered++
	}
	require.True(t, actor.active.pendingOverflow)
	require.Less(t, buffered, MaxReplayEvents)
	require.Empty(t, actor.active.pendingEvents)
	require.Zero(t, actor.active.pendingBytes)
}

func interactionResponseCommand(commandID, clientID, conversationID, requestID string, kind agentprotocol.InteractionKind, optionID string) agentprotocol.Command {
	return agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandInteractionRespond,
		Payload: agentprotocol.InteractionResponsePayload{RequestID: requestID, Kind: kind, OptionID: optionID, Answers: map[string][]string{}},
	}
}

func TestSubmitCommitsContextOnceAndDeduplicatesTheCommandResult(t *testing.T) {
	broker, state, session, connection, clientID, identity, resource, page := turnFixture(t, 7001)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(7003), clientID, conversationID, sequenceID(7004), sequenceID(7005), "question", &page)

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, command.Payload.(agentprotocol.SubmitPayload).TurnID, receiveLifecycle(t, session.submitted).TurnID)

	contextEvent := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventContext, contextEvent.Type)
	require.Equal(t, agentprotocol.ContextAccepted, contextEvent.Payload.(agentprotocol.ContextPayload).State)
	lifecycle := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventLifecycle, lifecycle.Type)
	require.Equal(t, agentprotocol.LifecycleResponding, lifecycle.Payload.(agentprotocol.LifecyclePayload).State)
	streamedResult := receiveLifecycle(t, connection.Events())
	require.Equal(t, result, streamedResult)

	state.mu.Lock()
	require.Equal(t, 1, state.prepareCalls)
	require.Equal(t, 1, state.acceptCalls)
	require.Equal(t, 1, state.promoteCalls)
	require.Nil(t, state.mapping.Current.Observed)
	require.Nil(t, state.mapping.Current.PreparedCommit)
	require.Equal(t, page.Digest, state.mapping.Current.Committed.Digest)
	state.mu.Unlock()

	duplicate, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, result, duplicate)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	require.EqualValues(t, 1, session.submissions.Load())

	changed := command
	payload := changed.Payload.(agentprotocol.SubmitPayload)
	payload.Content = agentprotocol.TextContent("different")
	changed.Payload = payload
	rejected, err := connection.Command(context.Background(), changed)
	require.NoError(t, err)
	requireCommandResult(t, rejected, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidCommand)
	require.EqualValues(t, 1, session.submissions.Load())
	_ = identity
	_ = resource
}

func TestFollowUpQueueEditRemoveFIFOAndInterruptPreserveQueue(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7101)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(7106)
	active := submitCommand(sequenceID(7103), clientID, conversationID, activeTurnID, sequenceID(7105), "active", &page)
	result, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, activeTurnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	first := submitCommand(sequenceID(7110), clientID, conversationID, sequenceID(7111), sequenceID(7112), "first", nil)
	result, err = connection.Command(context.Background(), first)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	queueEvent := receiveLifecycle(t, connection.Events())
	require.Equal(t, []agentprotocol.QueueItem{{TurnID: sequenceID(7111), MessageID: sequenceID(7112), Content: agentprotocol.TextContent("first")}}, queueEvent.Payload.(agentprotocol.QueuePayload).Items)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	second := submitCommand(sequenceID(7113), clientID, conversationID, sequenceID(7114), sequenceID(7115), "second", nil)
	result, err = connection.Command(context.Background(), second)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	drainEvents(t, connection.Events(), 2)

	edit := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(7116), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandQueueEdit, Payload: agentprotocol.QueueEditPayload{MessageID: sequenceID(7112), Content: agentprotocol.TextContent("edited")}}
	result, err = connection.Command(context.Background(), edit)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	queueEvent = receiveLifecycle(t, connection.Events())
	require.Equal(t, "edited", queueEvent.Payload.(agentprotocol.QueuePayload).Items[0].Content.Parts[0].Text)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	remove := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(7117), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandQueueRemove, Payload: agentprotocol.MessageReferencePayload{MessageID: sequenceID(7115)}}
	result, err = connection.Command(context.Background(), remove)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	queueEvent = receiveLifecycle(t, connection.Events())
	require.Len(t, queueEvent.Payload.(agentprotocol.QueuePayload).Items, 1)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	interrupt := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(7118), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandInterrupt, Payload: agentprotocol.TurnReferencePayload{TurnID: activeTurnID}}
	result, err = connection.Command(context.Background(), interrupt)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, activeTurnID, receiveLifecycle(t, session.interrupted).TurnID)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	session.events <- provider.NewInterruptionEvent(activeTurnID, provider.InterruptionRequested)
	require.Equal(t, agentprotocol.EventInterruption, receiveLifecycle(t, connection.Events()).Type)
	queueEvent = receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventQueue, queueEvent.Type)
	require.Empty(t, queueEvent.Payload.(agentprotocol.QueuePayload).Items)
	require.Equal(t, agentprotocol.LifecycleConnecting, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	dispatched := receiveLifecycle(t, session.submitted)
	require.Equal(t, sequenceID(7111), dispatched.TurnID)
	require.Equal(t, "edited", dispatched.Content.PlainText())
	require.Equal(t, agentprotocol.LifecycleResponding, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
}

func TestResyncAfterNewestEventEmitsSnapshotCheckpointThenStableResult(t *testing.T) {
	broker, _, _, _, clientID, identity, resource, page := turnFixture(t, 7090)
	defer broker.Close(context.Background())
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	defer connection.Close(context.Background())
	checkpoint := receiveLifecycle(t, connection.Events())
	conversationID := connection.ConversationID()
	command := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(7092), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandResync, Payload: agentprotocol.ResyncPayload{AfterEventID: checkpoint.EventID}}

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	duplicate, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, result, duplicate)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
}

func TestActiveAndQueueIdentifiersStayUniqueAndQueueLimitsRejectWithoutMutation(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7130)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(7132)
	activeMessageID := sequenceID(7133)
	active := submitCommand(sequenceID(7134), clientID, conversationID, activeTurnID, activeMessageID, "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)

	duplicate := submitCommand(sequenceID(7135), clientID, conversationID, activeTurnID, sequenceID(7136), "duplicate", nil)
	result, err := connection.Command(context.Background(), duplicate)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidState)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	for index := 0; index < agentprotocol.MaxQueueItems; index++ {
		command := submitCommand(sequenceID(uint64(8000+index)), clientID, conversationID, sequenceID(uint64(8100+index)), sequenceID(uint64(8200+index)), "queued", nil)
		result, err = connection.Command(context.Background(), command)
		require.NoError(t, err)
		requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
		drainEvents(t, connection.Events(), 2)
	}
	overflow := submitCommand(sequenceID(8300), clientID, conversationID, sequenceID(8301), sequenceID(8302), "overflow", nil)
	result, err = connection.Command(context.Background(), overflow)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorQueueFull)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
}

func TestCompletionBeforeSubmitReturnsIsPublishedAfterAcceptanceAndCommandResult(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7119)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7120)
	completion := provider.NewCompletionEvent(turnID)
	gate := make(chan struct{})
	session.mu.Lock()
	session.submitEvent = &completion
	session.submitGate = gate
	session.mu.Unlock()
	command := submitCommand(sequenceID(7121), clientID, conversationID, turnID, sequenceID(7122), "question", &page)
	commandDone := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), command)
		commandDone <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, session.submitted)
	session.events <- provider.NewActivityEvent("", provider.ActivityStatus, "event barrier")
	require.Equal(t, agentprotocol.EventActivity, receiveLifecycle(t, connection.Events()).Type)
	close(gate)
	response := receiveLifecycle(t, commandDone)
	require.NoError(t, response.err)
	requireCommandResult(t, response.event, agentprotocol.CommandSucceeded, "")
	require.Equal(t, agentprotocol.EventContext, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleResponding, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.Equal(t, response.event, receiveLifecycle(t, connection.Events()))
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
}

func TestCompletionRacingFailedInterruptStillTerminatesAndDrainsQueue(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7123)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7124)
	active := submitCommand(sequenceID(7125), clientID, conversationID, turnID, sequenceID(7126), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)
	queuedTurnID := sequenceID(7127)
	queued := submitCommand(sequenceID(7128), clientID, conversationID, queuedTurnID, sequenceID(7129), "queued", nil)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	completion := provider.NewCompletionEvent(turnID)
	gate := make(chan struct{})
	session.mu.Lock()
	session.interruptEvent = &completion
	session.interruptGate = gate
	session.interruptErr = provider.NewProviderError(provider.ErrorProtocolFailure)
	session.mu.Unlock()
	interrupt := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(7130), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandInterrupt, Payload: agentprotocol.TurnReferencePayload{TurnID: turnID}}
	commandDone := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), interrupt)
		commandDone <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, session.interrupted)
	session.events <- provider.NewActivityEvent("", provider.ActivityStatus, "interrupt barrier")
	require.Equal(t, agentprotocol.EventActivity, receiveLifecycle(t, connection.Events()).Type)
	close(gate)
	response := receiveLifecycle(t, commandDone)
	require.NoError(t, response.err)
	requireCommandResult(t, response.event, agentprotocol.CommandRejected, agentprotocol.ErrorProviderProtocolFailure)
	require.Equal(t, response.event, receiveLifecycle(t, connection.Events()))
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	drainEvents(t, connection.Events(), 2)
	require.Equal(t, queuedTurnID, receiveLifecycle(t, session.submitted).TurnID)
}

func TestProviderEventsRequireTheActiveTurnAndUseProviderUserMessageAuthority(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7120)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7122)
	messageID := sequenceID(7123)
	command := submitCommand(sequenceID(7124), clientID, conversationID, turnID, messageID, "question", &page)
	_, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, turnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	session.events <- provider.NewUserMessageEvent(sequenceID(7125), messageID, provider.TextMessage("wrong turn"), testTime())
	malformed := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventError, malformed.Type)
	require.Equal(t, agentprotocol.ErrorProviderMalformedStream, malformed.Payload.(agentprotocol.ErrorPayload).Error.Code())

	session.events <- provider.NewUserMessageEvent(turnID, messageID, provider.TextMessage("question"), testTime())
	user := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventUserMessage, user.Type)
	require.Equal(t, "question", user.Payload.(agentprotocol.UserMessagePayload).Content.Parts[0].Text)
	session.events <- provider.NewCompletionEvent(turnID)
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
}

func TestSessionWideTerminalFailureInterruptsActiveTurnAndRecoversQueuedWork(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7136)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7137)
	active := submitCommand(sequenceID(7138), clientID, conversationID, turnID, sequenceID(7139), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)
	queuedTurnID := sequenceID(7140)
	queued := submitCommand(sequenceID(7141), clientID, conversationID, queuedTurnID, sequenceID(7142), "queued", nil)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)
	secondQueuedTurnID := sequenceID(7200)
	secondQueued := submitCommand(sequenceID(7201), clientID, conversationID, secondQueuedTurnID, sequenceID(7202), "second queued", nil)
	_, err = connection.Command(context.Background(), secondQueued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	session.events <- provider.NewTerminalFailureEvent("", provider.NewProviderError(provider.ErrorChildExited))
	failure := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventError, failure.Type)
	require.Equal(t, agentprotocol.ErrorProviderCrashed, failure.Payload.(agentprotocol.ErrorPayload).Error.Code())
	interrupted := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventInterruption, interrupted.Type)
	require.Equal(t, agentprotocol.InterruptionProviderExit, interrupted.Payload.(agentprotocol.InterruptionPayload).Reason)
	require.Equal(t, turnID, interrupted.Payload.(agentprotocol.InterruptionPayload).TurnID)
	require.Equal(t, agentprotocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.Equal(t, agentprotocol.EventQueue, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleConnecting, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.Equal(t, queuedTurnID, receiveLifecycle(t, session.submitted).TurnID)
	require.Equal(t, agentprotocol.LifecycleResponding, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	session.events <- provider.NewCompletionEvent(queuedTurnID)
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.EventQueue, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleConnecting, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.Equal(t, secondQueuedTurnID, receiveLifecycle(t, session.submitted).TurnID)
	require.Equal(t, agentprotocol.LifecycleResponding, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.EqualValues(t, 3, session.submissions.Load())
}

func TestTurnScopedTerminalFailureDoesNotPromoteToSessionCrash(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7144)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(7145)
	active := submitCommand(sequenceID(7146), clientID, conversationID, turnID, sequenceID(7147), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)
	queuedTurnID := sequenceID(7148)
	queued := submitCommand(sequenceID(7149), clientID, conversationID, queuedTurnID, sequenceID(7150), "queued", nil)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	session.events <- provider.NewTerminalFailureEvent(turnID, provider.NewProviderError(provider.ErrorProtocolFailure))
	failure := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventError, failure.Type)
	require.Equal(t, agentprotocol.ErrorProviderProtocolFailure, failure.Payload.(agentprotocol.ErrorPayload).Error.Code())
	queue := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventQueue, queue.Type)
	require.Empty(t, queue.Payload.(agentprotocol.QueuePayload).Items)
	require.Equal(t, agentprotocol.LifecycleConnecting, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.Equal(t, queuedTurnID, receiveLifecycle(t, session.submitted).TurnID)
	require.Equal(t, agentprotocol.LifecycleResponding, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
}

func TestBrokerShutdownCancelsAndJoinsAnInFlightProviderWorker(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7140)
	entered := make(chan struct{}, 1)
	session.mu.Lock()
	session.preflightEntered = entered
	session.preflightGate = make(chan struct{})
	session.mu.Unlock()
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(7142), clientID, conversationID, sequenceID(7143), sequenceID(7144), "question", &page)
	commandResult := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), command)
		commandResult <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, entered)

	require.NoError(t, broker.Close(context.Background()))
	response := receiveLifecycle(t, commandResult)
	require.NoError(t, response.err)
	requireCommandResult(t, response.event, agentprotocol.CommandRejected, agentprotocol.ErrorProviderProtocolFailure)
	require.EqualValues(t, 1, session.shutdowns.Load())
}

func TestActorShutdownDoesNotKillChildThatExitsDuringTerminateGrace(t *testing.T) {
	exited := make(chan struct{})
	child := &hardeningChild{waitGate: exited, terminateSignal: exited}
	session := newHardeningSession("sessions/graceful-terminate")
	session.child = child
	session.shutdownErr = errors.New("native shutdown failed")
	attempt := newActorShutdown(captureSession(session), nil)

	require.NoError(t, attempt.run(context.Background(), 40*time.Millisecond))
	child.mu.Lock()
	require.Equal(t, []string{"terminate", "wait"}, child.order)
	child.mu.Unlock()
	require.EqualValues(t, 1, session.shutdowns.Load())
}

func TestActorShutdownBoundsPostKillJoinByConfiguredTimeout(t *testing.T) {
	waitGate := make(chan struct{})
	shutdownGate := make(chan struct{})
	workerGate := make(chan struct{})
	child := &hardeningChild{waitGate: waitGate}
	session := newHardeningSession("sessions/noncooperative-join")
	session.child = child
	session.shutdownFunc = func(context.Context) error {
		<-shutdownGate
		return nil
	}
	attempt := newActorShutdown(captureSession(session), workerGate)
	callerCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()

	require.Error(t, attempt.run(callerCtx, 20*time.Millisecond))
	require.Less(t, time.Since(started), 60*time.Millisecond)
	close(waitGate)
	close(shutdownGate)
	close(workerGate)
}

func TestActorShutdownEscalatesAndJoinsNonCooperativeTurnWorker(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 7145)
	broker.shutdownTimeout = 40 * time.Millisecond
	connection.actor.shutdownTimeout = 40 * time.Millisecond
	child := nonCooperativeHardeningChild(1)
	session.child = child
	connection.actor.session.child = child
	session.shutdownFunc = func(context.Context) error {
		<-child.killSignal
		return nil
	}
	session.mu.Lock()
	session.preflightEntered = make(chan struct{}, 1)
	session.preflightGate = child.killSignal
	session.preflightIgnoreContext = true
	session.mu.Unlock()
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(7146), clientID, conversationID, sequenceID(7147), sequenceID(7148), "question", &page)
	commandDone := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), command)
		commandDone <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, session.preflightEntered)

	closed := make(chan error, 1)
	go func() { closed <- broker.Close(context.Background()) }()
	require.Error(t, receiveLifecycle(t, closed))
	go func() { closed <- broker.Close(context.Background()) }()
	require.NoError(t, receiveLifecycle(t, closed))
	response := receiveLifecycle(t, commandDone)
	require.NoError(t, response.err)
	requireCommandResult(t, response.event, agentprotocol.CommandRejected, agentprotocol.ErrorProviderProtocolFailure)
	child.mu.Lock()
	require.Equal(t, []string{"terminate", "wait", "kill", "kill"}, child.order)
	child.mu.Unlock()
	require.EqualValues(t, 1, session.shutdowns.Load())
}

func TestActorShutdownEscalatesIgnoredGracefulStopInOrder(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(7150))
	state := &hardeningState{mapping: &mapping}
	child := nonCooperativeHardeningChild(0)
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	session.child = child
	session.shutdownFunc = func(context.Context) error {
		<-child.killSignal
		return nil
	}
	config := validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 7151})
	config.ShutdownTimeout = 200 * time.Millisecond
	broker, err := New(config)
	require.NoError(t, err)
	resource := testResource(identity.CapabilityID)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(7152), identity.CapabilityID, strings.Repeat("b", 64), resource, ""))
	require.NoError(t, err)
	receiveLifecycle(t, connected.Events())

	closed := make(chan error, 1)
	go func() { closed <- broker.Close(context.Background()) }()
	require.NoError(t, receiveLifecycle(t, closed))
	child.mu.Lock()
	require.Equal(t, []string{"terminate", "wait", "kill"}, child.order)
	child.mu.Unlock()
	require.EqualValues(t, 1, session.shutdowns.Load())
}

func TestDisconnectedConversationContinuesDrainingQueuedTurns(t *testing.T) {
	broker, _, session, connection, clientID, identity, resource, page := turnFixture(t, 7160)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(7162)
	active := submitCommand(sequenceID(7163), clientID, conversationID, activeTurnID, sequenceID(7164), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	require.Equal(t, activeTurnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)
	queuedTurnID := sequenceID(7165)
	queued := submitCommand(sequenceID(7166), clientID, conversationID, queuedTurnID, sequenceID(7167), "queued", nil)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)
	require.NoError(t, connection.Close(context.Background()))

	session.events <- provider.NewCompletionEvent(activeTurnID)
	require.Equal(t, queuedTurnID, receiveLifecycle(t, session.submitted).TurnID)

	reconnected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(7168), identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	defer reconnected.Close(context.Background())
	snapshot := receiveLifecycle(t, reconnected.Events()).Payload.(agentprotocol.SnapshotPayload)
	require.Empty(t, snapshot.Queue)
	if snapshot.Lifecycle == agentprotocol.LifecycleConnecting {
		lifecycle := receiveLifecycle(t, reconnected.Events()).Payload.(agentprotocol.LifecyclePayload)
		require.Equal(t, agentprotocol.LifecycleResponding, lifecycle.State)
		require.Equal(t, queuedTurnID, *lifecycle.TurnID)
	} else {
		require.Equal(t, agentprotocol.LifecycleResponding, snapshot.Lifecycle)
		require.NotNil(t, snapshot.ActiveTurnID)
		require.Equal(t, queuedTurnID, *snapshot.ActiveTurnID)
	}
}

func TestNewRevisionAttachesToTheNextExistingQueuedMessage(t *testing.T) {
	broker, state, session, connection, clientID, identity, resource, page := turnFixture(t, 7180)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(7182)
	active := submitCommand(sequenceID(7183), clientID, conversationID, activeTurnID, sequenceID(7184), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	require.Equal(t, activeTurnID, receiveLifecycle(t, session.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)

	firstTurnID := sequenceID(7185)
	first := submitCommand(sequenceID(7186), clientID, conversationID, firstTurnID, sequenceID(7187), "already queued", nil)
	_, err = connection.Command(context.Background(), first)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	newResource := resource
	newResource.UpdatedAt = resource.UpdatedAt.Add(lifecycleTestTimeout)
	newPage := testPageContext(newResource)
	newPage.Revision = agentprotocol.ContextReplacement
	newPage.Markdown = "# Replacement\n"
	newPage.Digest = contextdigest.Calculate([]byte(newPage.Markdown), []byte(newPage.CreatorContext))
	secondClientID := sequenceID(7188)
	second, err := broker.Connect(context.Background(), identity.Origin, observationConnect(secondClientID, identity.CapabilityID, newPage.Digest, newResource, ""))
	require.NoError(t, err)
	defer second.Close(context.Background())
	require.Equal(t, agentprotocol.ContextPending, receiveLifecycle(t, second.Events()).Payload.(agentprotocol.SnapshotPayload).ContextState)
	require.Equal(t, agentprotocol.EventContext, receiveLifecycle(t, connection.Events()).Type)

	third := submitCommand(sequenceID(7189), clientID, conversationID, sequenceID(7190), sequenceID(7191), "newer question", &newPage)
	_, err = connection.Command(context.Background(), third)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)
	require.Equal(t, agentprotocol.EventQueue, receiveLifecycle(t, second.Events()).Type)

	session.events <- provider.NewCompletionEvent(activeTurnID)
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	drainEvents(t, connection.Events(), 2)
	dispatched := receiveLifecycle(t, session.submitted)
	require.Equal(t, firstTurnID, dispatched.TurnID)
	require.Equal(t, "already queued", dispatched.Content.PlainText())
	require.NotNil(t, dispatched.Context)
	require.Equal(t, newPage.Digest, dispatched.Context.Digest)
	require.Equal(t, []byte(newPage.Markdown), dispatched.Context.Markdown)
	require.Equal(t, agentprotocol.EventContext, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleResponding, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	state.mu.Lock()
	require.Equal(t, newPage.Digest, state.mapping.Current.Committed.Digest)
	state.mu.Unlock()
}

func TestNewerObservedRevisionInvalidatesQueuedContextBeforeDispatch(t *testing.T) {
	broker, state, session, connection, clientID, identity, resource, page := turnFixture(t, 7192)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(7193)
	active := submitCommand(sequenceID(7194), clientID, conversationID, activeTurnID, sequenceID(7195), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)
	firstTurnID := sequenceID(7196)
	first := submitCommand(sequenceID(7197), clientID, conversationID, firstTurnID, sequenceID(7198), "first", nil)
	_, err = connection.Command(context.Background(), first)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	revisionB := replacementPage(resource, "# Revision B\n", lifecycleTestTimeout)
	attachedB, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(7199), identity.CapabilityID, revisionB.Digest, revisionB.Resource, ""))
	require.NoError(t, err)
	defer attachedB.Close(context.Background())
	receiveLifecycle(t, attachedB.Events())
	require.Equal(t, agentprotocol.EventContext, receiveLifecycle(t, connection.Events()).Type)
	questionB := submitCommand(sequenceID(7200), clientID, conversationID, sequenceID(7201), sequenceID(7202), "question B", &revisionB)
	_, err = connection.Command(context.Background(), questionB)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	revisionC := replacementPage(revisionB.Resource, "# Revision C\n", lifecycleTestTimeout)
	attachedC, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(7203), identity.CapabilityID, revisionC.Digest, revisionC.Resource, ""))
	require.NoError(t, err)
	defer attachedC.Close(context.Background())
	receiveLifecycle(t, attachedC.Events())
	require.Equal(t, agentprotocol.EventContext, receiveLifecycle(t, connection.Events()).Type)

	session.events <- provider.NewCompletionEvent(activeTurnID)
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)

	questionC := submitCommand(sequenceID(7204), clientID, conversationID, sequenceID(7205), sequenceID(7206), "question C", &revisionC)
	result, err := connection.Command(context.Background(), questionC)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	dispatched := receiveLifecycle(t, session.submitted)
	require.Equal(t, firstTurnID, dispatched.TurnID)
	require.NotNil(t, dispatched.Context)
	require.Equal(t, revisionC.Digest, dispatched.Context.Digest)
	drainEvents(t, connection.Events(), 4)
	acceptedContext := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventContext, acceptedContext.Type)
	require.Equal(t, revisionC.Digest, acceptedContext.Payload.(agentprotocol.ContextPayload).Digest)
	require.Equal(t, agentprotocol.LifecycleResponding, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	state.mu.Lock()
	require.Equal(t, revisionC.Digest, state.mapping.Current.Committed.Digest)
	state.mu.Unlock()
}

func TestRemovingContextBearingHeadPausesQueueUntilReplacementIsSupplied(t *testing.T) {
	broker, _, session, connection, clientID, identity, resource, page := turnFixture(t, 7210)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(7211)
	active := submitCommand(sequenceID(7212), clientID, conversationID, activeTurnID, sequenceID(7213), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)

	replacement := replacementPage(resource, "# Replacement\n", lifecycleTestTimeout)
	attached, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(7214), identity.CapabilityID, replacement.Digest, replacement.Resource, ""))
	require.NoError(t, err)
	defer attached.Close(context.Background())
	receiveLifecycle(t, attached.Events())
	require.Equal(t, agentprotocol.EventContext, receiveLifecycle(t, connection.Events()).Type)
	headMessageID := sequenceID(7215)
	head := submitCommand(sequenceID(7216), clientID, conversationID, sequenceID(7217), headMessageID, "head", &replacement)
	_, err = connection.Command(context.Background(), head)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)
	secondTurnID := sequenceID(7218)
	second := submitCommand(sequenceID(7219), clientID, conversationID, secondTurnID, sequenceID(7220), "second", nil)
	_, err = connection.Command(context.Background(), second)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)
	remove := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(7221), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandQueueRemove, Payload: agentprotocol.MessageReferencePayload{MessageID: headMessageID}}
	_, err = connection.Command(context.Background(), remove)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	session.events <- provider.NewCompletionEvent(activeTurnID)
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)

	third := submitCommand(sequenceID(7222), clientID, conversationID, sequenceID(7223), sequenceID(7224), "third", &replacement)
	_, err = connection.Command(context.Background(), third)
	require.NoError(t, err)
	dispatched := receiveLifecycle(t, session.submitted)
	require.Equal(t, secondTurnID, dispatched.TurnID)
	require.NotNil(t, dispatched.Context)
	require.Equal(t, replacement.Digest, dispatched.Context.Digest)
}

func replacementPage(previous agentprotocol.Resource, markdown string, advance time.Duration) agentprotocol.PageContext {
	resource := previous
	resource.UpdatedAt = previous.UpdatedAt.Add(advance)
	page := testPageContext(resource)
	page.Revision = agentprotocol.ContextReplacement
	page.Markdown = markdown
	page.Digest = contextdigest.Calculate([]byte(markdown), []byte(page.CreatorContext))
	return page
}

func TestDefinitePreflightRejectionKeepsObservationPendingAndNeverSubmits(t *testing.T) {
	broker, state, session, connection, clientID, _, _, page := turnFixture(t, 7201)
	defer broker.Close(context.Background())
	session.mu.Lock()
	session.preflightErr = provider.NewProviderError(provider.ErrorContextTooLarge)
	session.mu.Unlock()
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(7203), clientID, conversationID, sequenceID(7204), sequenceID(7205), "question", &page)

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorContextTooLarge)
	require.EqualValues(t, 0, session.submissions.Load())
	require.Equal(t, agentprotocol.EventError, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	state.mu.Lock()
	require.Equal(t, 1, state.prepareCalls)
	require.Equal(t, 1, state.reconcileCalls)
	require.NotNil(t, state.mapping.Current.Observed)
	require.Nil(t, state.mapping.Current.PreparedCommit)
	state.mu.Unlock()
}

func TestTurnDurableMutationsRetryOnlyFromProvenPreconditions(t *testing.T) {
	failure := errors.New("commit outcome unavailable")
	for _, test := range []struct {
		name          string
		prepare       []repairMutation
		accept        []repairMutation
		promote       []repairMutation
		reconcile     []repairMutation
		preflightErr  error
		wantStatus    agentprotocol.CommandStatus
		wantCode      agentprotocol.BrowserErrorCode
		wantPrepare   int
		wantAccept    int
		wantPromote   int
		wantReconcile int
	}{
		{name: "prepare uncertain precondition", prepare: []repairMutation{{outcome: agentstate.CommitUncertain, err: failure}, {outcome: agentstate.CommitApplied, apply: true}}, promote: []repairMutation{{outcome: agentstate.CommitApplied, apply: true}}, wantStatus: agentprotocol.CommandSucceeded, wantPrepare: 2, wantAccept: 1, wantPromote: 1},
		{name: "accept uncertain precondition", accept: []repairMutation{{outcome: agentstate.CommitUncertain, err: failure}, {outcome: agentstate.CommitApplied, apply: true}}, promote: []repairMutation{{outcome: agentstate.CommitApplied, apply: true}}, wantStatus: agentprotocol.CommandSucceeded, wantPrepare: 1, wantAccept: 2, wantPromote: 1},
		{name: "promote uncertain precondition", promote: []repairMutation{{outcome: agentstate.CommitUncertain, err: failure}, {outcome: agentstate.CommitApplied, apply: true}}, wantStatus: agentprotocol.CommandSucceeded, wantPrepare: 1, wantAccept: 1, wantPromote: 2},
		{name: "reconcile uncertain precondition", reconcile: []repairMutation{{outcome: agentstate.CommitUncertain, err: failure}, {outcome: agentstate.CommitApplied, apply: true}}, preflightErr: provider.NewProviderError(provider.ErrorContextTooLarge), wantStatus: agentprotocol.CommandRejected, wantCode: agentprotocol.ErrorContextTooLarge, wantPrepare: 1, wantReconcile: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			broker, state, session, connection, clientID, _, _, page := turnFixture(t, 7250)
			defer broker.Close(context.Background())
			state.mu.Lock()
			state.preparations = test.prepare
			state.acceptances = test.accept
			if test.promote != nil {
				state.promotions = test.promote
			}
			if test.reconcile != nil {
				state.reconciliations = test.reconcile
			}
			state.mu.Unlock()
			session.mu.Lock()
			session.preflightErr = test.preflightErr
			session.mu.Unlock()
			conversationID := connection.ConversationID()
			command := submitCommand(sequenceID(7260), clientID, conversationID, sequenceID(7261), sequenceID(7262), "question", &page)

			result, err := connection.Command(context.Background(), command)
			require.NoError(t, err)
			requireCommandResult(t, result, test.wantStatus, test.wantCode)
			state.mu.Lock()
			require.Equal(t, test.wantPrepare, state.prepareCalls)
			require.Equal(t, test.wantAccept, state.acceptCalls)
			require.Equal(t, test.wantPromote, state.promoteCalls)
			require.Equal(t, test.wantReconcile, state.reconcileCalls)
			state.mu.Unlock()
			if test.preflightErr == nil {
				require.EqualValues(t, 1, session.submissions.Load())
			} else {
				require.Zero(t, session.submissions.Load())
			}
		})
	}
}

func TestTurnDurableMutationDoesNotRetryAfterUnrelatedLoadedState(t *testing.T) {
	broker, state, session, connection, clientID, _, _, page := turnFixture(t, 7280)
	defer broker.Close(context.Background())
	state.mu.Lock()
	other := cloneMapping(*state.mapping)
	other.Current.ModelLabel = "other-model"
	state.preparations = []repairMutation{{outcome: agentstate.CommitUncertain, err: errors.New("uncertain"), mapping: &other}}
	state.mu.Unlock()
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(7282), clientID, conversationID, sequenceID(7283), sequenceID(7284), "question", &page)

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorStateRepairFailed)
	state.mu.Lock()
	require.Equal(t, 1, state.prepareCalls)
	state.mu.Unlock()
	require.Zero(t, session.submissions.Load())
}

func TestAcceptanceUnknownRetainsPreparedStateAndDeduplicatesWithoutResubmit(t *testing.T) {
	broker, state, session, connection, clientID, _, _, page := turnFixture(t, 7301)
	defer broker.Close(context.Background())
	session.mu.Lock()
	session.submitErr = provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	session.mu.Unlock()
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(7303), clientID, conversationID, sequenceID(7304), sequenceID(7305), "question", &page)

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorAcceptanceOutcomeUnknown)
	require.Equal(t, agentprotocol.EventError, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.LifecyclePayload).State)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	state.mu.Lock()
	require.NotNil(t, state.mapping.Current.PreparedCommit)
	require.Equal(t, agentstate.CommitPrepared, state.mapping.Current.PreparedCommit.Phase)
	require.Zero(t, state.reconcileCalls)
	state.mu.Unlock()

	duplicate, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, result, duplicate)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	require.EqualValues(t, 1, session.submissions.Load())
}

func turnFixture(t *testing.T, base uint64) (*Broker, *repairState, *turnSession, *Connection, string, agentstate.Identity, agentprotocol.Resource, agentprotocol.PageContext) {
	t.Helper()
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(base))
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	observed := agentstate.Revision{Digest: page.Digest, Revision: agentstate.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Observed = &observed
	state := &repairState{
		mapping: &mapping,
		promotions: []repairMutation{
			{outcome: agentstate.CommitApplied, apply: true},
			{outcome: agentstate.CommitApplied, apply: true},
		},
		reconciliations: []repairMutation{
			{outcome: agentstate.CommitApplied, apply: true},
			{outcome: agentstate.CommitApplied, apply: true},
		},
	}
	session := newTurnSession(mapping.Current.NativeSession.Value())
	broker, err := New(validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: base + 100}))
	require.NoError(t, err)
	clientID := sequenceID(base + 1)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)
	return broker, state, session, connection, clientID, identity, resource, page
}

func submitCommand(commandID, clientID, conversationID, turnID, messageID, message string, page *agentprotocol.PageContext) agentprotocol.Command {
	return agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandSubmit, Payload: agentprotocol.SubmitPayload{TurnID: turnID, MessageID: messageID, Content: agentprotocol.TextContent(message), Context: page}}
}

func requireCommandResult(t *testing.T, event agentprotocol.Event, status agentprotocol.CommandStatus, code agentprotocol.BrowserErrorCode) {
	t.Helper()
	require.Equal(t, agentprotocol.EventCommandResult, event.Type)
	payload := event.Payload.(agentprotocol.CommandResultPayload)
	require.Equal(t, status, payload.Status)
	if code == "" {
		require.Nil(t, payload.Error)
		return
	}
	require.NotNil(t, payload.Error)
	require.Equal(t, code, payload.Error.Code())
}

func drainEvents(t *testing.T, events <-chan agentprotocol.Event, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		receiveLifecycle(t, events)
	}
}
