package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func newCommand(commandID, clientID, conversationID string) agentprotocol.Command {
	return agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion, CommandID: commandID, ClientID: clientID,
		ConversationID: &conversationID, Type: agentprotocol.CommandNew, Payload: agentprotocol.EmptyPayload{},
	}
}

func restoreCommand(commandID, clientID, conversationID, archiveID string) agentprotocol.Command {
	return agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion, CommandID: commandID, ClientID: clientID,
		ConversationID: &conversationID, Type: agentprotocol.CommandArchiveRestore,
		Payload: agentprotocol.ArchiveReferencePayload{ArchiveID: archiveID},
	}
}

func TestCommandNewHandsOffTwoTabsAndPublishesExactDurableCurrent(t *testing.T) {
	broker, state, driver, first, identity := archiveFixture(t, 2100)
	defer broker.Close(context.Background())
	secondClient := sequenceID(2111)
	secondRaw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(secondClient, identity.CapabilityID))
	require.NoError(t, err)
	second := secondRaw.(*Connection)
	receiveLifecycle(t, second.Events())

	candidate := newTurnSession("sessions/new-handoff")
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.mu.Unlock()
	oldID := first.ConversationID()
	result, err := first.Command(context.Background(), newCommand(sequenceID(2112), first.clientID, oldID))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")

	firstArchive := receiveLifecycle(t, first.Events())
	require.Equal(t, oldID, firstArchive.ConversationID)
	require.Equal(t, agentprotocol.ArchivePayload{Action: agentprotocol.ArchiveCreated, ArchiveID: oldID}, firstArchive.Payload)
	require.Equal(t, result, receiveLifecycle(t, first.Events()))
	_, firstOpen := receiveLifecycleOpen(t, first.Events())
	require.False(t, firstOpen)
	secondArchive := receiveLifecycle(t, second.Events())
	require.Equal(t, firstArchive.EventID, secondArchive.EventID)
	_, secondOpen := receiveLifecycleOpen(t, second.Events())
	require.False(t, secondOpen)

	state.repairState.mu.Lock()
	mapping := cloneMapping(*state.mapping)
	state.repairState.mu.Unlock()
	require.NotEqual(t, oldID, mapping.Current.ConversationID)
	require.Equal(t, oldID, mapping.Archives[len(mapping.Archives)-1].ConversationID)
	state.muArchive.Lock()
	require.Equal(t, 1, state.newCalls)
	require.Equal(t, *mapping.Current, state.newSessions[0])
	state.muArchive.Unlock()
	driver.mu.Lock()
	require.Equal(t, provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Workspace: "/tmp/agent-whiteboard-test/" + mapping.Current.ConversationID}, driver.createRequests[0])
	driver.mu.Unlock()

	reconnectedRaw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(2113), identity.CapabilityID))
	require.NoError(t, err)
	reconnected := reconnectedRaw.(*Connection)
	defer reconnected.Close(context.Background())
	snapshot := receiveLifecycle(t, reconnected.Events())
	require.Equal(t, mapping.Current.ConversationID, reconnected.ConversationID())
	require.Equal(t, mapping.Current.ConversationID, snapshot.ConversationID)
	require.EqualValues(t, 1, driver.session.(*turnSession).shutdowns.Load())
}

func TestHandoffForciblyDetachesUnreadAttachmentAfterBound(t *testing.T) {
	broker, _, driver, first, identity := archiveFixture(t, 2120)
	defer broker.Close(context.Background())
	first.actor.shutdownTimeout = 20 * time.Millisecond
	secondRaw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(2121), identity.CapabilityID))
	require.NoError(t, err)
	second := secondRaw.(*Connection)
	receiveLifecycle(t, second.Events())
	candidate := newTurnSession("sessions/new-forced-detach")
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.mu.Unlock()
	result, err := first.Command(context.Background(), newCommand(sequenceID(2122), first.clientID, first.ConversationID()))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	receiveLifecycle(t, first.Events())
	receiveLifecycle(t, first.Events())
	select {
	case <-second.attachment.detached:
	case <-time.After(time.Second):
		t.Fatal("unread handoff attachment was not forcibly detached")
	}
}

