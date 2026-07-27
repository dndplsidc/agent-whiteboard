package broker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

type repairMutation struct {
	outcome agentstate.CommitOutcome
	err     error
	apply   bool
}

type repairState struct {
	mu               sync.Mutex
	mapping          *agentstate.Mapping
	promotions       []repairMutation
	reconciliations  []repairMutation
	observations     []repairMutation
	acknowledgements []repairMutation
	observeEntered   chan struct{}
	observeGate      chan struct{}
	observeEnterOnce sync.Once
	promoteCalls     int
	reconcileCalls   int
	observeCalls     int
	acknowledgeCalls int
	loadCalls        int
}

func (state *repairState) Load(identity agentstate.Identity) (agentstate.Mapping, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.loadCalls++
	if state.mapping == nil {
		return agentstate.Mapping{}, os.ErrNotExist
	}
	return cloneMapping(*state.mapping), nil
}

func (*repairState) Create(agentstate.Identity, agentstate.Session, time.Time) (agentstate.CommitOutcome, error) {
	return agentstate.CommitNotApplied, errors.New("create is not available")
}

func (state *repairState) ObserveRevision(_ agentstate.Identity, revision agentstate.Revision, at time.Time) (agentstate.CommitOutcome, error) {
	state.mu.Lock()
	state.observeCalls++
	entered := state.observeEntered
	gate := state.observeGate
	step := repairMutation{outcome: agentstate.CommitApplied, apply: true}
	if len(state.observations) != 0 {
		step = state.observations[0]
		state.observations = state.observations[1:]
	}
	state.mu.Unlock()
	if entered != nil {
		state.observeEnterOnce.Do(func() { close(entered) })
	}
	if gate != nil {
		<-gate
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.mapping == nil || state.mapping.Current == nil || state.mapping.Current.PreparedCommit != nil {
		return agentstate.CommitNotApplied, errors.New("observe rejected")
	}
	if step.apply {
		updated := observedMapping(*state.mapping, revision, at)
		state.mapping = &updated
	}
	return step.outcome, step.err
}

func (state *repairState) AcknowledgeCommittedRevision(_ agentstate.Identity, revision agentstate.Revision, at time.Time) (agentstate.CommitOutcome, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.acknowledgeCalls++
	step := repairMutation{outcome: agentstate.CommitApplied, apply: true}
	if len(state.acknowledgements) != 0 {
		step = state.acknowledgements[0]
		state.acknowledgements = state.acknowledgements[1:]
	}
	if step.apply {
		updated := acknowledgedMapping(*state.mapping, revision, at)
		state.mapping = &updated
	}
	return step.outcome, step.err
}

func (state *repairState) PromotePrepared(_ agentstate.Identity, _ string, at time.Time) (agentstate.CommitOutcome, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.promoteCalls++
	step := repairMutation{outcome: agentstate.CommitNotApplied, err: errors.New("promotion rejected")}
	if len(state.promotions) != 0 {
		step = state.promotions[0]
		state.promotions = state.promotions[1:]
	}
	if step.apply {
		updated := promotedMapping(*state.mapping, at)
		state.mapping = &updated
	}
	return step.outcome, step.err
}

func (state *repairState) ReconcilePrepared(_ agentstate.Identity, _ string, accepted bool, at time.Time) (agentstate.CommitOutcome, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.reconcileCalls++
	step := repairMutation{outcome: agentstate.CommitNotApplied, err: errors.New("reconciliation rejected")}
	if len(state.reconciliations) != 0 {
		step = state.reconciliations[0]
		state.reconciliations = state.reconciliations[1:]
	}
	if step.apply {
		updated := reconciledMapping(*state.mapping, accepted, at)
		state.mapping = &updated
	}
	return step.outcome, step.err
}

func (*repairState) EnsureWorkspace(id string) (string, error) {
	return "/tmp/agent-whiteboard-test/" + id, nil
}
func (*repairState) RemoveWorkspace(string) error { return nil }

func preparedRepairMapping(t *testing.T, phase agentstate.CommitPhase) (agentstate.Identity, agentstate.Mapping, agentstate.Revision) {
	t.Helper()
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1100))
	revision := agentstate.Revision{
		Digest: strings.Repeat("0", 64), Revision: agentstate.RevisionInitial,
		SourceUpdatedAt: testResource(identity.CapabilityID).UpdatedAt,
	}
	observed := revision
	mapping.Current.Observed = &observed
	mapping.Current.PreparedCommit = &agentstate.PreparedCommit{Revision: revision, TurnID: sequenceID(1101), Phase: phase}
	return identity, mapping, revision
}

