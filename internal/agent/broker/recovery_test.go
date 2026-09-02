package broker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/stretchr/testify/require"
)

type recoverySequenceDriver struct {
	mu         sync.Mutex
	sessions   []provider.Session
	errors     []error
	requests   []provider.ResumeRequest
	resumeHook func(context.Context, int, provider.ResumeRequest) (provider.Session, error)
	deletes    int
}

func (*recoverySequenceDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "model"}
}
func (*recoverySequenceDriver) Create(context.Context, provider.CreateRequest) (provider.Session, error) {
	return nil, errors.New("create not expected")
}
func (driver *recoverySequenceDriver) Resume(ctx context.Context, request provider.ResumeRequest) (provider.Session, error) {
	driver.mu.Lock()
	index := len(driver.requests)
	driver.requests = append(driver.requests, request)
	hook := driver.resumeHook
	var session provider.Session
	var err error
	if index < len(driver.sessions) {
		session = driver.sessions[index]
	}
	if index < len(driver.errors) {
		err = driver.errors[index]
	}
	driver.mu.Unlock()
	if hook != nil {
		return hook(ctx, index, request)
	}
	return session, err
}
func (*recoverySequenceDriver) Inspect(context.Context, provider.InspectRequest) (provider.NativeSession, error) {
	return provider.NativeSession{}, nil
}
func (driver *recoverySequenceDriver) Delete(context.Context, provider.DeleteRequest) error {
	driver.mu.Lock()
	driver.deletes++
	driver.mu.Unlock()
	return nil
}

type recoveryCountingSession struct {
	provider.Session
	nativeCalls atomic.Int32
	eventsCalls atomic.Int32
	childCalls  atomic.Int32
}

func (session *recoveryCountingSession) NativeSession() provider.NativeSession {
	session.nativeCalls.Add(1)
	return session.Session.NativeSession()
}
func (session *recoveryCountingSession) Events() <-chan provider.Event {
	session.eventsCalls.Add(1)
	return session.Session.Events()
}
func (session *recoveryCountingSession) Child() provider.ManagedChild {
	session.childCalls.Add(1)
	return session.Session.Child()
}

func TestRecoveryExpiresIdleSessionInteractionBeforeReplacement(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1250))
	state := &hardeningState{mapping: &mapping}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	replacement := newTurnSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, replacement}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1260}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	connected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1261), identity.CapabilityID))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	requestID := sequenceID(1262)
	old.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve", Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}}})
	require.Equal(t, protocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)
	close(old.events)
	resolved := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventInteractionResolved, resolved.Type)
	require.Empty(t, resolved.Payload.(protocol.InteractionResolvedPayload).OptionID)
	requireRecoveryCycle(t, connection.Events())
	require.Equal(t, 1, replayInteractionResolutionCount(t, connection.actor.replay, requestID))
}

func TestAcceptedSubmitTerminalSettlesBeforeImmediateProviderClosureRecovery(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1263))
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	mapping.Current.Observed = &statepkg.Revision{Digest: page.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	state := &repairState{mapping: &mapping, promotions: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}}}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	replacement := newTurnSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, replacement}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1264}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	clientID := sequenceID(1265)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	turnID, messageID := sequenceID(1266), sequenceID(1267)
	old.mu.Lock()
	old.submitGate = make(chan struct{})
	old.submitEvents = []provider.Event{
		provider.NewUserMessageEvent(turnID, messageID, provider.TextMessage("active"), testTime()),
		provider.NewTerminalFailureEvent(turnID, provider.NewProviderError(provider.ErrorProtocolFailure)),
	}
	old.closeEventsOnSubmit = true
	submitGate := old.submitGate
	old.mu.Unlock()
	closureObserved, releaseClosure := make(chan struct{}), make(chan struct{})
	connection.actor.afterProviderEventsClose = func() {
		close(closureObserved)
		<-releaseClosure
	}
	answer := make(chan commandResponse, 1)
	commandID := sequenceID(1268)
	go func() {
		event, commandErr := connection.Command(context.Background(), submitCommand(commandID, clientID, connection.ConversationID(), turnID, messageID, "active", &page))
		answer <- commandResponse{event: event, err: commandErr}
	}()
	receiveLifecycle(t, old.submitted)
	<-closureObserved
	close(submitGate)
	close(releaseClosure)
	result := receiveLifecycle(t, answer)
	require.NoError(t, result.err)
	requireCommandResult(t, result.event, protocol.CommandSucceeded, "")

	ready := false
	providerCrashed := false
	for !ready {
		event := receiveLifecycle(t, connection.Events())
		if payload, ok := event.Payload.(protocol.ErrorPayload); ok && payload.Error.Code() == protocol.ErrorProviderCrashed {
			providerCrashed = true
		}
		if payload, ok := event.Payload.(protocol.LifecyclePayload); ok && payload.State == protocol.LifecycleReady {
			ready = true
		}
	}
	require.False(t, providerCrashed, "accepted terminal failure was followed by a duplicate crash error")
	require.EqualValues(t, 1, old.submissions.Load())
	require.Zero(t, replacement.submissions.Load())
	driver.mu.Lock()
	require.Len(t, driver.requests, 2)
	driver.mu.Unlock()
	state.mu.Lock()
	require.NotNil(t, state.mapping.Current.Committed)
	require.Nil(t, state.mapping.Current.PreparedCommit)
	state.mu.Unlock()
}

