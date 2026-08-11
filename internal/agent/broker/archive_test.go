package broker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
	"github.com/stretchr/testify/require"
)

type archiveTestStore struct {
	*repairState
	muArchive             sync.Mutex
	operations            []string
	removeSteps           []repairMutation
	removeCalls           int
	newCalls              int
	restoreCalls          int
	newSessions           []statepkg.Session
	restoreIDs            []string
	newSteps              []repairMutation
	restoreSteps          []repairMutation
	beforePromotePrepared func()
	beforeReconcile       func()
}

func (store *archiveTestStore) RemoveWorkspace(string) error {
	store.muArchive.Lock()
	store.operations = append(store.operations, "workspace")
	store.muArchive.Unlock()
	return nil
}
func (store *archiveTestStore) RemoveSession(_ statepkg.Identity, archiveID string, at time.Time) (statepkg.CommitOutcome, error) {
	store.muArchive.Lock()
	store.operations = append(store.operations, "remove")
	store.removeCalls++
	step := repairMutation{outcome: statepkg.CommitApplied, apply: true}
	if len(store.removeSteps) != 0 {
		step = store.removeSteps[0]
		store.removeSteps = store.removeSteps[1:]
	}
	store.muArchive.Unlock()
	if step.mapping != nil {
		store.repairState.mu.Lock()
		updated := cloneMapping(*step.mapping)
		store.mapping = &updated
		store.repairState.mu.Unlock()
	} else if step.apply {
		store.repairState.mu.Lock()
		updated, ok := removedArchiveMapping(*store.mapping, archiveID, at)
		if ok {
			store.mapping = &updated
		}
		store.repairState.mu.Unlock()
	}
	return step.outcome, step.err
}

func (store *archiveTestStore) NewConversationIfUnchanged(identity statepkg.Identity, expected statepkg.Mapping, current statepkg.Session, at time.Time) (statepkg.CommitOutcome, error) {
	store.repairState.mu.Lock()
	unchanged := store.mapping != nil && store.mapping.Validate(identity) == nil && reflect.DeepEqual(*store.mapping, expected)
	store.repairState.mu.Unlock()
	if !unchanged {
		return statepkg.CommitNotApplied, errors.New("conversation mapping changed")
	}
	store.muArchive.Lock()
	store.operations = append(store.operations, "new")
	store.newCalls++
	store.newSessions = append(store.newSessions, current)
	step := repairMutation{outcome: statepkg.CommitApplied, apply: true}
	if len(store.newSteps) != 0 {
		step = store.newSteps[0]
		store.newSteps = store.newSteps[1:]
	}
	store.muArchive.Unlock()
	if step.apply {
		store.repairState.mu.Lock()
		updated := newCurrentMapping(*store.mapping, current, at)
		store.mapping = &updated
		store.repairState.mu.Unlock()
	}
	return step.outcome, step.err
}
func (store *archiveTestStore) RestoreArchiveIfUnchanged(identity statepkg.Identity, expected statepkg.Mapping, archiveID string, at time.Time) (statepkg.CommitOutcome, error) {
	store.repairState.mu.Lock()
	unchanged := store.mapping != nil && store.mapping.Validate(identity) == nil && reflect.DeepEqual(*store.mapping, expected)
	store.repairState.mu.Unlock()
	if !unchanged {
		return statepkg.CommitNotApplied, errors.New("conversation mapping changed")
	}
	store.muArchive.Lock()
	store.operations = append(store.operations, "restore")
	store.restoreCalls++
	store.restoreIDs = append(store.restoreIDs, archiveID)
	step := repairMutation{outcome: statepkg.CommitApplied, apply: true}
	if len(store.restoreSteps) != 0 {
		step = store.restoreSteps[0]
		store.restoreSteps = store.restoreSteps[1:]
	}
	store.muArchive.Unlock()
	if step.apply {
		store.repairState.mu.Lock()
		updated, ok := restoredCurrentMapping(*store.mapping, archiveID, at)
		if ok {
			store.mapping = &updated
		}
		store.repairState.mu.Unlock()
		if !ok {
			return statepkg.CommitNotApplied, errors.New("archive missing")
		}
	}
	return step.outcome, step.err
}

