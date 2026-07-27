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
	"github.com/edocsss/agent-whiteboard/internal/localapi"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

const lifecycleTestTimeout = 3 * time.Second

func receiveLifecycle[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(lifecycleTestTimeout):
		t.Fatal("timed out waiting for lifecycle test result")
		var zero T
		return zero
	}
}

func receiveLifecycleOpen[T any](t *testing.T, values <-chan T) (T, bool) {
	t.Helper()
	select {
	case value, open := <-values:
		return value, open
	case <-time.After(lifecycleTestTimeout):
		t.Fatal("timed out waiting for lifecycle test channel")
		var zero T
		return zero, false
	}
}

func waitLifecycle(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(lifecycleTestTimeout):
		t.Fatal("timed out waiting for lifecycle test barrier")
	}
}

type hardeningState struct {
	mu             sync.Mutex
	mapping        *agentstate.Mapping
	outcome        agentstate.CommitOutcome
	removeFailures int
	removeCalls    int
}

func (state *hardeningState) Load(identity agentstate.Identity) (agentstate.Mapping, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.mapping == nil {
		return agentstate.Mapping{}, os.ErrNotExist
	}
	return *state.mapping, nil
}
func (state *hardeningState) Create(agentstate.Identity, agentstate.Session, time.Time) (agentstate.CommitOutcome, error) {
	if state.outcome == "" {
		return agentstate.CommitNotApplied, errors.New("not applied")
	}
	return state.outcome, errors.New("not applied")
}
func (*hardeningState) EnsureWorkspace(id string) (string, error) {
	return "/tmp/agent-whiteboard-test/" + id, nil
}
func (state *hardeningState) RemoveWorkspace(string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.removeCalls++
	if state.removeFailures > 0 {
		state.removeFailures--
		return errors.New("remove failed /private/workspace")
	}
	return nil
}

type hardeningDriver struct {
	mu             sync.Mutex
	createSession  provider.Session
	createErr      error
	resumeSession  provider.Session
	resumeErr      error
	createEntered  chan struct{}
	createGate     chan struct{}
	createOnce     sync.Once
	createCalls    int
	resumeCalls    int
	deleteFailures int
	deleteCalls    int
}