func TestRecoveryPreservesConcurrentInteractionResponseOwnership(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1270))
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	mapping.Current.Observed = &statepkg.Revision{Digest: page.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	state := &repairState{mapping: &mapping, promotions: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}}}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	replacement := newTurnSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, replacement}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1280}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	clientID := sequenceID(1281)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	turnID := sequenceID(1282)
	_, err = connection.Command(context.Background(), submitCommand(sequenceID(1283), clientID, connection.ConversationID(), turnID, sequenceID(1284), "active", &page))
	require.NoError(t, err)
	receiveLifecycle(t, old.submitted)
	drainEvents(t, connection.Events(), 3)
	requestID := sequenceID(1285)
	options := []provider.InteractionOption{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}}
	old.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Approve", Options: options})
	require.Equal(t, protocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)
	old.respondGate = make(chan struct{})
	old.respondIgnoreContext = true
	oldAnswer := make(chan commandResponse, 1)
	oldCommandID := sequenceID(1286)
	go func() {
		event, commandErr := connection.Command(context.Background(), interactionResponseCommand(oldCommandID, clientID, connection.ConversationID(), requestID, protocol.InteractionCommandApproval, "accept"))
		oldAnswer <- commandResponse{event: event, err: commandErr}
	}()
	receiveLifecycle(t, old.responded)
	close(old.events)
	resolved := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventInteractionResolved, resolved.Type)
	require.Equal(t, "accept", resolved.Payload.(protocol.InteractionResolvedPayload).OptionID)
	for ready := false; !ready; {
		event := receiveLifecycle(t, connection.Events())
		if payload, ok := event.Payload.(protocol.LifecyclePayload); ok && payload.State == protocol.LifecycleReady {
			ready = true
		}
	}
	replacement.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve again", Options: options})
	require.Equal(t, protocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)
	replacement.respondGate = make(chan struct{})
	replacementAnswer := make(chan commandResponse, 1)
	replacementCommandID := sequenceID(1287)
	go func() {
		event, commandErr := connection.Command(context.Background(), interactionResponseCommand(replacementCommandID, clientID, connection.ConversationID(), requestID, protocol.InteractionCommandApproval, "decline"))
		replacementAnswer <- commandResponse{event: event, err: commandErr}
	}()
	receiveLifecycle(t, replacement.responded)
	oldResponse := receiveLifecycle(t, oldAnswer)
	require.NoError(t, oldResponse.err)
	requireCommandResult(t, oldResponse.event, protocol.CommandRejected, protocol.ErrorProviderCrashed)
	close(replacement.respondGate)
	replacementResponse := receiveLifecycle(t, replacementAnswer)
	require.NoError(t, replacementResponse.err)
	requireCommandResult(t, replacementResponse.event, protocol.CommandSucceeded, "")
	var newResolved protocol.Event
	for newResolved.Type != protocol.EventInteractionResolved {
		newResolved = receiveLifecycle(t, connection.Events())
	}
	require.Equal(t, "decline", newResolved.Payload.(protocol.InteractionResolvedPayload).OptionID)
	require.Equal(t, 2, replayInteractionResolutionCount(t, connection.actor.replay, requestID))
	replayed, err := connection.actor.replay.Replay(clientID, "")
	require.NoError(t, err)
	commandCounts := map[string]int{}
	for _, event := range replayed {
		if payload, ok := event.Payload.(protocol.CommandResultPayload); ok {
			commandCounts[payload.CommandID]++
		}
	}
	require.Equal(t, 1, commandCounts[oldCommandID])
	require.Equal(t, 1, commandCounts[replacementCommandID])
	require.Empty(t, connection.actor.pendingInteractions)
	require.EqualValues(t, 2, connection.actor.nextInteractionToken)
	close(old.respondGate)
}