func (store *archiveTestStore) PromotePreparedIfUnchanged(identity statepkg.Identity, expected statepkg.Mapping, turnID string, at time.Time) (statepkg.CommitOutcome, error) {
	store.muArchive.Lock()
	hook := store.beforePromotePrepared
	store.muArchive.Unlock()
	if hook != nil {
		hook()
	}
	store.repairState.mu.Lock()
	unchanged := store.mapping != nil && store.mapping.Validate(identity) == nil && reflect.DeepEqual(*store.mapping, expected)
	store.repairState.mu.Unlock()
	if !unchanged {
		return statepkg.CommitNotApplied, errors.New("conversation mapping changed")
	}
	return store.repairState.PromotePrepared(identity, turnID, at)
}

func (store *archiveTestStore) ReconcilePreparedIfUnchanged(identity statepkg.Identity, expected statepkg.Mapping, turnID string, accepted bool, at time.Time) (statepkg.CommitOutcome, error) {
	store.muArchive.Lock()
	hook := store.beforeReconcile
	store.muArchive.Unlock()
	if hook != nil {
		hook()
	}
	store.repairState.mu.Lock()
	unchanged := store.mapping != nil && store.mapping.Validate(identity) == nil && reflect.DeepEqual(*store.mapping, expected)
	store.repairState.mu.Unlock()
	if !unchanged {
		return statepkg.CommitNotApplied, errors.New("conversation mapping changed")
	}
	return store.repairState.ReconcilePrepared(identity, turnID, accepted, at)
}

func (store *archiveTestStore) resetOperations() {
	store.muArchive.Lock()
	store.operations = nil
	store.muArchive.Unlock()
}
func (store *archiveTestStore) operationSnapshot() []string {
	store.muArchive.Lock()
	defer store.muArchive.Unlock()
	return append([]string(nil), store.operations...)
}

type archiveTestDriver struct {
	mu                  sync.Mutex
	session             provider.Session
	resumeSessions      []provider.Session
	createSessions      []provider.Session
	createRequests      []provider.CreateRequest
	createErr           error
	resumeRequests      []provider.ResumeRequest
	createEntered       chan struct{}
	createGate          chan struct{}
	createIgnoreContext bool
	deleteErr           error
	deleteEntered       chan struct{}
	deleteGate          chan struct{}
	deletes             []provider.DeleteRequest
	resumes             int
	store               *archiveTestStore
}

func (*archiveTestDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "model"}
}
func (driver *archiveTestDriver) Create(ctx context.Context, request provider.CreateRequest) (provider.Session, error) {
	driver.mu.Lock()
	driver.createRequests = append(driver.createRequests, request)
	entered, gate := driver.createEntered, driver.createGate
	if len(driver.createSessions) == 0 {
		driver.mu.Unlock()
		return nil, errors.New("create is not configured")
	}
	session := driver.createSessions[0]
	driver.createSessions = driver.createSessions[1:]
	createErr := driver.createErr
	driver.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		driver.mu.Lock()
		ignoreContext := driver.createIgnoreContext
		driver.mu.Unlock()
		if ignoreContext {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return session, ctx.Err()
			}
		}
	}
	return session, createErr
}
func (driver *archiveTestDriver) Resume(_ context.Context, request provider.ResumeRequest) (provider.Session, error) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.resumeRequests = append(driver.resumeRequests, request)
	index := driver.resumes
	driver.resumes++
	if index < len(driver.resumeSessions) {
		return driver.resumeSessions[index], nil
	}
	return driver.session, nil
}
func (*archiveTestDriver) Inspect(context.Context, provider.InspectRequest) (provider.NativeSession, error) {
	return provider.NativeSession{}, nil
}
func (driver *archiveTestDriver) Delete(ctx context.Context, request provider.DeleteRequest) error {
	driver.mu.Lock()
	driver.deletes = append(driver.deletes, request)
	err, entered, gate := driver.deleteErr, driver.deleteEntered, driver.deleteGate
	driver.mu.Unlock()
	driver.store.muArchive.Lock()
	driver.store.operations = append(driver.store.operations, "delete")
	driver.store.muArchive.Unlock()
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
			return ctx.Err()
		}
	}
	return err
}

func archiveFixture(t *testing.T, base uint64) (*Broker, *archiveTestStore, *archiveTestDriver, *Connection, statepkg.Identity) {
	return archiveFixtureWithMapping(t, base, nil)
}