func TestStartupPromotesAcceptedPreparedCommitWithExactOutcomeClassification(t *testing.T) {
	failure := errors.New("durability result unavailable")
	tests := []struct {
		name      string
		steps     []repairMutation
		wantCalls int
	}{
		{name: "applied", steps: []repairMutation{{outcome: agentstate.CommitApplied, apply: true}}, wantCalls: 1},
		{name: "applied with error loaded target", steps: []repairMutation{{outcome: agentstate.CommitApplied, err: failure, apply: true}}, wantCalls: 1},
		{name: "uncertain loaded target", steps: []repairMutation{{outcome: agentstate.CommitUncertain, err: failure, apply: true}}, wantCalls: 1},
		{name: "not applied exact precondition retry", steps: []repairMutation{{outcome: agentstate.CommitNotApplied, err: failure}, {outcome: agentstate.CommitApplied, apply: true}}, wantCalls: 2},
		{name: "uncertain exact precondition retry", steps: []repairMutation{{outcome: agentstate.CommitUncertain, err: failure}, {outcome: agentstate.CommitApplied, apply: true}}, wantCalls: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, mapping, revision := preparedRepairMapping(t, agentstate.CommitAccepted)
			state := &repairState{mapping: &mapping, promotions: append([]repairMutation(nil), test.steps...)}
			session := newHardeningSession(mapping.Current.NativeSession.Value())
			driver := &hardeningDriver{resumeSession: session}
			broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1110}))
			require.NoError(t, err)
			connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1111), identity.CapabilityID))
			require.NoError(t, err)
			snapshot := receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.SnapshotPayload)
			require.Equal(t, agentprotocol.ContextUnchanged, snapshot.ContextState)
			state.mu.Lock()
			require.Equal(t, test.wantCalls, state.promoteCalls)
			require.Equal(t, revision, *state.mapping.Current.Committed)
			require.Nil(t, state.mapping.Current.PreparedCommit)
			state.mu.Unlock()
			driver.mu.Lock()
			require.Equal(t, 1, driver.resumeCalls)
			driver.mu.Unlock()
			require.Zero(t, session.submissions.Load())
			require.NoError(t, broker.Close(context.Background()))
		})
	}
}

func TestStartupPreparedReconciliationDrainsUnbufferedEventAndClassifiesMutation(t *testing.T) {
	failure := errors.New("durability result unavailable")
	tests := []struct {
		name          string
		providerState provider.TurnState
		step          repairMutation
		wantContext   agentprotocol.ContextState
		wantCommitted bool
	}{
		{name: "not accepted applied", providerState: provider.TurnNotAccepted, step: repairMutation{outcome: agentstate.CommitApplied, apply: true}, wantContext: agentprotocol.ContextPending},
		{name: "accepted applied", providerState: provider.TurnAccepted, step: repairMutation{outcome: agentstate.CommitApplied, apply: true}, wantContext: agentprotocol.ContextUnchanged, wantCommitted: true},
		{name: "running uncertain loaded target", providerState: provider.TurnRunning, step: repairMutation{outcome: agentstate.CommitUncertain, err: failure, apply: true}, wantContext: agentprotocol.ContextUnchanged, wantCommitted: true},
		{name: "completed applied with error loaded target", providerState: provider.TurnCompleted, step: repairMutation{outcome: agentstate.CommitApplied, err: failure, apply: true}, wantContext: agentprotocol.ContextUnchanged, wantCommitted: true},
		{name: "interrupted not applied but exact loaded target", providerState: provider.TurnInterrupted, step: repairMutation{outcome: agentstate.CommitNotApplied, err: failure, apply: true}, wantContext: agentprotocol.ContextUnchanged, wantCommitted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, mapping, revision := preparedRepairMapping(t, agentstate.CommitPrepared)
			state := &repairState{mapping: &mapping, reconciliations: []repairMutation{test.step}}
			session := newHardeningSession(mapping.Current.NativeSession.Value())
			session.events = make(chan provider.Event)
			event := provider.NewActivityEvent("", provider.ActivityStatus, "discarded during repair")
			session.reconcileEvent = &event
			session.reconcileState = test.providerState
			driver := &hardeningDriver{resumeSession: session}
			broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1120}))
			require.NoError(t, err)
			connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1121), identity.CapabilityID))
			require.NoError(t, err)
			snapshot := receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.SnapshotPayload)
			require.Equal(t, test.wantContext, snapshot.ContextState)
			require.EqualValues(t, 1, session.reconciliations.Load())
			require.Zero(t, session.submissions.Load())
			state.mu.Lock()
			require.Equal(t, 1, state.reconcileCalls)
			require.Nil(t, state.mapping.Current.PreparedCommit)
			if test.wantCommitted {
				require.Equal(t, revision, *state.mapping.Current.Committed)
			} else {
				require.Equal(t, revision, *state.mapping.Current.Observed)
			}
			state.mu.Unlock()
			require.NoError(t, broker.Close(context.Background()))
		})
	}
}