func TestRecoveryInteractionCallBudgetCapsNonCooperativeCallsAcrossGenerations(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1292))
	state := &hardeningState{mapping: &mapping}
	sessions := []*turnSession{
		newTurnSession(mapping.Current.NativeSession.Value()),
		newTurnSession(mapping.Current.NativeSession.Value()),
		newTurnSession(mapping.Current.NativeSession.Value()),
	}
	driver := &recoverySequenceDriver{sessions: []provider.Session{sessions[0], sessions[1], sessions[2]}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 70000}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	clientID := sequenceID(70001)
	connected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(clientID, identity.CapabilityID))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	connection.actor.interactionCallBudget = make(chan struct{}, 2)
	gates := make([]chan struct{}, 0, 2)
	for generation := 0; generation < 2; generation++ {
		session := sessions[generation]
		requestID := sequenceID(uint64(70010 + generation))
		session.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve", Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}}})
		require.Equal(t, protocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)
		gate := make(chan struct{})
		gates = append(gates, gate)
		session.mu.Lock()
		session.respondGate = gate
		session.respondIgnoreContext = true
		session.mu.Unlock()
		answer := make(chan commandResponse, 1)
		commandID := sequenceID(uint64(70020 + generation))
		go func() {
			event, commandErr := connection.Command(context.Background(), interactionResponseCommand(commandID, clientID, connection.ConversationID(), requestID, protocol.InteractionCommandApproval, "accept"))
			answer <- commandResponse{event: event, err: commandErr}
		}()
		receiveLifecycle(t, session.responded)
		close(session.events)
		settled := receiveLifecycle(t, answer)
		require.NoError(t, settled.err)
		requireCommandResult(t, settled.event, protocol.CommandRejected, protocol.ErrorProviderCrashed)
		ready := false
		resolved := 0
		for !ready {
			event := receiveLifecycle(t, connection.Events())
			if event.Type == protocol.EventInteractionResolved {
				resolved++
			}
			if payload, ok := event.Payload.(protocol.LifecyclePayload); ok && payload.State == protocol.LifecycleReady {
				ready = true
			}
		}
		require.Equal(t, 1, resolved)
		require.Equal(t, generation+1, len(connection.actor.interactionCallBudget))
		require.Empty(t, connection.actor.pendingInteractions)
	}

	requestID := sequenceID(70030)
	third := sessions[2]
	third.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve", Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}}})
	require.Equal(t, protocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)
	commandID := sequenceID(70031)
	result, err := connection.Command(context.Background(), interactionResponseCommand(commandID, clientID, connection.ConversationID(), requestID, protocol.InteractionCommandApproval, "accept"))
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorInvalidState)
	var resolved protocol.Event
	for resolved.Type != protocol.EventInteractionResolved {
		resolved = receiveLifecycle(t, connection.Events())
	}
	require.Equal(t, "accept", resolved.Payload.(protocol.InteractionResolvedPayload).OptionID)
	select {
	case unexpected := <-third.responded:
		t.Fatalf("budget-exhausted response spawned provider call: %#v", unexpected)
	default:
	}
	require.Empty(t, connection.actor.pendingInteractions)
	require.EqualValues(t, 3, connection.actor.nextInteractionToken)
	require.Equal(t, 2, len(connection.actor.interactionCallBudget))
	require.LessOrEqual(t, len(connection.actor.commands.entries), maxCommandLedgerEntries)
	for _, gate := range gates {
		close(gate)
	}
	require.Eventually(t, func() bool { return len(connection.actor.interactionCallBudget) == 0 }, lifecycleTestTimeout, time.Millisecond)
}