func archiveFixtureWithMapping(t *testing.T, base uint64, mutate func(*statepkg.Mapping)) (*Broker, *archiveTestStore, *archiveTestDriver, *Connection, statepkg.Identity) {
	t.Helper()
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(base))
	firstRef, err := statepkg.NativeSessionRef("sessions/archive-one")
	require.NoError(t, err)
	secondRef, err := statepkg.NativeSessionRef("sessions/archive-two")
	require.NoError(t, err)
	mapping.Archives = []statepkg.Session{
		{ConversationID: sequenceID(base + 1), NativeSession: firstRef, CreatedAt: testTime().Add(-4 * time.Hour), UpdatedAt: testTime().Add(-3 * time.Hour), ProviderLabel: "pi", ModelLabel: "older"},
		{ConversationID: sequenceID(base + 2), NativeSession: secondRef, CreatedAt: testTime().Add(-2 * time.Hour), UpdatedAt: testTime().Add(-time.Hour), ProviderLabel: "pi", ModelLabel: "newer"},
	}
	if mutate != nil {
		mutate(&mapping)
	}
	state := &archiveTestStore{repairState: &repairState{mapping: &mapping}}
	session := newTurnSession(mapping.Current.NativeSession.Value())
	driver := &archiveTestDriver{session: session, store: state}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: base + 20}))
	require.NoError(t, err)
	connected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(base+10), identity.CapabilityID))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	state.resetOperations()
	return broker, state, driver, connection, identity
}

func archiveListCommand(commandID, clientID, conversationID, before string, limit int) protocol.Command {
	return protocol.Command{APIVersion: protocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandArchiveList, Payload: protocol.PageRequestPayload{Before: before, Limit: limit}}
}
func archiveDeleteCommand(commandID, clientID, conversationID, archiveID string) protocol.Command {
	return protocol.Command{APIVersion: protocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandArchiveDelete, Payload: protocol.ArchiveReferencePayload{ArchiveID: archiveID}}
}

func TestArchiveListIsTargetedPagedAndContainsNoPreviewContent(t *testing.T) {
	broker, state, driver, first, identity := archiveFixture(t, 1700)
	defer broker.Close(context.Background())
	secondClient := sequenceID(1711)
	secondRaw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(secondClient, identity.CapabilityID))
	require.NoError(t, err)
	second := secondRaw.(*Connection)
	defer second.Close(context.Background())
	receiveLifecycle(t, second.Events())
	conversationID := first.ConversationID()
	clientID := first.clientID
	command := archiveListCommand(sequenceID(1712), clientID, conversationID, "", 1)
	result, err := first.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	history := receiveLifecycle(t, first.Events())
	require.Equal(t, protocol.EventHistory, history.Type)
	page := history.Payload.(protocol.HistoryPayload)
	require.Len(t, page.Items, 1)
	require.Equal(t, sequenceID(1702), page.Items[0].ArchiveID)
	require.Equal(t, "newer", page.Items[0].Model)
	require.Empty(t, page.Items[0].Preview)
	require.NotNil(t, page.NextCursor)
	require.Equal(t, page.Items[0].ArchiveID, *page.NextCursor)
	require.Equal(t, result, receiveLifecycle(t, first.Events()))
	select {
	case <-second.Events():
		t.Fatal("archive page leaked to another client")
	default:
	}
	secondPageCommand := archiveListCommand(sequenceID(1713), clientID, conversationID, *page.NextCursor, 1)
	_, err = first.Command(context.Background(), secondPageCommand)
	require.NoError(t, err)
	secondPage := receiveLifecycle(t, first.Events()).Payload.(protocol.HistoryPayload)
	require.Equal(t, sequenceID(1701), secondPage.Items[0].ArchiveID)
	require.Nil(t, secondPage.NextCursor)
	receiveLifecycle(t, first.Events())
	stale := archiveListCommand(sequenceID(1714), clientID, conversationID, sequenceID(1799), 1)
	staleResult, err := first.Command(context.Background(), stale)
	require.NoError(t, err)
	requireCommandResult(t, staleResult, protocol.CommandRejected, protocol.ErrorStaleReference)
	require.Equal(t, staleResult, receiveLifecycle(t, first.Events()))
	driver.mu.Lock()
	require.Equal(t, 1, driver.resumes, "listing must not resume archives")
	require.Empty(t, driver.deletes)
	require.Zero(t, driver.session.(*turnSession).historyCalls.Load(), "listing must not read provider history")
	driver.mu.Unlock()
	state.muArchive.Lock()
	require.Empty(t, state.operations)
	state.muArchive.Unlock()
}

