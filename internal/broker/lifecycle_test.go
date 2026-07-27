package broker

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/localapi"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

type lifecycleState struct {
	mu           sync.Mutex
	mappings     map[agentstate.Identity]agentstate.Mapping
	creates      int
	ensures      []string
	removes      []string
	outcome      agentstate.CommitOutcome
	createErr    error
	doNotPersist bool
}

func (s *lifecycleState) Load(identity agentstate.Identity) (agentstate.Mapping, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mapping, ok := s.mappings[identity]
	if !ok {
		return agentstate.Mapping{}, os.ErrNotExist
	}
	return cloneMapping(mapping), nil
}
func (s *lifecycleState) Create(identity agentstate.Identity, session agentstate.Session, at time.Time) (agentstate.CommitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	outcome := s.outcome
	if outcome == "" {
		outcome = agentstate.CommitApplied
	}
	if outcome != agentstate.CommitNotApplied && !s.doNotPersist {
		s.mappings[identity] = agentstate.Mapping{SchemaVersion: agentstate.SchemaVersion, Identity: identity, Current: &session, Archives: []agentstate.Session{}, CreatedAt: at, UpdatedAt: at}
	}
	return outcome, s.createErr
}
func (s *lifecycleState) ObserveRevision(identity agentstate.Identity, revision agentstate.Revision, at time.Time) (agentstate.CommitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mapping, ok := s.mappings[identity]
	if !ok || mapping.Current == nil || mapping.Current.PreparedCommit != nil {
		return agentstate.CommitNotApplied, errors.New("observation rejected")
	}
	observed := revision
	mapping.Current.Observed = &observed
	mapping.Current.UpdatedAt = at
	mapping.UpdatedAt = at
	s.mappings[identity] = mapping
	return agentstate.CommitApplied, nil
}
func (s *lifecycleState) PromotePrepared(identity agentstate.Identity, turnID string, at time.Time) (agentstate.CommitOutcome, error) {
	return agentstate.CommitNotApplied, errors.New("promotion not configured")
}
func (s *lifecycleState) ReconcilePrepared(identity agentstate.Identity, turnID string, accepted bool, at time.Time) (agentstate.CommitOutcome, error) {
	return agentstate.CommitNotApplied, errors.New("reconciliation not configured")
}
func (s *lifecycleState) EnsureWorkspace(id string) (string, error) {
	s.mu.Lock()
	s.ensures = append(s.ensures, id)
	s.mu.Unlock()
	return "/tmp/agent-whiteboard-test/" + id, nil
}
func (s *lifecycleState) RemoveWorkspace(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removes = append(s.removes, id)
	return nil
}

type lifecycleDriver struct {
	mu          sync.Mutex
	createGate  chan struct{}
	createEnter chan struct{}
	enterOnce   sync.Once
	creates     []provider.CreateRequest
	resumes     []provider.ResumeRequest
	deletes     []provider.DeleteRequest
	sessions    []*lifecycleSession
}