func (*hardeningDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "model"}
}
func (driver *hardeningDriver) Create(ctx context.Context, _ provider.CreateRequest) (provider.Session, error) {
	driver.mu.Lock()
	driver.createCalls++
	driver.mu.Unlock()
	if driver.createEntered != nil {
		driver.createOnce.Do(func() { close(driver.createEntered) })
	}
	if driver.createGate != nil {
		select {
		case <-driver.createGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return driver.createSession, driver.createErr
}
func (driver *hardeningDriver) Resume(context.Context, provider.ResumeRequest) (provider.Session, error) {
	driver.mu.Lock()
	driver.resumeCalls++
	driver.mu.Unlock()
	return driver.resumeSession, driver.resumeErr
}
func (*hardeningDriver) Inspect(context.Context, provider.InspectRequest) (provider.NativeSession, error) {
	return provider.NativeSession{}, nil
}
func (driver *hardeningDriver) Delete(context.Context, provider.DeleteRequest) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.deleteCalls++
	if driver.deleteFailures > 0 {
		driver.deleteFailures--
		return errors.New("delete failed /private/native")
	}
	return nil
}

type hardeningSession struct {
	native       provider.NativeSession
	events       chan provider.Event
	child        provider.ManagedChild
	shutdownErr  error
	shutdownFunc func(context.Context) error
	shutdowns    atomic.Int32
}

func newHardeningSession(refValue string) *hardeningSession {
	ref, _ := provider.NewNativeSessionRef(refValue)
	return &hardeningSession{
		native: provider.NativeSession{Ref: ref, Provider: provider.NamePi, Model: "model", CreatedAt: testTime(), UpdatedAt: testTime()},
		events: make(chan provider.Event, 4096), child: &hardeningChild{},
	}
}
func (session *hardeningSession) NativeSession() provider.NativeSession { return session.native }
func (session *hardeningSession) Model() string                         { return session.native.Model }
func (*hardeningSession) History(context.Context, provider.HistoryRequest) (provider.HistoryPage, error) {
	return provider.HistoryPage{}, nil
}
func (*hardeningSession) Preflight(context.Context, provider.PreflightRequest) (provider.PreflightResult, error) {
	return provider.PreflightResult{}, nil
}
func (*hardeningSession) Submit(context.Context, provider.TurnRequest) (provider.AcceptedTurn, error) {
	return provider.AcceptedTurn{}, nil
}
func (session *hardeningSession) Events() <-chan provider.Event { return session.events }
func (*hardeningSession) Interrupt(context.Context, provider.AcceptedTurn) error {
	return nil
}
func (*hardeningSession) Reconcile(context.Context, provider.TurnReference) (provider.TurnState, error) {
	return provider.TurnUnknown, nil
}
func (session *hardeningSession) Child() provider.ManagedChild { return session.child }
func (session *hardeningSession) Shutdown(ctx context.Context) error {
	session.shutdowns.Add(1)
	if session.shutdownFunc != nil {
		return session.shutdownFunc(ctx)
	}
	return session.shutdownErr
}

type hardeningChild struct {
	mu           sync.Mutex
	order        []string
	terminateErr error
	killFailures int
	waitEntered  chan struct{}
	waitGate     chan struct{}
	waitOnce     sync.Once
}

func (*hardeningChild) Input() io.WriteCloser { return nopWriteCloser{} }
func (*hardeningChild) Output() io.Reader     { return nil }
func (*hardeningChild) Errors() io.Reader     { return nil }
func (child *hardeningChild) record(operation string) {
	child.mu.Lock()
	child.order = append(child.order, operation)
	child.mu.Unlock()
}
func (child *hardeningChild) Wait() error {
	child.record("wait")
	if child.waitEntered != nil {
		child.waitOnce.Do(func() { close(child.waitEntered) })
	}
	if child.waitGate != nil {
		<-child.waitGate
	}
	return errors.New("provider exited with status 1")
}
func (child *hardeningChild) Terminate() error {
	child.record("terminate")
	return child.terminateErr
}
func (child *hardeningChild) Kill() error {
	child.record("kill")
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.killFailures > 0 {
		child.killFailures--
		return errors.New("kill failed")
	}
	return nil
}

func hardeningMapping(t *testing.T, origin, capability string) (agentstate.Identity, agentstate.Mapping) {
	t.Helper()
	identity := agentstate.Identity{Origin: origin, Kind: agentstate.ResourceMarkdown, CapabilityID: capability, Provider: provider.NamePi}
	ref, err := agentstate.NativeSessionRef("sessions/resume")
	require.NoError(t, err)
	current := agentstate.Session{ConversationID: sequenceID(901), NativeSession: ref, CreatedAt: testTime(), UpdatedAt: testTime(), ProviderLabel: "pi", ModelLabel: "model"}
	return identity, agentstate.Mapping{SchemaVersion: agentstate.SchemaVersion, Identity: identity, Current: &current, Archives: []agentstate.Session{}, CreatedAt: testTime(), UpdatedAt: testTime()}
}

func TestResumePartialAndMalformedSessionsAreStoppedAfterTypedNilChecks(t *testing.T) {
	origin := "https://example.com"
	capability := sequenceID(902)
	_, mapping := hardeningMapping(t, origin, capability)

	t.Run("partial session and error", func(t *testing.T) {
		session := newHardeningSession("sessions/resume")
		driver := &hardeningDriver{resumeSession: session, resumeErr: errors.New("resume failed /private/ref")}
		broker, err := New(validLifecycleConfig(&hardeningState{mapping: &mapping}, driver, &lockedIDs{next: 910}))
		require.NoError(t, err)
		connection, connectErr := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(911), capability))
		require.Nil(t, connection)
		require.Error(t, connectErr)
		require.NotContains(t, connectErr.Error(), "/private/ref")
		require.EqualValues(t, 1, session.shutdowns.Load())
		require.NoError(t, broker.Close(context.Background()))
	})

	t.Run("typed nil success", func(t *testing.T) {
		var typedNil *hardeningSession
		driver := &hardeningDriver{resumeSession: typedNil}
		broker, err := New(validLifecycleConfig(&hardeningState{mapping: &mapping}, driver, &lockedIDs{next: 920}))
		require.NoError(t, err)
		connection, connectErr := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(921), capability))
		require.Nil(t, connection)
		require.Error(t, connectErr)
		require.NoError(t, broker.Close(context.Background()))
	})

	t.Run("typed nil child", func(t *testing.T) {
		session := newHardeningSession("sessions/resume")
		var typedNilChild *hardeningChild
		session.child = typedNilChild
		session.native.Model = "mismatched"
		driver := &hardeningDriver{resumeSession: session}
		broker, err := New(validLifecycleConfig(&hardeningState{mapping: &mapping}, driver, &lockedIDs{next: 930}))
		require.NoError(t, err)
		_, connectErr := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(931), capability))
		require.Error(t, connectErr)
		require.EqualValues(t, 1, session.shutdowns.Load())
		require.NoError(t, broker.Close(context.Background()))
	})
}