func TestArchiveDeleteOrdersNativeWorkspaceAndDurableRemoval(t *testing.T) {
	broker, state, driver, connection, _ := archiveFixture(t, 1720)
	defer broker.Close(context.Background())
	archiveID := sequenceID(1722)
	command := archiveDeleteCommand(sequenceID(1731), connection.clientID, connection.ConversationID(), archiveID)
	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	archiveEvent := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.ArchivePayload{Action: protocol.ArchiveDeleted, ArchiveID: archiveID}, archiveEvent.Payload)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	require.Equal(t, []string{"delete", "workspace", "remove"}, state.operationSnapshot())
	state.repairState.mu.Lock()
	require.Len(t, state.mapping.Archives, 1)
	state.repairState.mu.Unlock()
	duplicate, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, result, duplicate)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	driver.mu.Lock()
	require.Len(t, driver.deletes, 1)
	driver.mu.Unlock()
}

func TestArchiveDeleteFailureRetainsArchiveAndRetryIsSafe(t *testing.T) {
	broker, state, driver, connection, _ := archiveFixture(t, 1740)
	defer broker.Close(context.Background())
	archiveID := sequenceID(1742)
	driver.mu.Lock()
	driver.deleteErr = errors.New("native deletion failed /private/session")
	driver.mu.Unlock()
	failed := archiveDeleteCommand(sequenceID(1751), connection.clientID, connection.ConversationID(), archiveID)
	result, err := connection.Command(context.Background(), failed)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorArchiveDeleteRetained)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	require.Equal(t, []string{"delete"}, state.operationSnapshot())
	state.repairState.mu.Lock()
	require.Len(t, state.mapping.Archives, 2)
	state.repairState.mu.Unlock()
	driver.mu.Lock()
	driver.deleteErr = nil
	driver.mu.Unlock()
	state.resetOperations()
	retry := archiveDeleteCommand(sequenceID(1752), connection.clientID, connection.ConversationID(), archiveID)
	result, err = connection.Command(context.Background(), retry)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	require.Equal(t, protocol.EventArchive, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	require.Equal(t, []string{"delete", "workspace", "remove"}, state.operationSnapshot())
}

func TestArchiveDeleteRetriesDurableMutationOnlyAfterExactPreconditionLoad(t *testing.T) {
	broker, state, _, connection, _ := archiveFixture(t, 1760)
	defer broker.Close(context.Background())
	state.muArchive.Lock()
	state.removeSteps = []repairMutation{
		{outcome: statepkg.CommitUncertain, err: errors.New("commit uncertain")},
		{outcome: statepkg.CommitApplied, apply: true},
	}
	state.muArchive.Unlock()
	archiveID := sequenceID(1762)
	command := archiveDeleteCommand(sequenceID(1771), connection.clientID, connection.ConversationID(), archiveID)
	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	receiveLifecycle(t, connection.Events())
	receiveLifecycle(t, connection.Events())
	state.muArchive.Lock()
	require.Equal(t, 2, state.removeCalls)
	state.muArchive.Unlock()
}

func TestArchiveDeleteRejectsWhileProviderWorkerIsBusy(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixture(t, 1772)
	defer broker.Close(context.Background())
	session := driver.session.(*turnSession)
	session.historyEntered = make(chan struct{}, 1)
	session.historyGate = make(chan struct{})
	history := historyCommand(sequenceID(1773), connection.clientID, connection.ConversationID(), protocol.PageRequestPayload{Limit: 1})
	historyDone := make(chan error, 1)
	go func() {
		_, commandErr := connection.Command(context.Background(), history)
		historyDone <- commandErr
	}()
	receiveLifecycle(t, session.historyEntered)
	deleteCommand := archiveDeleteCommand(sequenceID(1775), connection.clientID, connection.ConversationID(), sequenceID(1774))
	result, err := connection.Command(context.Background(), deleteCommand)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorInvalidState)
	close(session.historyGate)
	require.NoError(t, receiveLifecycle(t, historyDone))
	driver.mu.Lock()
	require.Empty(t, driver.deletes)
	driver.mu.Unlock()
}

