package broker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/localapi"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func observationConnect(client, resourceID, digest string, resource agentprotocol.Resource, replayAfter string) agentprotocol.Command {
	resource.ID = resourceID
	return agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(1200), ClientID: client,
		Type: agentprotocol.CommandConnect,
		Payload: agentprotocol.ConnectPayload{
			Provider: agentprotocol.ProviderPi, Resource: resource, ContextDigest: digest, ReplayAfter: replayAfter,
		},
	}
}

type observationConnectResult struct {
	connection localapi.Connection
	err        error
}

func TestAttachObservationOrderingConflictsMetadataAndReplay(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1201))
	state := &repairState{mapping: &mapping}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	session.events = make(chan provider.Event)
	driver := &hardeningDriver{resumeSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 1210}))
	require.NoError(t, err)
	defer broker.Close(context.Background())

	firstResource := testResource(identity.CapabilityID)
	firstDigest := strings.Repeat("0", 64)
	first, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1202), identity.CapabilityID, firstDigest, firstResource, ""))
	require.NoError(t, err)
	firstSnapshot := receiveLifecycle(t, first.Events())
	require.Equal(t, agentprotocol.EventSnapshot, firstSnapshot.Type)
	require.Equal(t, agentprotocol.ContextPending, firstSnapshot.Payload.(agentprotocol.SnapshotPayload).ContextState)

	entered := make(chan struct{})
	gate := make(chan struct{})
	state.mu.Lock()
	state.observeEntered = entered
	state.observeGate = gate
	state.mu.Unlock()
	newerResource := firstResource
	newerResource.UpdatedAt = firstResource.UpdatedAt.Add(2 * lifecycleTestTimeout)
	newerExpiry := newerResource.UpdatedAt.Add(lifecycleTestTimeout)
	newerResource.ExpiresAt = &newerExpiry
	newerDigest := strings.Repeat("1", 64)
	connected := make(chan observationConnectResult, 1)
	go func() {
		connection, connectErr := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1203), identity.CapabilityID, newerDigest, newerResource, ""))
		connected <- observationConnectResult{connection: connection, err: connectErr}
	}()
	waitLifecycle(t, entered)
	providerSent := make(chan struct{}, 1)
	go func() {
		session.events <- provider.NewActivityEvent("", provider.ActivityStatus, "after observed context")
		providerSent <- struct{}{}
	}()
	close(gate)
	secondResult := receiveLifecycle(t, connected)
	require.NoError(t, secondResult.err)
	secondSnapshot := receiveLifecycle(t, secondResult.connection.Events())
	require.Equal(t, agentprotocol.EventSnapshot, secondSnapshot.Type)
	require.Equal(t, agentprotocol.ContextPending, secondSnapshot.Payload.(agentprotocol.SnapshotPayload).ContextState)
	contextEvent := receiveLifecycle(t, first.Events())
	require.Equal(t, agentprotocol.EventContext, contextEvent.Type)
	require.Equal(t, newerDigest, contextEvent.Payload.(agentprotocol.ContextPayload).Digest)
	require.Equal(t, agentprotocol.ContextPending, contextEvent.Payload.(agentprotocol.ContextPayload).State)
	require.Equal(t, agentprotocol.EventActivity, receiveLifecycle(t, first.Events()).Type)
	waitLifecycle(t, providerSent)

	state.mu.Lock()
	require.Equal(t, 2, state.observeCalls)
	require.Equal(t, newerDigest, state.mapping.Current.Observed.Digest)
	require.Equal(t, agentstate.RevisionInitial, state.mapping.Current.Observed.Revision)
	state.observeEntered = nil
	state.observeGate = nil
	state.mu.Unlock()
	secondActor := secondResult.connection.(*Connection).actor
	require.Equal(t, newerResource, secondActor.resource)
	require.Equal(t, newerDigest, secondActor.contextDigest)

	require.NoError(t, secondResult.connection.Close(context.Background()))
	replayed, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1203), identity.CapabilityID, newerDigest, newerResource, secondSnapshot.EventID))
	require.NoError(t, err)
	require.Equal(t, agentprotocol.EventContext, receiveLifecycle(t, replayed.Events()).Type)

	olderSameDigest := newerResource
	olderSameDigest.UpdatedAt = firstResource.UpdatedAt
	olderSame, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1204), identity.CapabilityID, newerDigest, olderSameDigest, ""))
	require.NoError(t, err)
	require.Equal(t, agentprotocol.ContextPending, receiveLifecycle(t, olderSame.Events()).Payload.(agentprotocol.SnapshotPayload).ContextState)
	require.Equal(t, newerResource, secondActor.resource)
	state.mu.Lock()
	require.Equal(t, 2, state.observeCalls, "same digest must not be observed solely for a timestamp change")
	state.mu.Unlock()

	equalConflictResource := newerResource
	_, err = broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1205), identity.CapabilityID, strings.Repeat("2", 64), equalConflictResource, ""))
	requireBrokerCode(t, err, agentprotocol.ErrorBoardRevisionMalformed)
	olderConflictResource := newerResource
	olderConflictResource.UpdatedAt = newerResource.UpdatedAt.Add(-lifecycleTestTimeout)
	_, err = broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1206), identity.CapabilityID, strings.Repeat("3", 64), olderConflictResource, ""))
	requireBrokerCode(t, err, agentprotocol.ErrorBoardRevisionUnavailable)
}