func (d *lifecycleDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "model"}
}
func (d *lifecycleDriver) Create(ctx context.Context, request provider.CreateRequest) (provider.Session, error) {
	if d.createEnter != nil {
		d.enterOnce.Do(func() { close(d.createEnter) })
	}
	if d.createGate != nil {
		select {
		case <-d.createGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.creates = append(d.creates, request)
	session := newLifecycleSession(sequenceID(uint64(len(d.sessions) + 100)))
	d.sessions = append(d.sessions, session)
	return session, nil
}
func (d *lifecycleDriver) Resume(_ context.Context, request provider.ResumeRequest) (provider.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resumes = append(d.resumes, request)
	session := newLifecycleSession(request.NativeSession.Value())
	d.sessions = append(d.sessions, session)
	return session, nil
}
func (d *lifecycleDriver) Inspect(context.Context, provider.InspectRequest) (provider.NativeSession, error) {
	return provider.NativeSession{}, nil
}
func (d *lifecycleDriver) Delete(_ context.Context, request provider.DeleteRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletes = append(d.deletes, request)
	return nil
}

type lifecycleSession struct {
	native        provider.NativeSession
	events        chan provider.Event
	shutdownCalls atomic.Int32
	shutdownErr   error
}

func newLifecycleSession(refValue string) *lifecycleSession {
	ref, _ := provider.NewNativeSessionRef(refValue)
	return &lifecycleSession{native: provider.NativeSession{Ref: ref, Provider: provider.NamePi, Model: "model", CreatedAt: testTime(), UpdatedAt: testTime()}, events: make(chan provider.Event, 4096)}
}
func (s *lifecycleSession) NativeSession() provider.NativeSession { return s.native }
func (s *lifecycleSession) Model() string                         { return s.native.Model }
func (s *lifecycleSession) History(context.Context, provider.HistoryRequest) (provider.HistoryPage, error) {
	return provider.HistoryPage{}, nil
}
func (s *lifecycleSession) Preflight(context.Context, provider.PreflightRequest) (provider.PreflightResult, error) {
	return provider.PreflightResult{}, nil
}
func (s *lifecycleSession) Submit(context.Context, provider.TurnRequest) (provider.AcceptedTurn, error) {
	return provider.AcceptedTurn{}, nil
}
func (s *lifecycleSession) Events() <-chan provider.Event                          { return s.events }
func (s *lifecycleSession) Interrupt(context.Context, provider.AcceptedTurn) error { return nil }
func (s *lifecycleSession) Reconcile(context.Context, provider.TurnReference) (provider.TurnState, error) {
	return provider.TurnUnknown, nil
}
func (s *lifecycleSession) Child() provider.ManagedChild { return lifecycleChild{} }
func (s *lifecycleSession) Shutdown(context.Context) error {
	s.shutdownCalls.Add(1)
	return s.shutdownErr
}

type lifecycleChild struct{}

func (lifecycleChild) Input() io.WriteCloser { return nopWriteCloser{} }
func (lifecycleChild) Output() io.Reader     { return nil }
func (lifecycleChild) Errors() io.Reader     { return nil }
func (lifecycleChild) Wait() error           { return nil }
func (lifecycleChild) Terminate() error      { return nil }
func (lifecycleChild) Kill() error           { return nil }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(value []byte) (int, error) { return len(value), nil }
func (nopWriteCloser) Close() error                    { return nil }

func validLifecycleConfig(state StateStore, driver provider.Driver, ids common.IDGenerator) Config {
	return Config{State: state, Driver: driver, IDs: ids, Clock: testClock{now: testTime()}, ShutdownTimeout: time.Second}
}
func lifecycleConnect(client, resource string) agentprotocol.Command {
	payload := agentprotocol.ConnectPayload{Provider: agentprotocol.ProviderPi, Resource: testResource(resource), ContextDigest: string(make([]byte, 64))}
	payload.ContextDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	return agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(1), ClientID: client, Type: agentprotocol.CommandConnect, Payload: payload}
}

func TestNewRejectsNilTypedNilAndInvalidTimeout(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	driver := &lifecycleDriver{}
	ids := &testIDGenerator{ids: []string{sequenceID(10)}}
	_, err := New(Config{})
	require.Error(t, err)
	var typedState *lifecycleState
	_, err = New(validLifecycleConfig(typedState, driver, ids))
	require.Error(t, err)
	var typedDriver *lifecycleDriver
	_, err = New(validLifecycleConfig(state, typedDriver, ids))
	require.Error(t, err)
	config := validLifecycleConfig(state, driver, ids)
	config.ShutdownTimeout = 0
	_, err = New(config)
	require.Error(t, err)
}

func TestConcurrentConnectSingleFlightsIdentityAndAttachesSnapshots(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	gate := make(chan struct{})
	driver := &lifecycleDriver{createGate: gate}
	ids := &lockedIDs{next: 10}
	broker, err := New(validLifecycleConfig(state, driver, ids))
	require.NoError(t, err)
	defer broker.Close(context.Background())

	type connectResult struct {
		connection localapi.Connection
		err        error
	}
	connections := make(chan connectResult, 2)
	for _, client := range []string{sequenceID(20), sequenceID(21)} {
		client := client
		go func() {
			connection, connectErr := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(client, sequenceID(30)))
			connections <- connectResult{connection: connection, err: connectErr}
		}()
	}
	close(gate)
	firstResult := receiveLifecycle(t, connections)
	secondResult := receiveLifecycle(t, connections)
	require.NoError(t, firstResult.err)
	require.NoError(t, secondResult.err)
	first := firstResult.connection.(*Connection)
	second := secondResult.connection.(*Connection)
	require.Equal(t, first.ConversationID(), second.ConversationID())
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, first.Events()).Type)
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, second.Events()).Type)
	driver.mu.Lock()
	require.Len(t, driver.creates, 1)
	driver.mu.Unlock()
}