func TestCommandArchiveRestoreUsesStableWorkspaceWithoutHistoryAndHandsOff(t *testing.T) {
	broker, state, driver, first, identity := archiveFixture(t, 2140)
	defer broker.Close(context.Background())
	secondRaw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(2151), identity.CapabilityID))
	require.NoError(t, err)
	second := secondRaw.(*Connection)
	receiveLifecycle(t, second.Events())

	archiveID := sequenceID(2142)
	candidate := newTurnSession("sessions/archive-two")
	driver.mu.Lock()
	driver.resumeSessions = []provider.Session{nil, candidate}
	driver.mu.Unlock()
	oldID := first.ConversationID()
	result, err := first.Command(context.Background(), restoreCommand(sequenceID(2152), first.clientID, oldID, archiveID))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	archiveEvent := receiveLifecycle(t, first.Events())
	require.Equal(t, agentprotocol.ArchivePayload{Action: agentprotocol.ArchiveRestored, ArchiveID: archiveID}, archiveEvent.Payload)
	require.Equal(t, result, receiveLifecycle(t, first.Events()))
	_, open := receiveLifecycleOpen(t, first.Events())
	require.False(t, open)
	require.Equal(t, archiveEvent.EventID, receiveLifecycle(t, second.Events()).EventID)
	_, open = receiveLifecycleOpen(t, second.Events())
	require.False(t, open)

	state.repairState.mu.Lock()
	mapping := cloneMapping(*state.mapping)
	state.repairState.mu.Unlock()
	require.Equal(t, archiveID, mapping.Current.ConversationID)
	require.Equal(t, oldID, mapping.Archives[len(mapping.Archives)-1].ConversationID)
	state.muArchive.Lock()
	require.Equal(t, []string{archiveID}, state.restoreIDs)
	state.muArchive.Unlock()
	driver.mu.Lock()
	lastResume := driver.resumeRequests[len(driver.resumeRequests)-1]
	driver.mu.Unlock()
	require.Equal(t, provider.ResumeRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, NativeSession: mapping.Current.NativeSession, Workspace: "/tmp/agent-whiteboard-test/" + archiveID}, lastResume)
	require.Zero(t, candidate.historyCalls.Load(), "restore must not request provider history or preview content")

	reconnectedRaw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(2153), identity.CapabilityID))
	require.NoError(t, err)
	reconnected := reconnectedRaw.(*Connection)
	defer reconnected.Close(context.Background())
	require.Equal(t, archiveID, reconnected.ConversationID())
	require.Equal(t, archiveID, receiveLifecycle(t, reconnected.Events()).ConversationID)
}

func TestCommandNewRetriesOnlyAfterExactPreconditionProof(t *testing.T) {
	broker, state, driver, connection, _ := archiveFixture(t, 2160)
	defer broker.Close(context.Background())
	failure := errors.New("durability unavailable")
	state.muArchive.Lock()
	state.newSteps = []repairMutation{
		{outcome: agentstate.CommitUncertain, err: failure},
		{outcome: agentstate.CommitApplied, apply: true},
	}
	state.muArchive.Unlock()
	driver.mu.Lock()
	driver.createSessions = []provider.Session{newTurnSession("sessions/new-retry")}
	driver.mu.Unlock()
	result, err := connection.Command(context.Background(), newCommand(sequenceID(2171), connection.clientID, connection.ConversationID()))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	receiveLifecycle(t, connection.Events())
	receiveLifecycle(t, connection.Events())
	state.muArchive.Lock()
	require.Equal(t, 2, state.newCalls)
	require.Len(t, state.newSessions, 2)
	require.Equal(t, state.newSessions[0], state.newSessions[1], "retry must use the exact candidate session")
	state.muArchive.Unlock()
}