func TestStartupPreparedMutationWithoutExactAppliedLoadFailsWithoutRetry(t *testing.T) {
	identity, mapping, _ := preparedRepairMapping(t, agentstate.CommitPrepared)
	state := &repairState{
		mapping:         &mapping,
		reconciliations: []repairMutation{{outcome: agentstate.CommitUncertain, err: errors.New("mutation uncertain")}},
	}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	session.reconcileState = provider.TurnAccepted
	broker, err := New(validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 1125}))
	require.NoError(t, err)

	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1126), identity.CapabilityID))
	require.Nil(t, connection)
	requireBrokerCode(t, err, agentprotocol.ErrorStateRepairFailed)
	require.EqualValues(t, 1, session.reconciliations.Load())
	require.Zero(t, session.submissions.Load())
	state.mu.Lock()
	require.Equal(t, 1, state.reconcileCalls)
	require.NotNil(t, state.mapping.Current.PreparedCommit)
	state.mu.Unlock()
	require.EqualValues(t, 1, session.shutdowns.Load())
	require.NoError(t, broker.Close(context.Background()))
}

func TestStartupAcceptedPromotionStopsAfterOneExactPreconditionRetry(t *testing.T) {
	identity, mapping, _ := preparedRepairMapping(t, agentstate.CommitAccepted)
	failure := errors.New("promotion not applied")
	state := &repairState{mapping: &mapping, promotions: []repairMutation{
		{outcome: agentstate.CommitUncertain, err: failure},
		{outcome: agentstate.CommitNotApplied, err: failure},
	}}
	driver := &hardeningDriver{}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1127}))
	require.NoError(t, err)

	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1128), identity.CapabilityID))
	require.Nil(t, connection)
	requireBrokerCode(t, err, agentprotocol.ErrorStateRepairFailed)
	state.mu.Lock()
	require.Equal(t, 2, state.promoteCalls)
	require.Equal(t, agentstate.CommitAccepted, state.mapping.Current.PreparedCommit.Phase)
	state.mu.Unlock()
	driver.mu.Lock()
	require.Zero(t, driver.resumeCalls)
	driver.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))
}

func TestStartupReconciliationErrorsPreserveFrozenTaxonomy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want agentprotocol.BrowserErrorCode
	}{
		{name: "acceptance unknown", err: provider.NewProviderError(provider.ErrorAcceptanceUnknown), want: agentprotocol.ErrorAcceptanceOutcomeUnknown},
		{name: "native session missing", err: provider.NewProviderError(provider.ErrorNativeSessionMissing), want: agentprotocol.ErrorNativeSessionMissing},
		{name: "malformed stream", err: provider.NewProviderError(provider.ErrorMalformedStream), want: agentprotocol.ErrorProviderMalformedStream},
		{name: "child exited", err: provider.NewProviderError(provider.ErrorChildExited), want: agentprotocol.ErrorProviderCrashed},
		{name: "untyped protocol failure", err: errors.New("native /private/error"), want: agentprotocol.ErrorProviderProtocolFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, mapping, _ := preparedRepairMapping(t, agentstate.CommitPrepared)
			state := &repairState{mapping: &mapping}
			session := newHardeningSession(mapping.Current.NativeSession.Value())
			session.reconcileErr = test.err
			driver := &hardeningDriver{resumeSession: session}
			broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1129}))
			require.NoError(t, err)
			connection, connectErr := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1130), identity.CapabilityID))
			require.Nil(t, connection)
			requireBrokerCode(t, connectErr, test.want)
			require.NotContains(t, connectErr.Error(), "/private/error")
			state.mu.Lock()
			require.NotNil(t, state.mapping.Current.PreparedCommit)
			require.Zero(t, state.reconcileCalls)
			state.mu.Unlock()
			require.Zero(t, session.submissions.Load())
			require.EqualValues(t, 1, session.shutdowns.Load())
			require.NoError(t, broker.Close(context.Background()))
		})
	}
}

func TestStartupTurnUnknownRetainsPreparedStateAndNeverSubmits(t *testing.T) {
	identity, mapping, _ := preparedRepairMapping(t, agentstate.CommitPrepared)
	state := &repairState{mapping: &mapping}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	session.reconcileState = provider.TurnUnknown
	driver := &hardeningDriver{resumeSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1130}))
	require.NoError(t, err)

	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1131), identity.CapabilityID))
	require.Nil(t, connection)
	var brokerErr BrokerError
	require.ErrorAs(t, err, &brokerErr)
	require.Equal(t, agentprotocol.ErrorAcceptanceOutcomeUnknown, brokerErr.Code())
	require.EqualValues(t, 1, session.reconciliations.Load())
	require.Zero(t, session.submissions.Load())
	state.mu.Lock()
	require.Zero(t, state.reconcileCalls)
	require.NotNil(t, state.mapping.Current.PreparedCommit)
	state.mu.Unlock()
	require.EqualValues(t, 1, session.shutdowns.Load())
	require.NoError(t, broker.Close(context.Background()))
}

var _ StateStore = (*repairState)(nil)