func TestRecoveryRetiresNeverReturningInteractionOwnershipBoundedly(t *testing.T) {
	conversationID := sequenceID(1291)
	factory, err := NewEventFactory(conversationID, &lockedIDs{next: 60000}, testClock{now: testTime()})
	require.NoError(t, err)
	actor := &conversation{
		factory: factory, replay: NewReplayLog(), commands: newCommandLedger(),
		pendingInteractions: make(map[string]*pendingInteraction),
	}
	for index := 0; index < maxCommandLedgerEntries+8; index++ {
		requestID := sequenceID(uint64(61000 + index))
		clientID := sequenceID(uint64(63000 + index))
		commandID := sequenceID(uint64(65000 + index))
		request := provider.InteractionRequest{ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve", Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}}}
		require.NoError(t, actor.rememberInteraction(request))
		pending := actor.pendingInteractions[requestID]
		response := provider.InteractionResponse{RequestID: requestID, Kind: request.Kind, OptionID: "accept"}
		pending.resolving = true
		pending.response = &response
		pending.commandID = commandID
		pending.clientID = clientID
		_, cancel := context.WithCancel(context.Background())
		pending.cancel = cancel
		command := interactionResponseCommand(commandID, clientID, conversationID, requestID, protocol.InteractionCommandApproval, "accept")
		disposition, _, beginErr := actor.commands.begin(command)
		require.NoError(t, beginErr)
		require.Equal(t, commandNew, disposition)
		waiter := make(chan commandResponse, 1)
		require.NoError(t, actor.commands.wait(commandID, commandWaiter{response: waiter}))
		actor.expireTerminatedSessionInteractions(map[*clientAttachment]struct{}{})
		settled := receiveLifecycle(t, waiter)
		require.NoError(t, settled.err)
		requireCommandResult(t, settled.event, protocol.CommandRejected, protocol.ErrorProviderCrashed)
		require.Empty(t, actor.pendingInteractions)
		require.Zero(t, actor.pendingInteractionBytes)
	}
	require.EqualValues(t, maxCommandLedgerEntries+8, actor.nextInteractionToken)
	require.LessOrEqual(t, len(actor.commands.entries), maxCommandLedgerEntries)
}

func replayInteractionResolutionCount(t *testing.T, replay *ReplayLog, requestID string) int {
	t.Helper()
	events, err := replay.Replay(sequenceID(1290), "")
	require.NoError(t, err)
	count := 0
	for _, event := range events {
		if payload, ok := event.Payload.(protocol.InteractionResolvedPayload); ok && payload.RequestID == requestID {
			count++
		}
	}
	return count
}

func TestServingGenerationRecoveryDeduplicatesFailureAndRecoversLaterGeneration(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1300))
	state := &hardeningState{mapping: &mapping}
	firstInner := newHardeningSession(mapping.Current.NativeSession.Value())
	secondInner := newHardeningSession(mapping.Current.NativeSession.Value())
	thirdInner := newHardeningSession(mapping.Current.NativeSession.Value())
	first := &recoveryCountingSession{Session: firstInner}
	second := &recoveryCountingSession{Session: secondInner}
	third := &recoveryCountingSession{Session: thirdInner}
	driver := &recoverySequenceDriver{sessions: []provider.Session{first, second, third}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1310}))
	require.NoError(t, err)
	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1311), identity.CapabilityID))
	require.NoError(t, err)
	require.Equal(t, protocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)

	firstInner.events <- provider.NewTerminalFailureEvent("", provider.NewProviderError(provider.ErrorChildExited))
	close(firstInner.events)
	requireRecoveryCycle(t, connection.Events())

	close(secondInner.events)
	requireRecoveryCycle(t, connection.Events())

	driver.mu.Lock()
	require.Len(t, driver.requests, 3)
	for _, request := range driver.requests {
		require.Equal(t, provider.NamePi, request.Provider)
		require.Equal(t, provider.AccessConfigured, request.Access)
		require.Equal(t, mapping.Current.NativeSession, request.NativeSession)
		require.Equal(t, "/tmp/agent-whiteboard-test/"+mapping.Current.ConversationID, request.Workspace)
	}
	require.Zero(t, driver.deletes)
	driver.mu.Unlock()
	require.EqualValues(t, 1, firstInner.shutdowns.Load())
	require.EqualValues(t, 1, secondInner.shutdowns.Load())
	for _, session := range []*recoveryCountingSession{first, second, third} {
		require.EqualValues(t, 1, session.nativeCalls.Load())
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
	}
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 1, thirdInner.shutdowns.Load())
	state.mu.Lock()
	require.Zero(t, state.removeCalls)
	state.mu.Unlock()
}

func requireRecoveryCycle(t *testing.T, events <-chan protocol.Event) {
	t.Helper()
	failure := receiveLifecycle(t, events)
	require.Equal(t, protocol.EventError, failure.Type)
	require.Equal(t, protocol.ErrorProviderCrashed, failure.Payload.(protocol.ErrorPayload).Error.Code())
	unavailable := receiveLifecycle(t, events)
	require.Equal(t, protocol.EventLifecycle, unavailable.Type)
	require.Equal(t, protocol.LifecycleUnavailable, unavailable.Payload.(protocol.LifecyclePayload).State)
	ready := receiveLifecycle(t, events)
	require.Equal(t, protocol.EventLifecycle, ready.Type)
	require.Equal(t, protocol.LifecycleReady, ready.Payload.(protocol.LifecyclePayload).State)
}

func TestRecoveryStopsMalformedCandidateAndFailsClosed(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1315))
	state := &hardeningState{mapping: &mapping}
	old := newHardeningSession(mapping.Current.NativeSession.Value())
	malformed := newHardeningSession("sessions/different")
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, malformed}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1316}))
	require.NoError(t, err)
	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1317), identity.CapabilityID))
	require.NoError(t, err)
	receiveLifecycle(t, connection.Events())
	close(old.events)
	require.Equal(t, protocol.ErrorProviderCrashed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, protocol.ErrorProviderRecoveryFailed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.EqualValues(t, 1, malformed.shutdowns.Load())
	require.NoError(t, broker.Close(context.Background()))
}

