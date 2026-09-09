package broker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/stretchr/testify/require"
)

type missingResumeDriver struct {
	*archiveTestDriver
	missing provider.NativeSessionRef
}

func TestMissingActorShutdownJoinsWorkerAndSupportsRetry(t *testing.T) {
	worker := make(chan struct{})
	attempt := newActorShutdown(&sessionHandle{missing: true}, worker)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, attempt.run(ctx, time.Second))
	close(worker)
	require.NoError(t, attempt.run(context.Background(), time.Second), "retry must observe the settled worker without requiring a native session")
}

func TestMissingConversationCloseJoinsArchiveDeletion(t *testing.T) {
	oldBroker, store, base, _, identity := archiveFixture(t, 9300)
	require.NoError(t, oldBroker.Close(context.Background()))
	mapping, err := store.Load(identity)
	require.NoError(t, err)
	base.deleteEntered = make(chan struct{}, 1)
	base.deleteGate = make(chan struct{})
	defer close(base.deleteGate)
	driver := &missingResumeDriver{archiveTestDriver: base, missing: mapping.Current.NativeSession}
	broker, err := New(validLifecycleConfig(store, driver, &lockedIDs{next: 9400}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = broker.Close(context.Background()) })
	raw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(9450), identity.CapabilityID))
	require.NoError(t, err)
	connection := raw.(*Connection)
	receiveLifecycle(t, connection.Events())
	receiveLifecycle(t, connection.Events())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = connection.Command(context.Background(), archiveDeleteCommand(sequenceID(9451), connection.clientID, connection.ConversationID(), mapping.Archives[0].ConversationID))
	}()
	select {
	case <-base.deleteEntered:
	case <-time.After(time.Second):
		t.Fatal("archive delete did not start")
	}
	require.NoError(t, broker.Close(context.Background()))
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("archive command did not settle")
	}
}

type missingAfterFirstResumeDriver struct {
	*missingResumeDriver
	count atomic.Int32
}

func (driver *missingAfterFirstResumeDriver) Resume(ctx context.Context, request provider.ResumeRequest) (provider.Session, error) {
	if driver.count.Add(1) == 1 {
		return driver.archiveTestDriver.Resume(ctx, request)
	}
	return driver.missingResumeDriver.Resume(ctx, request)
}

func TestLiveRecoveryMissingRetiresAndAllowsExplicitNew(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(9500))
	store := &archiveTestStore{repairState: &repairState{mapping: &mapping}}
	old := newTurnSession(mapping.Current.NativeSession.Value())
	base := &archiveTestDriver{session: old, createSessions: []provider.Session{newTurnSession("sessions/new-after-crash")}, store: store}
	driver := &missingAfterFirstResumeDriver{missingResumeDriver: &missingResumeDriver{archiveTestDriver: base, missing: mapping.Current.NativeSession}}
	broker, err := New(validLifecycleConfig(store, driver, &lockedIDs{next: 9600}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, broker.Close(context.Background())) })
	raw, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(9650), identity.CapabilityID))
	require.NoError(t, err)
	connection := raw.(*Connection)
	receiveLifecycle(t, connection.Events())
	before, err := store.Load(identity)
	require.NoError(t, err)
	close(old.events)
	closed := make(chan struct{})
	go func() {
		for range connection.Events() {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("confirmed missing recovery must retire the joined old actor")
	}
	reconnected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(9651), identity.CapabilityID))
	require.NoError(t, err)
	require.Equal(t, protocol.LifecycleUnavailable, receiveLifecycle(t, reconnected.Events()).Payload.(protocol.SnapshotPayload).Lifecycle)
	receiveLifecycle(t, reconnected.Events())
	next := reconnected.(*Connection)
	result, err := next.Command(context.Background(), newCommand(sequenceID(9652), next.clientID, next.ConversationID()))
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	after, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, *before.Current, after.Archives[0])
	require.Empty(t, base.deletes)
}