func TestRestoreCommitFailureStopsCandidateWithoutDeletingRestoredState(t *testing.T) {
	broker, state, driver, connection, _ := archiveFixture(t, 2175)
	defer broker.Close(context.Background())
	failure := errors.New("restore commit rejected")
	state.muArchive.Lock()
	state.restoreSteps = []repairMutation{
		{outcome: agentstate.CommitNotApplied, err: failure},
		{outcome: agentstate.CommitNotApplied, err: failure},
	}
	state.muArchive.Unlock()
	archiveID := sequenceID(2177)
	candidate := newTurnSession("sessions/archive-two")
	driver.mu.Lock()
	driver.resumeSessions = []provider.Session{nil, candidate}
	driver.mu.Unlock()
	result, err := connection.Command(context.Background(), restoreCommand(sequenceID(2178), connection.clientID, connection.ConversationID(), archiveID))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorStateRepairFailed)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	_, open := receiveLifecycleOpen(t, connection.Events())
	require.False(t, open)
	require.EqualValues(t, 1, candidate.shutdowns.Load())
	driver.mu.Lock()
	require.Empty(t, driver.deletes, "restore failures must never delete native archive state")
	driver.mu.Unlock()
	state.muArchive.Lock()
	require.Equal(t, 2, state.restoreCalls)
	require.NotContains(t, state.operations, "workspace", "restore failures must never remove the stable workspace")
	state.muArchive.Unlock()
	state.repairState.mu.Lock()
	require.NotEqual(t, archiveID, state.mapping.Current.ConversationID)
	state.repairState.mu.Unlock()
}

func TestCommandNewStopFailureKeepsDurableCurrentAndCleansCandidate(t *testing.T) {
	broker, state, driver, connection, _ := archiveFixture(t, 2180)
	defer broker.Close(context.Background())
	old := driver.session.(*turnSession)
	old.shutdownErr = errors.New("graceful stop failed")
	old.child.(*hardeningChild).killFailures = 1
	candidate := newTurnSession("sessions/new-stop-failure")
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.mu.Unlock()
	oldID := connection.ConversationID()
	result, err := connection.Command(context.Background(), newCommand(sequenceID(2191), connection.clientID, oldID))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorProviderProtocolFailure)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	require.Equal(t, agentprotocol.EventLifecycle, receiveLifecycle(t, connection.Events()).Type)
	state.repairState.mu.Lock()
	require.Equal(t, oldID, state.mapping.Current.ConversationID)
	state.repairState.mu.Unlock()
	state.muArchive.Lock()
	require.Zero(t, state.newCalls, "durable mutation must follow a proven old-session stop")
	require.Contains(t, state.operations, "workspace")
	state.muArchive.Unlock()
	driver.mu.Lock()
	require.Len(t, driver.deletes, 1)
	require.Equal(t, "sessions/new-stop-failure", driver.deletes[0].NativeSession.Value())
	driver.mu.Unlock()
	require.EqualValues(t, 1, candidate.shutdowns.Load())
}

func TestHandoffExactPreconditionRejectsInterveningDurableMapping(t *testing.T) {
	broker, state, driver, connection, _ := archiveFixture(t, 2195)
	defer broker.Close(context.Background())
	old := driver.session.(*turnSession)
	entered := make(chan struct{})
	gate := make(chan struct{})
	old.shutdownFunc = func(context.Context) error {
		close(entered)
		<-gate
		return nil
	}
	candidate := newTurnSession("sessions/new-stale-precondition")
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.mu.Unlock()
	response := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), newCommand(sequenceID(2196), connection.clientID, connection.ConversationID()))
		response <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, entered)
	state.repairState.mu.Lock()
	state.mapping.Current.ModelLabel = "foreign-current"
	state.repairState.mu.Unlock()
	close(gate)
	answer := receiveLifecycle(t, response)
	require.NoError(t, answer.err)
	requireCommandResult(t, answer.event, agentprotocol.CommandRejected, agentprotocol.ErrorStateRepairFailed)
	require.Equal(t, answer.event, receiveLifecycle(t, connection.Events()))
	_, open := receiveLifecycleOpen(t, connection.Events())
	require.False(t, open)
	state.repairState.mu.Lock()
	require.Equal(t, "foreign-current", state.mapping.Current.ModelLabel)
	require.NotEqual(t, "sessions/new-stale-precondition", state.mapping.Current.NativeSession.Value())
	state.repairState.mu.Unlock()
	state.muArchive.Lock()
	require.Zero(t, state.newCalls, "atomic precondition failure must not invoke the mutation body")
	state.muArchive.Unlock()
	driver.mu.Lock()
	require.Empty(t, driver.deletes, "ambiguous ownership must not authorize destructive compensation")
	driver.mu.Unlock()
	require.EqualValues(t, 1, candidate.shutdowns.Load())
}