func TestRecoveryNeverReplaysActiveTurnAndPreservesQueuedFollowUpOnFailure(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1320))
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	observed := statepkg.Revision{Digest: page.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Observed = &observed
	state := &repairState{mapping: &mapping, promotions: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}}}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	partial := newHardeningSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{
		sessions: []provider.Session{old, partial},
		errors:   []error{nil, errors.New("resume failed")},
	}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1330}))
	require.NoError(t, err)
	clientID := sequenceID(1331)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	require.Equal(t, protocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)

	activeTurnID := sequenceID(1332)
	active := submitCommand(sequenceID(1333), clientID, connection.ConversationID(), activeTurnID, sequenceID(1334), "active", &page)
	_, err = connection.Command(context.Background(), active)
	require.NoError(t, err)
	require.Equal(t, activeTurnID, receiveLifecycle(t, old.submitted).TurnID)
	drainEvents(t, connection.Events(), 3)
	queuedTurnID := sequenceID(1335)
	queuedMessageID := sequenceID(1336)
	queued := submitCommand(sequenceID(1337), clientID, connection.ConversationID(), queuedTurnID, queuedMessageID, "queued", nil)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	close(old.events)
	failure := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.ErrorProviderCrashed, failure.Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.EventInterruption, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	recoveryFailure := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.ErrorProviderRecoveryFailed, recoveryFailure.Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.EqualValues(t, 1, old.submissions.Load())
	require.EqualValues(t, 1, partial.shutdowns.Load())

	reconnected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1338), identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	snapshot := receiveLifecycle(t, reconnected.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, protocol.LifecycleUnavailable, snapshot.Lifecycle)
	require.Nil(t, snapshot.ActiveWork)
	require.Equal(t, []protocol.QueueItem{{TurnID: queuedTurnID, MessageID: queuedMessageID, Content: protocol.TextContent("queued")}}, snapshot.Queue)
	require.NoError(t, broker.Close(context.Background()))
	driver.mu.Lock()
	require.Len(t, driver.requests, 2)
	require.Zero(t, driver.deletes)
	driver.mu.Unlock()
}

func TestRecoveryReconcilesPreparedWithoutSubmitAndFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		state     provider.TurnState
		err       error
		recovered bool
		accepted  bool
		mutation  *repairMutation
	}{
		{name: "not accepted", state: provider.TurnNotAccepted, recovered: true},
		{name: "accepted", state: provider.TurnAccepted, recovered: true, accepted: true},
		{name: "running", state: provider.TurnRunning, recovered: true, accepted: true},
		{name: "completed", state: provider.TurnCompleted, recovered: true, accepted: true},
		{name: "interrupted", state: provider.TurnInterrupted, recovered: true, accepted: true},
		{name: "unknown", state: provider.TurnUnknown},
		{name: "invalid", state: provider.TurnState("native")},
		{name: "error", state: provider.TurnAccepted, err: errors.New("native failure")},
		{name: "durable ambiguity", state: provider.TurnAccepted, mutation: &repairMutation{outcome: statepkg.CommitUncertain, err: errors.New("uncertain")}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := uint64(1400 + index*20)
			identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(base))
			resource := testResource(identity.CapabilityID)
			page := testPageContext(resource)
			observed := statepkg.Revision{Digest: page.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
			mapping.Current.Observed = &observed
			mutation := repairMutation{outcome: statepkg.CommitApplied, apply: true}
			if test.mutation != nil {
				mutation = *test.mutation
			}
			state := &repairState{mapping: &mapping, reconciliations: []repairMutation{mutation}}
			old := newTurnSession(mapping.Current.NativeSession.Value())
			old.submitGate = make(chan struct{})
			var release sync.Once
			old.shutdownFunc = func(context.Context) error {
				release.Do(func() { close(old.submitGate) })
				return nil
			}
			candidate := newTurnSession(mapping.Current.NativeSession.Value())
			candidate.reconcileState = test.state
			candidate.reconcileErr = test.err
			var orderingViolation atomic.Bool
			driver := &recoverySequenceDriver{sessions: []provider.Session{old, candidate}}
			driver.resumeHook = func(_ context.Context, call int, _ provider.ResumeRequest) (provider.Session, error) {
				if call == 1 {
					if old.shutdowns.Load() == 0 || old.providerCalls.Load() != 0 {
						orderingViolation.Store(true)
					}
				}
				return driver.sessions[call], nil
			}
			broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: base + 10}))
			require.NoError(t, err)
			clientID := sequenceID(base + 1)
			connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
			require.NoError(t, err)
			connection := connected.(*Connection)
			receiveLifecycle(t, connection.Events())

			turnID := sequenceID(base + 2)
			command := submitCommand(sequenceID(base+3), clientID, connection.ConversationID(), turnID, sequenceID(base+4), "active", &page)
			commandDone := make(chan error, 1)
			go func() {
				_, commandErr := connection.Command(context.Background(), command)
				commandDone <- commandErr
			}()
			require.Equal(t, turnID, receiveLifecycle(t, old.submitted).TurnID)
			close(old.events)
			require.Equal(t, protocol.EventError, receiveLifecycle(t, connection.Events()).Type)
			require.Equal(t, protocol.EventInterruption, receiveLifecycle(t, connection.Events()).Type)
			require.Equal(t, protocol.EventCommandResult, receiveLifecycle(t, connection.Events()).Type)
			require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
			require.NoError(t, receiveLifecycle(t, commandDone))

			result := receiveLifecycle(t, connection.Events())
			if test.recovered {
				require.Equal(t, protocol.EventLifecycle, result.Type)
				require.Equal(t, protocol.LifecycleReady, result.Payload.(protocol.LifecyclePayload).State)
			} else {
				require.Equal(t, protocol.ErrorProviderRecoveryFailed, result.Payload.(protocol.ErrorPayload).Error.Code())
				require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
				require.EqualValues(t, 1, candidate.shutdowns.Load())
			}
			require.False(t, orderingViolation.Load())
			require.EqualValues(t, 1, candidate.reconciliations.Load())
			require.Zero(t, candidate.submissions.Load())
			state.mu.Lock()
			if test.recovered || test.mutation != nil {
				require.Equal(t, 1, state.reconcileCalls)
			} else {
				require.Zero(t, state.reconcileCalls)
			}
			if test.recovered && test.accepted {
				require.Nil(t, state.mapping.Current.PreparedCommit)
				require.Nil(t, state.mapping.Current.Observed)
				require.NotNil(t, state.mapping.Current.Committed)
			} else if test.recovered {
				require.Nil(t, state.mapping.Current.PreparedCommit)
				require.NotNil(t, state.mapping.Current.Observed)
			} else {
				require.NotNil(t, state.mapping.Current.PreparedCommit)
			}
			state.mu.Unlock()
			require.NoError(t, broker.Close(context.Background()))
		})
	}
}