type lockedIDs struct {
	mu   sync.Mutex
	next uint64
}

func (g *lockedIDs) NewID() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return sequenceID(g.next), nil
}

func TestAttachObservesInitialDigestAndClassifiesMatchingObservedDigestPending(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	broker, err := New(validLifecycleConfig(state, &lifecycleDriver{}, &lockedIDs{next: 40}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	origin := "https://example.com"
	resourceID := sequenceID(41)

	first, err := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(42), resourceID))
	require.NoError(t, err)
	firstSnapshot := receiveLifecycle(t, first.Events()).Payload.(agentprotocol.SnapshotPayload)
	require.Equal(t, agentprotocol.ContextPending, firstSnapshot.ContextState)
	identity, err := IdentityFromConnect(origin, lifecycleConnect(sequenceID(42), resourceID).Payload.(agentprotocol.ConnectPayload), origin)
	require.NoError(t, err)
	state.mu.Lock()
	observed := state.mappings[identity].Current.Observed
	state.mu.Unlock()
	require.NotNil(t, observed)
	require.Equal(t, strings.Repeat("0", 64), observed.Digest)
	require.Equal(t, agentstate.RevisionInitial, observed.Revision)
	require.Equal(t, testResource(resourceID).UpdatedAt, observed.SourceUpdatedAt)

	second, err := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(43), resourceID))
	require.NoError(t, err)
	secondSnapshot := receiveLifecycle(t, second.Events()).Payload.(agentprotocol.SnapshotPayload)
	require.Equal(t, agentprotocol.ContextPending, secondSnapshot.ContextState)
}

func TestUnsupportedCommandIsTargetedAndConnectionCloseDoesNotShutdownProvider(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	driver := &lifecycleDriver{}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 50}))
	require.NoError(t, err)
	firstRaw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(60), sequenceID(70)))
	require.NoError(t, err)
	secondRaw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(61), sequenceID(70)))
	require.NoError(t, err)
	first := firstRaw.(*Connection)
	second := secondRaw.(*Connection)
	receiveLifecycle(t, first.Events())
	receiveLifecycle(t, second.Events())
	conversation := first.ConversationID()
	command := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(80), ClientID: sequenceID(60), ConversationID: &conversation, Type: agentprotocol.CommandNew, Payload: agentprotocol.EmptyPayload{}}
	result, err := first.Command(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, agentprotocol.EventCommandResult, result.Type)
	require.Equal(t, agentprotocol.ErrorInvalidState, result.Payload.(agentprotocol.CommandResultPayload).Error.Code())
	require.Equal(t, result.EventID, receiveLifecycle(t, first.Events()).EventID)
	select {
	case <-second.Events():
		t.Fatal("result leaked to another client")
	default:
	}
	require.NoError(t, first.Close(context.Background()))
	require.Zero(t, driver.sessions[0].shutdownCalls.Load())
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 1, driver.sessions[0].shutdownCalls.Load())
}

func TestReplayAfterLatestCreatesSnapshotCheckpointAndMissingCursorIsSafe(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	broker, err := New(validLifecycleConfig(state, &lifecycleDriver{}, &lockedIDs{next: 100}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	client := sequenceID(110)
	resource := sequenceID(120)
	firstRaw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(client, resource))
	require.NoError(t, err)
	first := firstRaw.(*Connection)
	snapshot := receiveLifecycle(t, first.Events())
	require.NoError(t, first.Close(context.Background()))
	connect := lifecycleConnect(client, resource)
	payload := connect.Payload.(agentprotocol.ConnectPayload)
	payload.ReplayAfter = snapshot.EventID
	connect.Payload = payload
	replayedRaw, err := broker.Connect(context.Background(), "https://example.com", connect)
	require.NoError(t, err)
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, replayedRaw.Events()).Type)
	require.NoError(t, replayedRaw.Close(context.Background()))
	payload.ReplayAfter = sequenceID(999)
	connect.Payload = payload
	_, err = broker.Connect(context.Background(), "https://example.com", connect)
	var safe BrokerError
	require.ErrorAs(t, err, &safe)
	require.Equal(t, agentprotocol.ErrorReplayWindowUnavailable, safe.Code())
	require.NotContains(t, err.Error(), payload.ReplayAfter)
}