func TestHandoffRegistryCASDoesNotOverwriteAReplacementSlot(t *testing.T) {
	broker, state, driver, connection, identity := archiveFixture(t, 2200)
	defer broker.Close(context.Background())
	old := driver.session.(*turnSession)
	entered := make(chan struct{})
	gate := make(chan struct{})
	old.shutdownFunc = func(ctx context.Context) error {
		close(entered)
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	candidate := newTurnSession("sessions/new-cas")
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.mu.Unlock()
	response := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), newCommand(sequenceID(2211), connection.clientID, connection.ConversationID()))
		response <- commandResponse{event: event, err: err}
	}()
	<-entered
	foreign := &conversationSlot{ready: make(chan struct{})}
	close(foreign.ready)
	broker.mu.Lock()
	broker.registry[identity] = foreign
	broker.mu.Unlock()
	close(gate)
	answer := <-response
	require.NoError(t, answer.err)
	requireCommandResult(t, answer.event, agentprotocol.CommandRejected, agentprotocol.ErrorStateRepairFailed)
	require.Equal(t, answer.event, receiveLifecycle(t, connection.Events()))
	_, open := receiveLifecycleOpen(t, connection.Events())
	require.False(t, open)
	broker.mu.Lock()
	require.Same(t, foreign, broker.registry[identity])
	broker.mu.Unlock()
	state.repairState.mu.Lock()
	require.Equal(t, "sessions/new-cas", state.mapping.Current.NativeSession.Value(), "the exact durable target remains authoritative")
	state.repairState.mu.Unlock()
	require.EqualValues(t, 1, candidate.shutdowns.Load(), "unregistered replacement actor must be joined")
}

func TestSuccessfulReplacementKeepsDisplacedActorOwnedUntilItStops(t *testing.T) {
	broker, _, driver, connection, identity := archiveFixture(t, 2235)
	old := connection.actor
	candidate := newTurnSession(old.mapping.Current.NativeSession.Value())
	replacement, err := broker.newConversation(identity, cloneMapping(old.mapping), captureSession(candidate))
	require.NoError(t, err)
	broker.mu.Lock()
	slot := broker.registry[identity]
	broker.mu.Unlock()
	require.True(t, broker.installReplacement(slot, old, replacement))
	broker.mu.Lock()
	_, owned := broker.orphans[old]
	broker.mu.Unlock()
	require.True(t, owned, "displaced actor must remain visible to Broker.Close")
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 1, driver.session.(*turnSession).shutdowns.Load())
	require.EqualValues(t, 1, candidate.shutdowns.Load())
	broker.mu.Lock()
	require.Empty(t, broker.orphans)
	broker.mu.Unlock()
}

func TestRestoreReconcilesPreparedArchiveWithoutSubmitting(t *testing.T) {
	archiveID := sequenceID(2242)
	turnID := sequenceID(2243)
	broker, state, driver, connection, _ := archiveFixtureWithMapping(t, 2240, func(mapping *agentstate.Mapping) {
		archived := mapping.Archives[1]
		revision := agentstate.Revision{Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Revision: agentstate.RevisionInitial, SourceUpdatedAt: archived.UpdatedAt}
		archived.Observed = &revision
		archived.PreparedCommit = &agentstate.PreparedCommit{Revision: revision, TurnID: turnID, Phase: agentstate.CommitPrepared}
		mapping.Archives[1] = archived
	})
	defer broker.Close(context.Background())
	state.repairState.mu.Lock()
	state.reconciliations = []repairMutation{{outcome: agentstate.CommitApplied, apply: true}}
	state.repairState.mu.Unlock()
	candidate := newTurnSession("sessions/archive-two")
	candidate.reconcileState = provider.TurnAccepted
	driver.mu.Lock()
	driver.resumeSessions = []provider.Session{nil, candidate}
	driver.mu.Unlock()
	result, err := connection.Command(context.Background(), restoreCommand(sequenceID(2244), connection.clientID, connection.ConversationID(), archiveID))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	receiveLifecycle(t, connection.Events())
	receiveLifecycle(t, connection.Events())
	state.repairState.mu.Lock()
	require.Equal(t, archiveID, state.mapping.Current.ConversationID)
	require.Nil(t, state.mapping.Current.PreparedCommit)
	require.Nil(t, state.mapping.Current.Observed)
	require.NotNil(t, state.mapping.Current.Committed)
	state.repairState.mu.Unlock()
	require.EqualValues(t, 1, candidate.reconciliations.Load())
	require.Zero(t, candidate.submissions.Load())
}