func TestRecoveryCompletesBlockedHistoryBeforeOldShutdownSettles(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1580))
	state := &hardeningState{mapping: &mapping}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	old.historyEntered = make(chan struct{}, 1)
	old.historyGate = make(chan struct{})
	old.shutdownFunc = func(context.Context) error {
		close(old.historyGate)
		return nil
	}
	candidate := newHardeningSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, candidate}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1581}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	clientID := sequenceID(1582)
	connected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(clientID, identity.CapabilityID))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	command := historyCommand(sequenceID(1583), clientID, connection.ConversationID(), protocol.PageRequestPayload{Limit: 1})
	type commandOutcome struct {
		event protocol.Event
		err   error
	}
	commandDone := make(chan commandOutcome, 1)
	go func() {
		result, commandErr := connection.Command(context.Background(), command)
		commandDone <- commandOutcome{event: result, err: commandErr}
	}()
	receiveLifecycle(t, old.historyEntered)
	close(old.events)
	require.Equal(t, protocol.ErrorProviderCrashed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	result := receiveLifecycle(t, connection.Events())
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorProviderCrashed)
	outcome := receiveLifecycle(t, commandDone)
	require.NoError(t, outcome.err)
	require.Equal(t, result, outcome.event)
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	select {
	case duplicate := <-connection.Events():
		if duplicate.Type == protocol.EventCommandResult && duplicate.Payload.(protocol.CommandResultPayload).CommandID == command.CommandID {
			t.Fatal("history command completed twice")
		}
	default:
	}
}