func TestCreateCompensationWaitsForProvenStopBeforeDeleteAndWorkspaceRemoval(t *testing.T) {
	state := &hardeningState{outcome: agentstate.CommitNotApplied}
	child := &hardeningChild{terminateErr: errors.New("terminate failed"), waitEntered: make(chan struct{}), waitGate: make(chan struct{})}
	session := newHardeningSession("sessions/create")
	session.shutdownErr = errors.New("graceful shutdown failed")
	session.child = child
	driver := &hardeningDriver{createSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 940}))
	require.NoError(t, err)

	type connectResult struct {
		connection any
		err        error
	}
	result := make(chan connectResult, 1)
	go func() {
		connection, connectErr := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(941), sequenceID(942)))
		result <- connectResult{connection: connection, err: connectErr}
	}()
	waitLifecycle(t, child.waitEntered)
	driver.mu.Lock()
	deleteCalls := driver.deleteCalls
	driver.mu.Unlock()
	state.mu.Lock()
	removeCalls := state.removeCalls
	state.mu.Unlock()
	require.Zero(t, deleteCalls)
	require.Zero(t, removeCalls)
	close(child.waitGate)
	completed := receiveLifecycle(t, result)
	require.Nil(t, completed.connection)
	require.Error(t, completed.err)
	driver.mu.Lock()
	require.Equal(t, 1, driver.deleteCalls)
	driver.mu.Unlock()
	state.mu.Lock()
	require.Equal(t, 1, state.removeCalls)
	state.mu.Unlock()
	child.mu.Lock()
	require.Equal(t, []string{"terminate", "kill", "wait"}, child.order)
	child.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))
}

func TestCreateCompensationRetainsRetryOwnershipAtEveryFailedStep(t *testing.T) {
	for _, test := range []struct {
		name             string
		shutdownErr      error
		killFailures     int
		deleteFailures   int
		removeFailures   int
		wantDeletesFirst int
		wantRemovesFirst int
	}{
		{name: "stop", shutdownErr: errors.New("shutdown failed"), killFailures: 1},
		{name: "delete", deleteFailures: 1, wantDeletesFirst: 1},
		{name: "workspace", removeFailures: 1, wantDeletesFirst: 1, wantRemovesFirst: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &hardeningState{outcome: agentstate.CommitNotApplied, removeFailures: test.removeFailures}
			child := &hardeningChild{killFailures: test.killFailures}
			session := newHardeningSession("sessions/create")
			session.shutdownErr = test.shutdownErr
			session.child = child
			driver := &hardeningDriver{createSession: session, deleteFailures: test.deleteFailures}
			broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 950}))
			require.NoError(t, err)
			_, connectErr := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(951), sequenceID(952)))
			require.Error(t, connectErr)
			driver.mu.Lock()
			require.Equal(t, test.wantDeletesFirst, driver.deleteCalls)
			driver.mu.Unlock()
			state.mu.Lock()
			require.Equal(t, test.wantRemovesFirst, state.removeCalls)
			state.mu.Unlock()
			broker.cleanupMu.Lock()
			require.Len(t, broker.cleanups, 1)
			broker.cleanupMu.Unlock()
			require.NoError(t, broker.Close(context.Background()))
			driver.mu.Lock()
			require.GreaterOrEqual(t, driver.deleteCalls, 1)
			driver.mu.Unlock()
			state.mu.Lock()
			require.GreaterOrEqual(t, state.removeCalls, 1)
			state.mu.Unlock()
			broker.cleanupMu.Lock()
			require.Empty(t, broker.cleanups)
			broker.cleanupMu.Unlock()
		})
	}
}