func TestRestorePromotesAcceptedArchiveWithoutSubmitting(t *testing.T) {
	archiveID := sequenceID(2282)
	turnID := sequenceID(2283)
	broker, state, driver, connection, _ := archiveFixtureWithMapping(t, 2280, func(mapping *agentstate.Mapping) {
		archived := mapping.Archives[1]
		revision := agentstate.Revision{Digest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Revision: agentstate.RevisionInitial, SourceUpdatedAt: archived.UpdatedAt}
		archived.Observed = &revision
		archived.PreparedCommit = &agentstate.PreparedCommit{Revision: revision, TurnID: turnID, Phase: agentstate.CommitAccepted}
		mapping.Archives[1] = archived
	})
	defer broker.Close(context.Background())
	state.repairState.mu.Lock()
	state.promotions = []repairMutation{{outcome: agentstate.CommitApplied, apply: true}}
	state.repairState.mu.Unlock()
	candidate := newTurnSession("sessions/archive-two")
	driver.mu.Lock()
	driver.resumeSessions = []provider.Session{nil, candidate}
	driver.mu.Unlock()
	result, err := connection.Command(context.Background(), restoreCommand(sequenceID(2284), connection.clientID, connection.ConversationID(), archiveID))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	receiveLifecycle(t, connection.Events())
	receiveLifecycle(t, connection.Events())
	state.repairState.mu.Lock()
	require.Equal(t, archiveID, state.mapping.Current.ConversationID)
	require.Nil(t, state.mapping.Current.PreparedCommit)
	require.NotNil(t, state.mapping.Current.Committed)
	state.repairState.mu.Unlock()
	require.Zero(t, candidate.reconciliations.Load())
	require.Zero(t, candidate.submissions.Load())
}

func TestRestorePreparedRepairRejectsForeignPostRestoreMapping(t *testing.T) {
	archiveID := sequenceID(2302)
	turnID := sequenceID(2303)
	broker, state, driver, connection, _ := archiveFixtureWithMapping(t, 2300, func(mapping *agentstate.Mapping) {
		archived := mapping.Archives[1]
		revision := agentstate.Revision{Digest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Revision: agentstate.RevisionInitial, SourceUpdatedAt: archived.UpdatedAt}
		archived.Observed = &revision
		archived.PreparedCommit = &agentstate.PreparedCommit{Revision: revision, TurnID: turnID, Phase: agentstate.CommitPrepared}
		mapping.Archives[1] = archived
	})
	defer broker.Close(context.Background())
	candidate := newTurnSession("sessions/archive-two")
	candidate.reconcileState = provider.TurnAccepted
	state.muArchive.Lock()
	state.beforeReconcile = func() {
		state.repairState.mu.Lock()
		state.mapping.Current.ModelLabel = "foreign-after-restore"
		state.repairState.mu.Unlock()
	}
	state.muArchive.Unlock()
	driver.mu.Lock()
	driver.resumeSessions = []provider.Session{nil, candidate}
	driver.mu.Unlock()
	result, err := connection.Command(context.Background(), restoreCommand(sequenceID(2304), connection.clientID, connection.ConversationID(), archiveID))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorStateRepairFailed)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	_, open := receiveLifecycleOpen(t, connection.Events())
	require.False(t, open)
	state.repairState.mu.Lock()
	require.Equal(t, archiveID, state.mapping.Current.ConversationID)
	require.Equal(t, "foreign-after-restore", state.mapping.Current.ModelLabel)
	require.NotNil(t, state.mapping.Current.PreparedCommit)
	state.repairState.mu.Unlock()
	require.EqualValues(t, 1, candidate.reconciliations.Load())
	require.Zero(t, candidate.submissions.Load())
	require.EqualValues(t, 1, candidate.shutdowns.Load())
	driver.mu.Lock()
	require.Empty(t, driver.deletes)
	driver.mu.Unlock()
}