func TestRecoveryCompletesBlockedInterruptBeforeOldShutdownSettles(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1585))
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	observed := statepkg.Revision{Digest: page.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Observed = &observed
	state := &repairState{mapping: &mapping, promotions: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}}}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	old.interruptGate = make(chan struct{})
	old.shutdownFunc = func(context.Context) error {
		close(old.interruptGate)
		return nil
	}
	candidate := newTurnSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, candidate}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1586}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	clientID := sequenceID(1587)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	turnID := sequenceID(1588)
	submit := submitCommand(sequenceID(1589), clientID, connection.ConversationID(), turnID, sequenceID(1590), "active", &page)
	result, err := connection.Command(context.Background(), submit)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	receiveLifecycle(t, old.submitted)
	drainEvents(t, connection.Events(), 3)
	conversationID := connection.ConversationID()
	interrupt := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(1591), ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandInterrupt, Payload: protocol.WorkReferencePayload{WorkID: turnID}}
	type interruptOutcome struct {
		event protocol.Event
		err   error
	}
	interruptDone := make(chan interruptOutcome, 1)
	go func() {
		result, commandErr := connection.Command(context.Background(), interrupt)
		interruptDone <- interruptOutcome{event: result, err: commandErr}
	}()
	receiveLifecycle(t, old.interrupted)
	stopping := receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload)
	require.NotNil(t, stopping.ActiveWork)
	require.Equal(t, protocol.ActiveWorkStopping, stopping.ActiveWork.State)
	close(old.events)
	require.Equal(t, protocol.ErrorProviderCrashed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	interruptResult := receiveLifecycle(t, connection.Events())
	requireCommandResult(t, interruptResult, protocol.CommandRejected, protocol.ErrorTurnInterrupted)
	outcome := receiveLifecycle(t, interruptDone)
	require.NoError(t, outcome.err)
	require.Equal(t, interruptResult, outcome.event)
	interrupted := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventInterruption, interrupted.Type)
	require.Equal(t, turnID, interrupted.Payload.(protocol.InterruptionPayload).TurnID)
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
}

func TestNewerRevisionAttachedDuringRecoveryErasesStaleQueuedContext(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1620))
	resource1 := testResource(identity.CapabilityID)
	page1 := testPageContext(resource1)
	observed := statepkg.Revision{Digest: page1.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource1.UpdatedAt}
	mapping.Current.Observed = &observed
	state := &repairState{mapping: &mapping, promotions: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}}}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	shutdownEntered := make(chan struct{})
	shutdownGate := make(chan struct{})
	old.shutdownFunc = func(context.Context) error {
		close(shutdownEntered)
		<-shutdownGate
		return nil
	}
	candidate := newTurnSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, candidate}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1630}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	clientID := sequenceID(1631)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page1.Digest, resource1, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	activeTurnID := sequenceID(1632)
	active := submitCommand(sequenceID(1633), clientID, connection.ConversationID(), activeTurnID, sequenceID(1634), "active", &page1)
	_, err = connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, old.submitted)
	drainEvents(t, connection.Events(), 3)

	resource2 := resource1
	resource2.UpdatedAt = resource1.UpdatedAt.Add(time.Minute)
	page2 := testPageContext(resource2)
	page2.Revision = protocol.ContextReplacement
	page2.Source = "# replacement two\n"
	page2.Digest = agent.CalculateContextDigest([]byte(page2.Source), []byte(page2.CreatorContext))
	secondClientID := sequenceID(1635)
	second, err := broker.Connect(context.Background(), identity.Origin, observationConnect(secondClientID, identity.CapabilityID, page2.Digest, resource2, ""))
	require.NoError(t, err)
	defer second.Close(context.Background())
	require.Equal(t, protocol.EventContext, receiveLifecycle(t, connection.Events()).Type)
	receiveLifecycle(t, second.Events())
	queuedTurnID := sequenceID(1636)
	queuedMessageID := sequenceID(1637)
	queued := submitCommand(sequenceID(1638), clientID, connection.ConversationID(), queuedTurnID, queuedMessageID, "queued", &page2)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	close(old.events)
	require.Equal(t, protocol.ErrorProviderCrashed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.EventInterruption, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	receiveLifecycle(t, shutdownEntered)
	resource3 := resource2
	resource3.UpdatedAt = resource2.UpdatedAt.Add(time.Minute)
	page3 := testPageContext(resource3)
	page3.Revision = protocol.ContextReplacement
	page3.Source = "# replacement three\n"
	page3.Digest = agent.CalculateContextDigest([]byte(page3.Source), []byte(page3.CreatorContext))
	thirdClientID := sequenceID(1639)
	third, err := broker.Connect(context.Background(), identity.Origin, observationConnect(thirdClientID, identity.CapabilityID, page3.Digest, resource3, ""))
	require.NoError(t, err)
	defer third.Close(context.Background())
	snapshot := receiveLifecycle(t, third.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, protocol.LifecycleUnavailable, snapshot.Lifecycle)
	close(shutdownGate)
	require.Equal(t, protocol.EventContext, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, protocol.EventContext, receiveLifecycle(t, third.Events()).Type)
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, third.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Zero(t, candidate.submissions.Load(), "queued turn must wait for fresh context instead of using stale queued page bytes")
	fourth, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1640), identity.CapabilityID, page3.Digest, resource3, ""))
	require.NoError(t, err)
	defer fourth.Close(context.Background())
	resynced := receiveLifecycle(t, fourth.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, []protocol.QueueItem{{TurnID: queuedTurnID, MessageID: queuedMessageID, Content: protocol.TextContent("queued")}}, resynced.Queue)
	require.Equal(t, protocol.ContextPending, resynced.ContextState)
}