func TestUnresolvedCreateCleanupGatesReconnectForSameIdentity(t *testing.T) {
	state := &hardeningState{outcome: agentstate.CommitNotApplied}
	child := &hardeningChild{killFailures: 3}
	session := newHardeningSession("sessions/gated-create")
	session.shutdownErr = errors.New("shutdown failed")
	session.child = child
	driver := &hardeningDriver{createSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 955}))
	require.NoError(t, err)
	origin := "https://example.com"
	resource := sequenceID(956)

	_, err = broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(957), resource))
	require.Error(t, err)
	_, err = broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(958), resource))
	require.Error(t, err)
	driver.mu.Lock()
	require.Equal(t, 1, driver.createCalls)
	driver.mu.Unlock()

	child.mu.Lock()
	child.killFailures = 0
	child.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))
	driver.mu.Lock()
	require.Equal(t, 1, driver.deleteCalls)
	driver.mu.Unlock()
	state.mu.Lock()
	require.Equal(t, 1, state.removeCalls)
	state.mu.Unlock()
}

func TestUnresolvedResumeCleanupGatesReconnectForSameIdentity(t *testing.T) {
	origin := "https://example.com"
	resource := sequenceID(959)
	_, mapping := hardeningMapping(t, origin, resource)
	child := &hardeningChild{killFailures: 3}
	session := newHardeningSession("sessions/resume")
	session.shutdownErr = errors.New("shutdown failed")
	session.child = child
	driver := &hardeningDriver{resumeSession: session, resumeErr: errors.New("partial resume")}
	broker, err := New(validLifecycleConfig(&hardeningState{mapping: &mapping}, driver, &lockedIDs{next: 960}))
	require.NoError(t, err)

	_, err = broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(961), resource))
	require.Error(t, err)
	_, err = broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(962), resource))
	require.Error(t, err)
	driver.mu.Lock()
	require.Equal(t, 1, driver.resumeCalls)
	driver.mu.Unlock()

	child.mu.Lock()
	child.killFailures = 0
	child.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))
	driver.mu.Lock()
	require.Zero(t, driver.deleteCalls)
	driver.mu.Unlock()
}

func TestActorShutdownWorkerDrainsFinalUnbufferedProviderEvent(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	session := newHardeningSession("sessions/actor")
	session.events = make(chan provider.Event)
	session.shutdownFunc = func(ctx context.Context) error {
		select {
		case session.events <- provider.NewActivityEvent("", provider.ActivityStatus, "final"):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	driver := &hardeningDriver{createSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 960}))
	require.NoError(t, err)
	connection, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(961), sequenceID(962)))
	require.NoError(t, err)
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)
	closed := make(chan error, 1)
	go func() { closed <- broker.Close(context.Background()) }()
	require.NoError(t, receiveLifecycle(t, closed))
	require.EqualValues(t, 1, session.shutdowns.Load())
}

func TestFailedActorShutdownRetryNeverOverlaps(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	session := newHardeningSession("sessions/shutdown-retry")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	session.shutdownFunc = func(context.Context) error {
		current := active.Add(1)
		for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
		}
		call := calls.Add(1)
		if call == 1 {
			close(firstEntered)
			<-releaseFirst
			active.Add(-1)
			return errors.New("first shutdown failed")
		}
		active.Add(-1)
		return nil
	}
	driver := &hardeningDriver{createSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 965}))
	require.NoError(t, err)
	connection, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(966), sequenceID(967)))
	require.NoError(t, err)
	receiveLifecycle(t, connection.Events())
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() { firstResult <- broker.Close(context.Background()) }()
	waitLifecycle(t, firstEntered)
	go func() { secondResult <- broker.Close(context.Background()) }()
	require.EqualValues(t, 1, active.Load())
	close(releaseFirst)
	require.Error(t, receiveLifecycle(t, firstResult))
	require.NoError(t, receiveLifecycle(t, secondResult))
	require.EqualValues(t, 2, calls.Load())
	require.EqualValues(t, 1, maximum.Load())
}

