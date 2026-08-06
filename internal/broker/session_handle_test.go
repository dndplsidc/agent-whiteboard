package broker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

type countingSession struct {
	*hardeningSession
	nativeCalls atomic.Int32
	eventsCalls atomic.Int32
	childCalls  atomic.Int32
}

func (session *countingSession) NativeSession() provider.NativeSession {
	session.nativeCalls.Add(1)
	return session.hardeningSession.native
}

func (session *countingSession) Events() <-chan provider.Event {
	session.eventsCalls.Add(1)
	return session.hardeningSession.events
}

func (session *countingSession) Child() provider.ManagedChild {
	session.childCalls.Add(1)
	return session.hardeningSession.child
}

func TestSessionHandleCapturesResourcesOnceForStartupAndReconciliation(t *testing.T) {
	t.Run("startup actor", func(t *testing.T) {
		session := &countingSession{hardeningSession: newHardeningSession("sessions/create")}
		state := &lifecycleState{mappings: make(map[agentstate.Identity]agentstate.Mapping)}
		broker, err := New(validLifecycleConfig(state, &hardeningDriver{createSession: session}, &lockedIDs{next: 1200}))
		require.NoError(t, err)

		connection, err := broker.Connect(context.Background(), "https://example.com", lifecycleConnect(sequenceID(1201), sequenceID(1202)))
		require.NoError(t, err)
		require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
		require.NoError(t, broker.Close(context.Background()))
		require.EqualValues(t, 1, session.nativeCalls.Load())
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
	})

	t.Run("prepared reconciliation", func(t *testing.T) {
		identity, mapping, _ := preparedRepairMapping(t, agentstate.CommitPrepared)
		state := &repairState{mapping: &mapping, reconciliations: []repairMutation{{outcome: agentstate.CommitApplied, apply: true}}}
		session := &countingSession{hardeningSession: newHardeningSession(mapping.Current.NativeSession.Value())}
		session.reconcileState = provider.TurnAccepted
		broker, err := New(validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 1210}))
		require.NoError(t, err)

		connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(1211), identity.CapabilityID))
		require.NoError(t, err)
		require.Equal(t, agentprotocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
		require.NoError(t, broker.Close(context.Background()))
		require.EqualValues(t, 1, session.nativeCalls.Load())
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
	})
}

func TestSessionHandleCapturesResourcesOnceForInvalidAndPartialCleanup(t *testing.T) {
	origin := "https://example.com"
	capability := sequenceID(1220)

	t.Run("invalid captured child", func(t *testing.T) {
		_, mapping := hardeningMapping(t, origin, capability)
		session := &countingSession{hardeningSession: newHardeningSession(mapping.Current.NativeSession.Value())}
		var typedNilChild *hardeningChild
		session.hardeningSession.child = typedNilChild
		broker, err := New(validLifecycleConfig(&hardeningState{mapping: &mapping}, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 1221}))
		require.NoError(t, err)

		connection, err := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(1222), capability))
		require.Nil(t, connection)
		require.Error(t, err)
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
		require.NoError(t, broker.Close(context.Background()))
		require.EqualValues(t, 1, session.nativeCalls.Load())
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
	})

	t.Run("partial session with error", func(t *testing.T) {
		_, mapping := hardeningMapping(t, origin, capability)
		session := &countingSession{hardeningSession: newHardeningSession(mapping.Current.NativeSession.Value())}
		broker, err := New(validLifecycleConfig(&hardeningState{mapping: &mapping}, &hardeningDriver{
			resumeSession: session,
			resumeErr:     errors.New("partial resume"),
		}, &lockedIDs{next: 1230}))
		require.NoError(t, err)

		connection, err := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(1231), capability))
		require.Nil(t, connection)
		require.Error(t, err)
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
		require.NoError(t, broker.Close(context.Background()))
		require.EqualValues(t, 1, session.nativeCalls.Load())
		require.EqualValues(t, 1, session.eventsCalls.Load())
		require.EqualValues(t, 1, session.childCalls.Load())
	})
}

func TestValidateProviderSessionRejectsProviderIdentityMismatch(t *testing.T) {
	session := newHardeningSession("sessions/provider-mismatch")
	require.Equal(t, provider.NamePi, session.native.Provider)
	_, err := validateProviderSession(captureSession(session), nil, provider.NameCodex)
	require.Error(t, err)
}

var _ provider.Session = (*countingSession)(nil)