func (driver *missingResumeDriver) Resume(ctx context.Context, request provider.ResumeRequest) (provider.Session, error) {
	if request.NativeSession == driver.missing {
		return nil, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return driver.archiveTestDriver.Resume(ctx, request)
}

func TestMissingCurrentCanConnectAndExplicitlyStartNewWithoutReplacingOldReference(t *testing.T) {
	oldBroker, store, driver, oldConnection, identity := archiveFixture(t, 8800)
	oldID := oldConnection.ConversationID()
	require.NoError(t, oldBroker.Close(context.Background()))
	before, err := store.Load(identity)
	require.NoError(t, err)
	driver.createSessions = []provider.Session{newTurnSession("sessions/recovered-new")}
	missing := &missingResumeDriver{archiveTestDriver: driver, missing: before.Current.NativeSession}
	broker, err := New(validLifecycleConfig(store, missing, &lockedIDs{next: 8900}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, broker.Close(context.Background())) })
	connected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(8850), identity.CapabilityID))
	require.NoError(t, err, "a missing native thread must not prevent conversation management")
	connection := connected.(*Connection)
	snapshot := receiveLifecycle(t, connection.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, oldID, connection.ConversationID())
	require.Equal(t, protocol.LifecycleUnavailable, snapshot.Lifecycle)
	require.Equal(t, protocol.ComposerBlocked, snapshot.ComposerAdmission)
	failure := receiveLifecycle(t, connection.Events()).Payload.(protocol.ErrorPayload)
	require.Equal(t, protocol.ErrorNativeSessionMissing, failure.Error.Code())
	afterConnect, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, before, afterConnect, "opening unavailable state must preserve all saved metadata")
	require.Empty(t, driver.createRequests, "connect must never silently create a replacement")
	history := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(8853), ClientID: connection.clientID, ConversationID: &oldID, Type: protocol.CommandHistoryPage, Payload: protocol.PageRequestPayload{Limit: 50}}
	rejected, err := connection.Command(context.Background(), history)
	require.NoError(t, err)
	requireCommandResult(t, rejected, protocol.CommandRejected, protocol.ErrorNativeSessionMissing)
	result, err := connection.Command(context.Background(), newCommand(sequenceID(8851), connection.clientID, oldID))
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	after, err := store.Load(identity)
	require.NoError(t, err)
	require.NotEqual(t, oldID, after.Current.ConversationID)
	require.Equal(t, *before.Current, after.Archives[len(after.Archives)-1])
	require.Empty(t, driver.deletes)
	refreshed, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(8852), identity.CapabilityID))
	require.NoError(t, err)
	require.Equal(t, after.Current.ConversationID, refreshed.ConversationID())
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, refreshed.Events()).Payload.(protocol.SnapshotPayload).Lifecycle)
}

func TestMissingCurrentRecoverySurvivesStateStoreReopenWithPreparedMetadataIntact(t *testing.T) {
	identity, mapping, _ := preparedRepairMapping(t, statepkg.CommitPrepared)
	home := t.TempDir()
	store, err := statepkg.Open(home)
	require.NoError(t, err)
	outcome, err := store.Create(identity, *mapping.Current, testTime())
	require.NoError(t, err)
	require.Equal(t, statepkg.CommitApplied, outcome)
	before, err := store.Load(identity)
	require.NoError(t, err)
	base := &archiveTestDriver{createSessions: []provider.Session{newTurnSession("sessions/durable-recovery")}}
	driver := &missingResumeDriver{archiveTestDriver: base, missing: before.Current.NativeSession}
	broker, err := New(validLifecycleConfig(store, driver, &lockedIDs{next: 9100}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = broker.Close(context.Background()); _ = store.Close() })
	connected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(9150), identity.CapabilityID))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	receiveLifecycle(t, connection.Events())
	result, err := connection.Command(context.Background(), newCommand(sequenceID(9151), connection.clientID, connection.ConversationID()))
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	after, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, *before.Current, after.Archives[0], "an unknown submission outcome must remain attached to the old reference")
	require.NoError(t, broker.Close(context.Background()))
	require.NoError(t, store.Close())
	reopened, err := statepkg.Open(home)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	persisted, err := reopened.Load(identity)
	require.NoError(t, err)
	require.Equal(t, after, persisted)
	base.session = newTurnSession(after.Current.NativeSession.Value())
	restarted, err := New(validLifecycleConfig(reopened, driver, &lockedIDs{next: 9200}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restarted.Close(context.Background())) })
	refreshed, err := restarted.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(9250), identity.CapabilityID))
	require.NoError(t, err)
	require.Equal(t, after.Current.ConversationID, refreshed.ConversationID())
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, refreshed.Events()).Payload.(protocol.SnapshotPayload).Lifecycle)
}