func TestAttachmentByteBoundEvictsOnlySlowSubscriber(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	session := newHardeningSession("sessions/attachments")
	driver := &hardeningDriver{createSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 970}))
	require.NoError(t, err)
	slowRaw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(971), sequenceID(973)))
	require.NoError(t, err)
	fastRaw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(972), sequenceID(973)))
	require.NoError(t, err)
	slow := slowRaw.(*Connection)
	fast := fastRaw.(*Connection)
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, slow.Events()).Type)
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, fast.Events()).Type)

	const eventCount = 1100
	fastResult := make(chan int, 1)
	go func() {
		count := 0
		timer := time.NewTimer(lifecycleTestTimeout)
		defer timer.Stop()
		for count < eventCount {
			select {
			case _, open := <-fast.Events():
				if !open {
					fastResult <- -count - 1
					return
				}
				count++
			case <-timer.C:
				fastResult <- -eventCount - 1
				return
			}
		}
		fastResult <- count
	}()
	produced := make(chan bool, 1)
	go func() {
		timer := time.NewTimer(lifecycleTestTimeout)
		defer timer.Stop()
		for index := 0; index < eventCount; index++ {
			select {
			case session.events <- provider.NewActivityEvent("", provider.ActivityStatus, strings.Repeat("x", provider.MaxSummaryBytes)):
			case <-timer.C:
				produced <- false
				return
			}
		}
		produced <- true
	}()
	require.True(t, receiveLifecycle(t, produced))
	waitLifecycle(t, slow.attachment.detached)
	require.Equal(t, eventCount, receiveLifecycle(t, fastResult))
	require.NoError(t, broker.Close(context.Background()))
	waitLifecycle(t, fast.attachment.detached)
	_, slowOpen := receiveLifecycleOpen(t, slow.Events())
	_, fastOpen := receiveLifecycleOpen(t, fast.Events())
	require.False(t, slowOpen)
	require.False(t, fastOpen)
}

func TestLoadedMappingValidationAndFreshMappingProofAreExact(t *testing.T) {
	origin := "https://example.com"
	capability := sequenceID(980)
	identity, valid := hardeningMapping(t, origin, capability)
	invalidLoads := map[string]func(*agentstate.Mapping){
		"schema":   func(mapping *agentstate.Mapping) { mapping.SchemaVersion++ },
		"identity": func(mapping *agentstate.Mapping) { mapping.Identity.Origin = "https://other.example.com" },
		"timestamp": func(mapping *agentstate.Mapping) {
			mapping.CreatedAt = time.Time{}
		},
		"duplicate archive": func(mapping *agentstate.Mapping) {
			mapping.Archives = append(mapping.Archives, *mapping.Current)
		},
	}
	for name, mutate := range invalidLoads {
		t.Run(name, func(t *testing.T) {
			mapping := valid
			current := *valid.Current
			mapping.Current = &current
			mutate(&mapping)
			driver := &hardeningDriver{}
			broker, err := New(validLifecycleConfig(&hardeningState{mapping: &mapping}, driver, &lockedIDs{next: 981}))
			require.NoError(t, err)
			_, connectErr := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(982), capability))
			require.Error(t, connectErr)
			driver.mu.Lock()
			require.Zero(t, driver.resumeCalls)
			driver.mu.Unlock()
			require.NoError(t, broker.Close(context.Background()))
		})
	}

	fresh := *valid.Current
	fresh.CreatedAt = testTime()
	fresh.UpdatedAt = testTime()
	exact := agentstate.Mapping{SchemaVersion: agentstate.SchemaVersion, Identity: identity, Current: &fresh, Archives: []agentstate.Session{}, CreatedAt: fresh.CreatedAt, UpdatedAt: fresh.UpdatedAt}
	require.True(t, mappingHasCurrent(exact, identity, fresh))
	exactMutations := map[string]func(*agentstate.Mapping){
		"schema":          func(mapping *agentstate.Mapping) { mapping.SchemaVersion++ },
		"identity":        func(mapping *agentstate.Mapping) { mapping.Identity.Origin = "https://other.example.com" },
		"mapping created": func(mapping *agentstate.Mapping) { mapping.CreatedAt = mapping.CreatedAt.Add(time.Second) },
		"mapping updated": func(mapping *agentstate.Mapping) { mapping.UpdatedAt = mapping.UpdatedAt.Add(time.Second) },
		"nil archives":    func(mapping *agentstate.Mapping) { mapping.Archives = nil },
		"archives": func(mapping *agentstate.Mapping) {
			archive := *mapping.Current
			archive.ConversationID = sequenceID(983)
			archive.NativeSession, _ = agentstate.NativeSessionRef("sessions/archive")
			mapping.Archives = append(mapping.Archives, archive)
		},
		"conversation": func(mapping *agentstate.Mapping) { mapping.Current.ConversationID = sequenceID(984) },
		"native": func(mapping *agentstate.Mapping) {
			mapping.Current.NativeSession, _ = agentstate.NativeSessionRef("sessions/other")
		},
		"session created": func(mapping *agentstate.Mapping) {
			mapping.Current.CreatedAt = mapping.Current.CreatedAt.Add(time.Second)
		},
		"session updated": func(mapping *agentstate.Mapping) {
			mapping.Current.UpdatedAt = mapping.Current.UpdatedAt.Add(time.Second)
		},
		"provider label": func(mapping *agentstate.Mapping) { mapping.Current.ProviderLabel = "other" },
		"model label":    func(mapping *agentstate.Mapping) { mapping.Current.ModelLabel = "other" },
		"committed bookkeeping": func(mapping *agentstate.Mapping) {
			revision := agentstate.Revision{Digest: strings.Repeat("0", 64), Revision: agentstate.RevisionInitial, SourceUpdatedAt: testTime()}
			mapping.Current.Committed = &revision
		},
		"observed bookkeeping": func(mapping *agentstate.Mapping) {
			revision := agentstate.Revision{Digest: strings.Repeat("1", 64), Revision: agentstate.RevisionReplacement, SourceUpdatedAt: testTime()}
			mapping.Current.Observed = &revision
		},
		"prepared bookkeeping": func(mapping *agentstate.Mapping) {
			revision := agentstate.Revision{Digest: strings.Repeat("2", 64), Revision: agentstate.RevisionReplacement, SourceUpdatedAt: testTime()}
			mapping.Current.Observed = &revision
			mapping.Current.PreparedCommit = &agentstate.PreparedCommit{Revision: revision, TurnID: sequenceID(985), Phase: agentstate.CommitPrepared}
		},
	}
	for name, mutate := range exactMutations {
		t.Run("fresh "+name, func(t *testing.T) {
			mapping := exact
			current := fresh
			mapping.Current = &current
			mutate(&mapping)
			require.False(t, mappingHasCurrent(mapping, identity, fresh))
		})
	}
}