func TestPendingDuplicateArchiveDeleteJoinsOneWorker(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixture(t, 1800)
	defer broker.Close(context.Background())
	driver.mu.Lock()
	driver.deleteEntered = make(chan struct{}, 1)
	driver.deleteGate = make(chan struct{})
	entered, gate := driver.deleteEntered, driver.deleteGate
	driver.mu.Unlock()
	command := archiveDeleteCommand(sequenceID(1811), connection.clientID, connection.ConversationID(), sequenceID(1802))
	type deleteOutcome struct {
		event protocol.Event
		err   error
	}
	firstDone := make(chan deleteOutcome, 1)
	go func() {
		event, commandErr := connection.Command(context.Background(), command)
		firstDone <- deleteOutcome{event: event, err: commandErr}
	}()
	receiveLifecycle(t, entered)
	duplicateResponse := make(chan commandResponse, 1)
	connection.actor.requests <- commandRequest{ctx: context.Background(), attachment: connection.attachment, command: command, response: duplicateResponse}
	close(gate)
	first := receiveLifecycle(t, firstDone)
	second := receiveLifecycle(t, duplicateResponse)
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.Equal(t, first.event, second.event)
	requireCommandResult(t, first.event, protocol.CommandSucceeded, "")
	require.Equal(t, protocol.EventArchive, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, first.event, receiveLifecycle(t, connection.Events()))
	driver.mu.Lock()
	require.Len(t, driver.deletes, 1)
	driver.mu.Unlock()
}

func TestAttachmentObservationDuringArchiveDeleteIsAppliedAfterRemoval(t *testing.T) {
	broker, state, driver, connection, identity := archiveFixture(t, 1815)
	defer broker.Close(context.Background())
	driver.mu.Lock()
	driver.deleteEntered = make(chan struct{}, 1)
	driver.deleteGate = make(chan struct{})
	entered, gate := driver.deleteEntered, driver.deleteGate
	driver.mu.Unlock()
	command := archiveDeleteCommand(sequenceID(1816), connection.clientID, connection.ConversationID(), sequenceID(1817))
	commandDone := make(chan error, 1)
	go func() {
		_, commandErr := connection.Command(context.Background(), command)
		commandDone <- commandErr
	}()
	receiveLifecycle(t, entered)
	newerResource := testResource(identity.CapabilityID)
	newerResource.UpdatedAt = newerResource.UpdatedAt.Add(time.Minute)
	newerDigest := strings.Repeat("a", 64)
	observing, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1818), identity.CapabilityID, newerDigest, newerResource, ""))
	require.NoError(t, err)
	defer observing.Close(context.Background())
	require.Equal(t, protocol.EventSnapshot, receiveLifecycle(t, observing.Events()).Type)
	close(gate)
	require.NoError(t, receiveLifecycle(t, commandDone))
	require.Equal(t, protocol.EventContext, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.EventArchive, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.EventCommandResult, receiveLifecycle(t, connection.Events()).Type)
	state.repairState.mu.Lock()
	require.Len(t, state.mapping.Archives, 1)
	require.NotNil(t, state.mapping.Current.Observed)
	require.Equal(t, newerDigest, state.mapping.Current.Observed.Digest)
	state.repairState.mu.Unlock()
	fresh, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1819), identity.CapabilityID, newerDigest, newerResource, ""))
	require.NoError(t, err)
	defer fresh.Close(context.Background())
	snapshot := receiveLifecycle(t, fresh.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, protocol.ContextPending, snapshot.ContextState)
}