func TestStateCreateNotAppliedCompensatesAndAppliedErrorIsRecoveredByLoad(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    agentstate.CommitOutcome
		createErr  error
		wantError  bool
		wantDelete int
	}{
		{name: "not applied", outcome: agentstate.CommitNotApplied, createErr: errors.New("disk /secret"), wantError: true, wantDelete: 1},
		{name: "applied with error", outcome: agentstate.CommitApplied, createErr: errors.New("sync /secret"), wantError: false, wantDelete: 0},
		{name: "uncertain but load proves applied", outcome: agentstate.CommitUncertain, createErr: errors.New("sync /secret"), wantError: false, wantDelete: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping), outcome: test.outcome, createErr: test.createErr}
			driver := &lifecycleDriver{}
			broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 200}))
			require.NoError(t, err)
			connection, connectErr := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(210), sequenceID(220)))
			if test.wantError {
				require.Error(t, connectErr)
				require.Nil(t, connection)
			} else {
				require.NoError(t, connectErr)
				require.NotNil(t, connection)
			}
			driver.mu.Lock()
			require.Len(t, driver.deletes, test.wantDelete)
			driver.mu.Unlock()
			_ = broker.Close(context.Background())
		})
	}
}

func TestDifferentIdentitiesCreateIsolatedSessionsAndExistingMappingResumesExactly(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	driver := &lifecycleDriver{}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 300}))
	require.NoError(t, err)
	first, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(310), sequenceID(320)))
	require.NoError(t, err)
	second, err := broker.Connect(context.Background(), "https://other.example.com", lifecycleConnect(sequenceID(311), sequenceID(320)))
	require.NoError(t, err)
	require.NotEqual(t, first.ConversationID(), second.ConversationID())
	driver.mu.Lock()
	require.Len(t, driver.creates, 2)
	for _, request := range driver.creates {
		require.Equal(t, provider.NamePi, request.Provider)
		require.Equal(t, provider.AccessContentOnly, request.Access)
		require.Contains(t, request.Workspace, "/tmp/agent-whiteboard-test/")
	}
	driver.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))

	identity := agentstate.Identity{Origin: "https://example.com", Kind: agentstate.ResourceMarkdown, CapabilityID: sequenceID(330), Provider: provider.NamePi}
	ref, err := agentstate.NativeSessionRef("native/session.json")
	require.NoError(t, err)
	current := agentstate.Session{ConversationID: sequenceID(331), NativeSession: ref, CreatedAt: testTime(), UpdatedAt: testTime(), ProviderLabel: "pi", ModelLabel: "model"}
	state = &lifecycleState{mappings: map[agentstate.Identity]agentstate.Mapping{identity: {SchemaVersion: agentstate.SchemaVersion, Identity: identity, Current: &current, CreatedAt: testTime(), UpdatedAt: testTime()}}}
	driver = &lifecycleDriver{}
	broker, err = New(validLifecycleConfig(state, driver, &lockedIDs{next: 340}))
	require.NoError(t, err)
	resumed, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(341), identity.CapabilityID))
	require.NoError(t, err)
	require.Equal(t, current.ConversationID, resumed.ConversationID())
	driver.mu.Lock()
	require.Len(t, driver.resumes, 1)
	require.Equal(t, ref, driver.resumes[0].NativeSession)
	require.Equal(t, "/tmp/agent-whiteboard-test/"+current.ConversationID, driver.resumes[0].Workspace)
	driver.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))
}