func TestBrokerCloseDeadlineDoesNotBlockOnNoncooperativeCandidateCleanup(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixture(t, 2250)
	broker.shutdownTimeout = 40 * time.Millisecond
	connection.actor.shutdownTimeout = 40 * time.Millisecond
	candidate := newTurnSession("sessions/new-noncooperative-cleanup")
	cleanupEntered := make(chan struct{})
	cleanupGate := make(chan struct{})
	candidate.shutdownFunc = func(context.Context) error {
		close(cleanupEntered)
		<-cleanupGate
		return nil
	}
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.createErr = errors.New("partial create")
	driver.mu.Unlock()
	response := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), newCommand(sequenceID(2251), connection.clientID, connection.ConversationID()))
		response <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, cleanupEntered)
	closeDone := make(chan error, 1)
	go func() { closeDone <- broker.Close(context.Background()) }()
	select {
	case closeErr := <-closeDone:
		require.Error(t, closeErr)
	case <-time.After(time.Second):
		t.Fatal("Broker.Close blocked on cleanup ownership mutex")
	}
	close(cleanupGate)
	answer := receiveLifecycle(t, response)
	require.NoError(t, answer.err)
	requireCommandResult(t, answer.event, agentprotocol.CommandRejected, agentprotocol.ErrorProviderProtocolFailure)
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 1, candidate.shutdowns.Load())
}

func TestBrokerCloseDeadlineDoesNotWaitForNoncooperativeHandoff(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixture(t, 2260)
	broker.shutdownTimeout = 40 * time.Millisecond
	connection.actor.shutdownTimeout = 40 * time.Millisecond
	candidate := newTurnSession("sessions/new-noncooperative-close")
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.createEntered = make(chan struct{}, 1)
	driver.createGate = make(chan struct{})
	driver.createIgnoreContext = true
	entered, gate := driver.createEntered, driver.createGate
	driver.mu.Unlock()
	response := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), newCommand(sequenceID(2271), connection.clientID, connection.ConversationID()))
		response <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, entered)
	closeDone := make(chan error, 1)
	go func() { closeDone <- broker.Close(context.Background()) }()
	select {
	case closeErr := <-closeDone:
		require.Error(t, closeErr)
	case <-time.After(time.Second):
		t.Fatal("Broker.Close ignored its handoff shutdown bound")
	}
	close(gate)
	answer := receiveLifecycle(t, response)
	require.NoError(t, answer.err)
	requireCommandResult(t, answer.event, agentprotocol.CommandRejected, agentprotocol.ErrorProviderProtocolFailure)
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 1, candidate.shutdowns.Load())
}

func TestBrokerCloseJoinsInFlightHandoffAndCandidate(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixture(t, 2220)
	candidate := newTurnSession("sessions/new-close")
	driver.mu.Lock()
	driver.createSessions = []provider.Session{candidate}
	driver.createEntered = make(chan struct{}, 1)
	driver.createGate = make(chan struct{})
	entered := driver.createEntered
	driver.mu.Unlock()
	response := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), newCommand(sequenceID(2231), connection.clientID, connection.ConversationID()))
		response <- commandResponse{event: event, err: err}
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- broker.Close(context.Background()) }()
	require.NoError(t, <-closeDone)
	answer := <-response
	require.NoError(t, answer.err)
	requireCommandResult(t, answer.event, agentprotocol.CommandRejected, agentprotocol.ErrorProviderProtocolFailure)
	require.EqualValues(t, 1, candidate.shutdowns.Load())
	require.EqualValues(t, 1, driver.session.(*turnSession).shutdowns.Load())
	broker.cleanupMu.Lock()
	require.Empty(t, broker.cleanups)
	broker.cleanupMu.Unlock()
	broker.mu.Lock()
	require.Empty(t, broker.orphans)
	broker.mu.Unlock()
}

func TestConversationHandoffRejectsWhileProviderWorkerIsActive(t *testing.T) {
	broker, _, driver, first, identity := archiveFixture(t, 2180)
	defer broker.Close(context.Background())
	secondRaw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(2191), identity.CapabilityID))
	require.NoError(t, err)
	second := secondRaw.(*Connection)
	receiveLifecycle(t, second.Events())

	session := driver.session.(*turnSession)
	session.historyEntered = make(chan struct{})
	session.historyGate = make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		_, _ = first.Command(context.Background(), historyCommand(sequenceID(2192), first.clientID, first.ConversationID(), agentprotocol.PageRequestPayload{Limit: 1}))
	}()
	<-session.historyEntered
	result, err := second.Command(context.Background(), newCommand(sequenceID(2195), second.clientID, second.ConversationID()))
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorInvalidState)
	require.Equal(t, result, receiveLifecycle(t, second.Events()))
	driver.mu.Lock()
	require.Empty(t, driver.createRequests)
	driver.mu.Unlock()
	close(session.historyGate)
	<-workerDone
}