func TestObservationUncertaintyDuringDeletePreventsPendingCrashRecovery(t *testing.T) {
	broker, state, driver, connection, identity := archiveFixture(t, 1810)
	defer broker.Close(context.Background())
	state.repairState.mu.Lock()
	state.observations = []repairMutation{{outcome: statepkg.CommitUncertain, err: errors.New("observation uncertain")}}
	state.repairState.mu.Unlock()
	old := driver.session.(*turnSession)
	driver.mu.Lock()
	driver.deleteEntered = make(chan struct{}, 1)
	driver.deleteGate = make(chan struct{})
	entered, gate := driver.deleteEntered, driver.deleteGate
	driver.mu.Unlock()
	command := archiveDeleteCommand(sequenceID(1811), connection.clientID, connection.ConversationID(), sequenceID(1812))
	type uncertainDeleteOutcome struct {
		event protocol.Event
		err   error
	}
	commandDone := make(chan uncertainDeleteOutcome, 1)
	go func() {
		event, commandErr := connection.Command(context.Background(), command)
		commandDone <- uncertainDeleteOutcome{event: event, err: commandErr}
	}()
	receiveLifecycle(t, entered)
	newer := testResource(identity.CapabilityID)
	newer.UpdatedAt = newer.UpdatedAt.Add(time.Minute)
	observing, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1813), identity.CapabilityID, strings.Repeat("b", 64), newer, ""))
	require.NoError(t, err)
	defer observing.Close(context.Background())
	receiveLifecycle(t, observing.Events())
	close(old.events)
	close(gate)
	outcome := receiveLifecycle(t, commandDone)
	require.NoError(t, outcome.err)
	requireCommandResult(t, outcome.event, protocol.CommandRejected, protocol.ErrorStateRepairFailed)
	require.Equal(t, protocol.ErrorStateRepairFailed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, protocol.EventArchive, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, outcome.event, receiveLifecycle(t, connection.Events()))
	require.Equal(t, protocol.ErrorProviderCrashed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	driver.mu.Lock()
	require.Equal(t, 1, driver.resumes, "uncertain observation must prevent a stale recovery handoff")
	driver.mu.Unlock()
}

func TestProviderCrashWaitsForArchiveDeleteWorker(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixture(t, 1820)
	defer broker.Close(context.Background())
	old := driver.session.(*turnSession)
	replacement := newTurnSession(old.native.Ref.Value())
	driver.mu.Lock()
	driver.resumeSessions = []provider.Session{old, replacement}
	driver.deleteEntered = make(chan struct{}, 1)
	driver.deleteGate = make(chan struct{})
	entered, gate := driver.deleteEntered, driver.deleteGate
	driver.mu.Unlock()
	command := archiveDeleteCommand(sequenceID(1831), connection.clientID, connection.ConversationID(), sequenceID(1822))
	commandDone := make(chan error, 1)
	go func() {
		_, commandErr := connection.Command(context.Background(), command)
		commandDone <- commandErr
	}()
	receiveLifecycle(t, entered)
	close(old.events)
	select {
	case event := <-connection.Events():
		t.Fatalf("recovery started before archive worker settled: %s", event.Type)
	default:
	}
	close(gate)
	require.NoError(t, receiveLifecycle(t, commandDone))
	require.Equal(t, protocol.EventArchive, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.EventCommandResult, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.ErrorProviderCrashed, receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload).Error.Code())
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	driver.mu.Lock()
	require.Equal(t, 2, driver.resumes)
	driver.mu.Unlock()
}

func TestBrokerCloseJoinsPendingArchiveDelete(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixture(t, 1840)
	driver.mu.Lock()
	driver.deleteEntered = make(chan struct{}, 1)
	driver.deleteGate = make(chan struct{})
	entered := driver.deleteEntered
	driver.mu.Unlock()
	command := archiveDeleteCommand(sequenceID(1851), connection.clientID, connection.ConversationID(), sequenceID(1842))
	type closeDeleteOutcome struct {
		event protocol.Event
		err   error
	}
	commandDone := make(chan closeDeleteOutcome, 1)
	go func() {
		event, commandErr := connection.Command(context.Background(), command)
		commandDone <- closeDeleteOutcome{event: event, err: commandErr}
	}()
	receiveLifecycle(t, entered)
	closeDone := make(chan error, 1)
	go func() { closeDone <- broker.Close(context.Background()) }()
	outcome := receiveLifecycle(t, commandDone)
	require.NoError(t, outcome.err)
	requireCommandResult(t, outcome.event, protocol.CommandRejected, protocol.ErrorArchiveDeleteRetained)
	require.NoError(t, receiveLifecycle(t, closeDone))
	require.EqualValues(t, 1, driver.session.(*turnSession).shutdowns.Load())
}