func TestUncertainAbsentCommitCompensatesButUnclassifiableOutcomeDoesNot(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    agentstate.CommitOutcome
		wantDelete int
	}{
		{name: "uncertain and absent", outcome: agentstate.CommitUncertain, wantDelete: 1},
		{name: "unknown outcome", outcome: agentstate.CommitOutcome("future"), wantDelete: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping), outcome: test.outcome, createErr: errors.New("redacted /state/path"), doNotPersist: true}
			driver := &lifecycleDriver{}
			broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 350}))
			require.NoError(t, err)
			_, connectErr := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(351), sequenceID(352)))
			require.Error(t, connectErr)
			require.NotContains(t, connectErr.Error(), "/state/path")
			driver.mu.Lock()
			require.Len(t, driver.deletes, test.wantDelete)
			driver.mu.Unlock()
			_ = broker.Close(context.Background())
		})
	}
}

func TestSlowSubscriberIsEvictedWithoutAffectingFastSubscriber(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	driver := &lifecycleDriver{}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 400}))
	require.NoError(t, err)
	slowRaw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(410), sequenceID(420)))
	require.NoError(t, err)
	fastRaw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(411), sequenceID(420)))
	require.NoError(t, err)
	slow := slowRaw.(*Connection)
	fast := fastRaw.(*Connection)
	receiveLifecycle(t, slow.Events())
	receiveLifecycle(t, fast.Events())

	const eventCount = MaxReplayEvents + 1
	produced := make(chan struct{})
	go func() {
		for index := 0; index < eventCount; index++ {
			driver.sessions[0].events <- provider.NewActivityEvent("", provider.ActivityStatus, "working")
		}
		close(produced)
	}()
	for index := 0; index < eventCount; index++ {
		event, open := receiveLifecycleOpen(t, fast.Events())
		require.True(t, open)
		require.Equal(t, agentprotocol.EventActivity, event.Type)
	}
	waitLifecycle(t, produced)
	select {
	case <-slow.attachment.detached:
	case <-time.After(time.Second):
		t.Fatal("slow attachment was not evicted")
	}
	require.NoError(t, broker.Close(context.Background()))
}

func TestConnectionDetachAndBrokerShutdownAreRetrySafe(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	driver := &lifecycleDriver{}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 500}))
	require.NoError(t, err)
	raw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(510), sequenceID(520)))
	require.NoError(t, err)
	connection := raw.(*Connection)
	receiveLifecycle(t, connection.Events())
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, connection.Close(canceled), context.Canceled)
	require.NoError(t, connection.Close(context.Background()))
	require.Zero(t, driver.sessions[0].shutdownCalls.Load())

	driver.sessions[0].shutdownErr = errors.New("provider /private/ref")
	closeErr := broker.Close(context.Background())
	require.Error(t, closeErr)
	require.NotContains(t, closeErr.Error(), "/private/ref")
	driver.sessions[0].shutdownErr = nil
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 2, driver.sessions[0].shutdownCalls.Load())
}

func TestBrokerCloseWaitsForInflightStartupWithoutHoldingRegistryMutex(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	gate := make(chan struct{})
	entered := make(chan struct{})
	driver := &lifecycleDriver{createGate: gate, createEnter: entered}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 600}))
	require.NoError(t, err)
	connectDone := make(chan error, 1)
	go func() {
		_, connectErr := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(610), sequenceID(620)))
		connectDone <- connectErr
	}()
	waitLifecycle(t, entered)
	closeDone := make(chan error, 1)
	go func() { closeDone <- broker.Close(context.Background()) }()
	connectErr := receiveLifecycle(t, connectDone)
	require.Error(t, connectErr)
	var shuttingDown BrokerError
	require.ErrorAs(t, connectErr, &shuttingDown)
	require.Equal(t, agentprotocol.ErrorBrokerShuttingDown, shuttingDown.Code())
	require.NoError(t, receiveLifecycle(t, closeDone))
	driver.mu.Lock()
	require.Empty(t, driver.sessions)
	driver.mu.Unlock()
	close(gate)
}

func TestCanceledConnectBeforeAdmissionCreatesNothing(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	driver := &lifecycleDriver{}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 700}))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	connection, err := broker.Connect(ctx, "https://example.com", lifecycleConnect(sequenceID(710), sequenceID(720)))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, connection)
	driver.mu.Lock()
	require.Empty(t, driver.creates)
	driver.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))
}

var _ StateStore = (*lifecycleState)(nil)
var _ provider.Driver = (*lifecycleDriver)(nil)