func TestAttachCommittedDigestIsUnchangedWithoutTimestampObservation(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1220))
	resource := testResource(identity.CapabilityID)
	committed := agentstate.Revision{Digest: strings.Repeat("a", 64), Revision: agentstate.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Committed = &committed
	state := &repairState{mapping: &mapping}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	broker, err := New(validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 1221}))
	require.NoError(t, err)
	defer broker.Close(context.Background())

	newerMetadata := resource
	newerMetadata.UpdatedAt = resource.UpdatedAt.Add(lifecycleTestTimeout)
	connection, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1222), identity.CapabilityID, committed.Digest, newerMetadata, ""))
	require.NoError(t, err)
	snapshot := receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.SnapshotPayload)
	require.Equal(t, agentprotocol.ContextUnchanged, snapshot.ContextState)
	state.mu.Lock()
	require.Zero(t, state.observeCalls)
	state.mu.Unlock()
	require.Equal(t, newerMetadata, connection.(*Connection).actor.resource)
}

func TestAttachUncertainObservationAcceptsOnlyExactLoadedObservedMapping(t *testing.T) {
	failure := errors.New("directory sync outcome unavailable")
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1230))
	state := &repairState{
		mapping:      &mapping,
		observations: []repairMutation{{outcome: agentstate.CommitUncertain, err: failure, apply: true}},
	}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	broker, err := New(validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 1231}))
	require.NoError(t, err)
	defer broker.Close(context.Background())

	resource := testResource(identity.CapabilityID)
	digest := strings.Repeat("b", 64)
	connection, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1232), identity.CapabilityID, digest, resource, ""))
	require.NoError(t, err)
	snapshot := receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.SnapshotPayload)
	require.Equal(t, agentprotocol.ContextPending, snapshot.ContextState)
	state.mu.Lock()
	require.Equal(t, 1, state.observeCalls)
	require.Equal(t, digest, state.mapping.Current.Observed.Digest)
	state.mu.Unlock()
}

func TestObservationActorAndReplayHaveNoPageContentStorage(t *testing.T) {
	actorType := reflect.TypeOf(conversation{})
	pageContextTypes := map[reflect.Type]struct{}{
		reflect.TypeOf(agentprotocol.PageContext{}): {},
		reflect.TypeOf(provider.PageContext{}):      {},
		reflect.TypeOf([]byte(nil)):                 {},
	}
	for index := 0; index < actorType.NumField(); index++ {
		field := actorType.Field(index)
		_, forbidden := pageContextTypes[field.Type]
		require.False(t, forbidden, field.Name)
	}

	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(1240))
	state := &repairState{mapping: &mapping}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	broker, err := New(validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 1241}))
	require.NoError(t, err)
	defer broker.Close(context.Background())
	resource := testResource(identity.CapabilityID)
	connection, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(1242), identity.CapabilityID, strings.Repeat("c", 64), resource, ""))
	require.NoError(t, err)
	receiveLifecycle(t, connection.Events())
	actor := connection.(*Connection).actor
	require.Equal(t, resource, actor.resource)
	require.Equal(t, strings.Repeat("c", 64), actor.contextDigest)
	for _, entry := range actor.replay.entries {
		require.NotEqual(t, reflect.TypeOf(agentprotocol.PageContext{}), reflect.TypeOf(entry.Event.Payload))
		require.NotEqual(t, reflect.TypeOf(provider.PageContext{}), reflect.TypeOf(entry.Event.Payload))
	}
}

func requireBrokerCode(t *testing.T, err error, code agentprotocol.BrowserErrorCode) {
	t.Helper()
	var brokerErr BrokerError
	require.ErrorAs(t, err, &brokerErr)
	require.Equal(t, code, brokerErr.Code())
}