func TestArchiveRemovalNegativeClassificationsDoNotRetry(t *testing.T) {
	tests := []struct {
		name      string
		step      func(statepkg.Mapping) repairMutation
		wantCode  protocol.BrowserErrorCode
		wantCalls int
	}{
		{name: "loaded target", step: func(statepkg.Mapping) repairMutation {
			return repairMutation{outcome: statepkg.CommitUncertain, err: errors.New("uncertain"), apply: true}
		}, wantCalls: 1},
		{name: "unknown outcome", step: func(statepkg.Mapping) repairMutation {
			return repairMutation{outcome: statepkg.CommitOutcome("future")}
		}, wantCode: protocol.ErrorStateRepairFailed, wantCalls: 1},
		{name: "loaded other", step: func(mapping statepkg.Mapping) repairMutation {
			other := cloneMapping(mapping)
			other.Current.ModelLabel = "other"
			return repairMutation{outcome: statepkg.CommitUncertain, err: errors.New("uncertain"), mapping: &other}
		}, wantCode: protocol.ErrorStateRepairFailed, wantCalls: 1},
		{name: "loaded invalid", step: func(statepkg.Mapping) repairMutation {
			invalid := statepkg.Mapping{}
			return repairMutation{outcome: statepkg.CommitUncertain, err: errors.New("uncertain"), mapping: &invalid}
		}, wantCode: protocol.ErrorStateRepairFailed, wantCalls: 1},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := uint64(1860 + index*20)
			broker, state, _, connection, _ := archiveFixture(t, base)
			defer broker.Close(context.Background())
			state.repairState.mu.Lock()
			mapping := cloneMapping(*state.mapping)
			state.repairState.mu.Unlock()
			state.muArchive.Lock()
			state.removeSteps = []repairMutation{test.step(mapping)}
			state.muArchive.Unlock()
			command := archiveDeleteCommand(sequenceID(base+11), connection.clientID, connection.ConversationID(), sequenceID(base+2))
			result, err := connection.Command(context.Background(), command)
			require.NoError(t, err)
			if test.wantCode == "" {
				requireCommandResult(t, result, protocol.CommandSucceeded, "")
				receiveLifecycle(t, connection.Events())
			} else {
				requireCommandResult(t, result, protocol.CommandRejected, test.wantCode)
			}
			receiveLifecycle(t, connection.Events())
			state.muArchive.Lock()
			require.Equal(t, test.wantCalls, state.removeCalls)
			state.muArchive.Unlock()
		})
	}
}

func TestArchiveStalePreparedAndRetryReferencesFailWithoutProviderAccess(t *testing.T) {
	broker, _, driver, connection, _ := archiveFixtureWithMapping(t, 1780, func(mapping *statepkg.Mapping) {
		prepared := mapping.Archives[0]
		revision := statepkg.Revision{Digest: "0000000000000000000000000000000000000000000000000000000000000000", Revision: statepkg.RevisionInitial, SourceUpdatedAt: prepared.UpdatedAt}
		prepared.Observed = &revision
		prepared.PreparedCommit = &statepkg.PreparedCommit{Revision: revision, TurnID: sequenceID(1792), Phase: statepkg.CommitPrepared}
		mapping.Archives[0] = prepared
	})
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	stale := archiveDeleteCommand(sequenceID(1791), connection.clientID, conversationID, sequenceID(1799))
	result, err := connection.Command(context.Background(), stale)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorStaleReference)
	receiveLifecycle(t, connection.Events())
	preparedCommand := archiveDeleteCommand(sequenceID(1793), connection.clientID, conversationID, sequenceID(1781))
	result, err = connection.Command(context.Background(), preparedCommand)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorStateRepairFailed)
	receiveLifecycle(t, connection.Events())
	retry := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(1794), ClientID: connection.clientID, ConversationID: &conversationID, Type: protocol.CommandRetry, Payload: protocol.TurnReferencePayload{TurnID: sequenceID(1795)}}
	result, err = connection.Command(context.Background(), retry)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorInvalidState)
	receiveLifecycle(t, connection.Events())
	driver.mu.Lock()
	require.Empty(t, driver.deletes)
	driver.mu.Unlock()
}

var _ StateStore = (*archiveTestStore)(nil)
var _ archiveRemovalStore = (*archiveTestStore)(nil)
var _ provider.Driver = (*archiveTestDriver)(nil)