func TestRecoveryPromotesCommitAcceptedWithExactRetryClassification(t *testing.T) {
	identity, mapping, revision := preparedRepairMapping(t, statepkg.CommitAccepted)
	state := &repairState{mapping: &mapping, promotions: []repairMutation{
		{outcome: statepkg.CommitNotApplied, err: errors.New("not applied")},
		{outcome: statepkg.CommitApplied, apply: true},
	}}
	old := newHardeningSession(mapping.Current.NativeSession.Value())
	candidate := newHardeningSession(mapping.Current.NativeSession.Value())
	driver := &recoverySequenceDriver{sessions: []provider.Session{candidate}}
	actor := &conversation{
		identity: identity, state: state, driver: driver, clock: testClock{now: testTime()},
		shutdownTimeout: time.Second, retainSession: func(*sessionHandle) { t.Fatal("candidate unexpectedly retained") },
	}
	results := make(chan recoveryWorkerResult, 1)
	actor.runRecovery(context.Background(), 1, mapping, newActorShutdown(captureSession(old), nil), results)
	result := receiveLifecycle(t, results)
	require.NoError(t, result.err)
	require.NotNil(t, result.handle)
	require.Equal(t, revision, *result.mapping.Current.Committed)
	require.Nil(t, result.mapping.Current.PreparedCommit)
	require.Nil(t, result.mapping.Current.Observed)
	state.mu.Lock()
	require.Equal(t, 2, state.promoteCalls)
	state.mu.Unlock()
	require.Zero(t, candidate.reconciliations.Load())
	require.Zero(t, candidate.submissions.Load())
	require.EqualValues(t, 1, old.shutdowns.Load())
}

func TestFailedCandidateCleanupRemainsBrokerOwnedAndRetryable(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1590))
	state := &hardeningState{mapping: &mapping}
	old := newHardeningSession(mapping.Current.NativeSession.Value())
	candidate := newHardeningSession("sessions/different")
	candidate.shutdownErr = errors.New("shutdown failed")
	child := nonCooperativeHardeningChild(1)
	candidate.child = child
	driver := &recoverySequenceDriver{sessions: []provider.Session{old, candidate}}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1591}))
	require.NoError(t, err)
	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1592), identity.CapabilityID))
	require.NoError(t, err)
	receiveLifecycle(t, connection.Events())
	close(old.events)
	require.Equal(t, protocol.ErrorProviderCrashed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, protocol.ErrorProviderRecoveryFailed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.EqualValues(t, 1, candidate.shutdowns.Load())

	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 2, candidate.shutdowns.Load())
	child.mu.Lock()
	require.Equal(t, 2, operationCount(child.order, "terminate"))
	require.Equal(t, 2, operationCount(child.order, "kill"))
	require.GreaterOrEqual(t, operationCount(child.order, "wait"), 1)
	child.mu.Unlock()
	driver.mu.Lock()
	require.Zero(t, driver.deletes)
	driver.mu.Unlock()
	state.mu.Lock()
	require.Zero(t, state.removeCalls)
	state.mu.Unlock()
}

func TestCloseJoinsPartialSessionReturnedByCanceledRecovery(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1600))
	state := &hardeningState{mapping: &mapping}
	old := newHardeningSession(mapping.Current.NativeSession.Value())
	partial := newHardeningSession(mapping.Current.NativeSession.Value())
	resumeEntered := make(chan struct{})
	var enteredOnce sync.Once
	driver := &recoverySequenceDriver{sessions: []provider.Session{old}}
	driver.resumeHook = func(ctx context.Context, call int, _ provider.ResumeRequest) (provider.Session, error) {
		if call == 0 {
			return old, nil
		}
		enteredOnce.Do(func() { close(resumeEntered) })
		<-ctx.Done()
		return partial, ctx.Err()
	}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1610}))
	require.NoError(t, err)
	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1611), identity.CapabilityID))
	require.NoError(t, err)
	receiveLifecycle(t, connection.Events())
	close(old.events)
	waitLifecycle(t, resumeEntered)
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 1, old.shutdowns.Load())
	require.EqualValues(t, 1, partial.shutdowns.Load())
	driver.mu.Lock()
	require.Len(t, driver.requests, 2)
	require.Zero(t, driver.deletes)
	driver.mu.Unlock()
	state.mu.Lock()
	require.Zero(t, state.removeCalls)
	state.mu.Unlock()
}

var _ provider.Driver = (*recoverySequenceDriver)(nil)
var _ provider.Session = (*recoveryCountingSession)(nil)