func TestSharedStartupIsBrokerOwnedNotCreatorCanceled(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	entered := make(chan struct{})
	gate := make(chan struct{})
	session := newHardeningSession("sessions/startup")
	driver := &hardeningDriver{createSession: session, createEntered: entered, createGate: gate}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 990}))
	require.NoError(t, err)

	type connectResult struct {
		connection localapi.Connection
		err        error
	}
	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorDone := make(chan connectResult, 1)
	go func() {
		raw, connectErr := broker.Connect(creatorCtx, "https://example.com", lifecycleConnect(sequenceID(991), sequenceID(993)))
		creatorDone <- connectResult{connection: raw, err: connectErr}
	}()
	waitLifecycle(t, entered)
	waiterDone := make(chan connectResult, 1)
	go func() {
		raw, connectErr := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(992), sequenceID(993)))
		waiterDone <- connectResult{connection: raw, err: connectErr}
	}()
	cancelCreator()
	creator := receiveLifecycle(t, creatorDone)
	require.ErrorIs(t, creator.err, context.Canceled)
	require.Nil(t, creator.connection)
	close(gate)
	waiter := receiveLifecycle(t, waiterDone)
	require.NoError(t, waiter.err)
	require.NotNil(t, waiter.connection)
	driver.mu.Lock()
	require.Equal(t, 1, driver.createCalls)
	driver.mu.Unlock()
	require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, waiter.connection.Events()).Type)
	require.NoError(t, broker.Close(context.Background()))
}

func TestAlreadyCanceledConnectionOperationsAreDeterministic(t *testing.T) {
	state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
	driver := &lifecycleDriver{}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1000}))
	require.NoError(t, err)
	raw, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(1001), sequenceID(1002)))
	require.NoError(t, err)
	connection := raw.(*Connection)
	receiveLifecycle(t, connection.Events())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conversationID := connection.ConversationID()
	command := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(1003), ClientID: sequenceID(1001), ConversationID: &conversationID, Type: agentprotocol.CommandNew, Payload: agentprotocol.EmptyPayload{}}
	_, err = connection.Command(ctx, command)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, connection.Close(ctx), context.Canceled)
	require.NoError(t, connection.Close(context.Background()))
	require.NoError(t, connection.Close(ctx))
	require.NoError(t, broker.Close(context.Background()))
}

var _ StateStore = (*hardeningState)(nil)
var _ provider.Driver = (*hardeningDriver)(nil)
var _ provider.Session = (*hardeningSession)(nil)
var _ provider.ManagedChild = (*hardeningChild)(nil)
