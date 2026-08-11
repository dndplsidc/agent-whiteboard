package broker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
	"github.com/stretchr/testify/require"
)

type manualTimerFactory struct {
	created chan *manualTimer
}

func newManualTimerFactory() *manualTimerFactory {
	return &manualTimerFactory{created: make(chan *manualTimer, 32)}
}
func (factory *manualTimerFactory) NewTimer(time.Duration) Timer {
	timer := &manualTimer{channel: make(chan time.Time, 1)}
	factory.created <- timer
	return timer
}

type manualTimer struct {
	mu      sync.Mutex
	channel chan time.Time
	stopped bool
}

func (timer *manualTimer) C() <-chan time.Time { return timer.channel }
func (timer *manualTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped
	timer.stopped = true
	return wasActive
}
func (timer *manualTimer) fire() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	if timer.stopped {
		return false
	}
	timer.channel <- testTime()
	return true
}
func (timer *manualTimer) isStopped() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	return timer.stopped
}

func TestIdleActorStopsAndLaterConnectResumesTheSameDurableSession(t *testing.T) {
	state := &lifecycleState{mappings: make(map[statepkg.Identity]statepkg.Mapping)}
	driver := &lifecycleDriver{}
	timers := newManualTimerFactory()
	config := validLifecycleConfig(state, driver, &lockedIDs{next: 10000})
	config.Timers = timers
	config.IdleTimeout = time.Hour
	broker, err := New(config)
	require.NoError(t, err)
	defer broker.Close(context.Background())
	origin := "https://example.com"
	resourceID := sequenceID(10001)
	clientID := sequenceID(10002)
	connected, err := broker.Connect(context.Background(), origin, lifecycleConnect(clientID, resourceID))
	require.NoError(t, err)
	connection := connected.(*Connection)
	initialSnapshot := receiveLifecycle(t, connection.Events())
	initialTimer := receiveLifecycle(t, timers.created)
	conversationID := connection.ConversationID()
	resync := resyncCommand(sequenceID(10003), clientID, conversationID, "")
	_, err = connection.Command(context.Background(), resync)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)
	require.True(t, initialTimer.isStopped())
	require.NoError(t, connection.Close(context.Background()))

	idleTimer := receiveLifecycle(t, timers.created)
	require.True(t, idleTimer.fire())
	select {
	case <-connection.actor.done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle actor did not stop")
	}
	require.EqualValues(t, 1, driver.sessions[0].shutdownCalls.Load())

	staleReplay := observationConnect(clientID, resourceID, strings.Repeat("0", 64), testResource(resourceID), initialSnapshot.EventID)
	_, err = broker.Connect(context.Background(), origin, staleReplay)
	requireBrokerCode(t, err, protocol.ErrorReplayWindowUnavailable)
	reconnected, err := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(10005), resourceID))
	require.NoError(t, err)
	defer reconnected.Close(context.Background())
	receiveLifecycle(t, reconnected.Events())
	driver.mu.Lock()
	require.Len(t, driver.creates, 1)
	require.Len(t, driver.resumes, 1)
	require.Equal(t, driver.sessions[0].NativeSession().Ref, driver.resumes[0].NativeSession)
	driver.mu.Unlock()
}

func TestCompletedActorWatcherCannotDeleteReplacementSlot(t *testing.T) {
	identity := statepkg.Identity{Origin: "https://example.com", Kind: statepkg.ResourceMarkdown, CapabilityID: sequenceID(10025), Provider: provider.NamePi}
	oldDone := make(chan struct{})
	close(oldDone)
	oldActor := &conversation{done: oldDone}
	oldSlot := &conversationSlot{actor: oldActor}
	newActor := &conversation{done: make(chan struct{})}
	newSlot := &conversationSlot{actor: newActor}
	broker := &Broker{registry: map[statepkg.Identity]*conversationSlot{identity: newSlot}}

	broker.watchActor(identity, oldSlot, oldActor)

	broker.mu.Lock()
	require.Same(t, newSlot, broker.registry[identity])
	broker.mu.Unlock()
}

func TestAttachRacingIdleExpiryEitherCancelsTimerOrTransparentlyResumes(t *testing.T) {
	state := &lifecycleState{mappings: make(map[statepkg.Identity]statepkg.Mapping)}
	driver := &lifecycleDriver{}
	timers := newManualTimerFactory()
	config := validLifecycleConfig(state, driver, &lockedIDs{next: 10030})
	config.Timers = timers
	broker, err := New(config)
	require.NoError(t, err)
	defer broker.Close(context.Background())
	origin := "https://example.com"
	resourceID := sequenceID(10031)
	first, err := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(10032), resourceID))
	require.NoError(t, err)
	receiveLifecycle(t, first.Events())
	receiveLifecycle(t, timers.created)
	require.NoError(t, first.Close(context.Background()))
	idleTimer := receiveLifecycle(t, timers.created)
	require.True(t, idleTimer.fire())

	second, err := broker.Connect(context.Background(), origin, lifecycleConnect(sequenceID(10033), resourceID))
	require.NoError(t, err)
	defer second.Close(context.Background())
	receiveLifecycle(t, second.Events())
	driver.mu.Lock()
	require.Len(t, driver.creates, 1)
	require.LessOrEqual(t, len(driver.resumes), 1)
	driver.mu.Unlock()
}

func TestCloseJoinsAnInFlightIdleShutdownAttempt(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(10006))
	state := &hardeningState{mapping: &mapping}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	shutdownEntered := make(chan struct{})
	shutdownGate := make(chan struct{})
	session.shutdownFunc = func(context.Context) error {
		close(shutdownEntered)
		<-shutdownGate
		return nil
	}
	timers := newManualTimerFactory()
	config := validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 10020})
	config.Timers = timers
	broker, err := New(config)
	require.NoError(t, err)
	resource := testResource(identity.CapabilityID)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(10007), identity.CapabilityID, strings.Repeat("c", 64), resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	receiveLifecycle(t, timers.created)
	require.NoError(t, connection.Close(context.Background()))
	idleTimer := receiveLifecycle(t, timers.created)
	require.True(t, idleTimer.fire())
	receiveLifecycle(t, shutdownEntered)

	closeResult := make(chan error, 1)
	go func() { closeResult <- broker.Close(context.Background()) }()
	receiveLifecycle(t, broker.lifecycleCtx.Done())
	select {
	case err := <-closeResult:
		t.Fatalf("broker close returned before the owned idle shutdown settled: %v", err)
	default:
	}
	close(shutdownGate)
	require.NoError(t, receiveLifecycle(t, closeResult))
	select {
	case <-connection.actor.done:
	case <-time.After(3 * time.Second):
		t.Fatal("joined idle shutdown did not close actor")
	}
	require.EqualValues(t, 1, session.shutdowns.Load())
}

func TestDisconnectedActiveAndQueuedTurnsDrainBeforeIdleTimerArms(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(10010))
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	observed := statepkg.Revision{Digest: page.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Observed = &observed
	state := &repairState{mapping: &mapping, promotions: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}}}
	session := newTurnSession(mapping.Current.NativeSession.Value())
	timers := newManualTimerFactory()
	config := validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: 10100})
	config.Timers = timers
	broker, err := New(config)
	require.NoError(t, err)
	defer broker.Close(context.Background())
	clientID := sequenceID(10011)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	receiveLifecycle(t, connection.Events())
	initialTimer := receiveLifecycle(t, timers.created)
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(10012)
	active := submitCommand(sequenceID(10013), clientID, conversationID, activeTurnID, sequenceID(10014), "active", &page)
	_, err = connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)
	queuedTurnID := sequenceID(10015)
	queued := submitCommand(sequenceID(10016), clientID, conversationID, queuedTurnID, sequenceID(10017), "queued", nil)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)
	require.NoError(t, connection.Close(context.Background()))
	require.True(t, initialTimer.isStopped())

	session.events <- provider.NewCompletionEvent(activeTurnID)
	require.Equal(t, queuedTurnID, receiveLifecycle(t, session.submitted).TurnID)
	session.events <- provider.NewCompletionEvent(queuedTurnID)
	idleTimer := receiveLifecycle(t, timers.created)
	require.True(t, idleTimer.fire())
	select {
	case <-connection.actor.done:
	case <-time.After(3 * time.Second):
		t.Fatal("drained disconnected actor did not stop")
	}
}

func resyncCommand(commandID, clientID, conversationID, after string) protocol.Command {
	return protocol.Command{APIVersion: protocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandResync, Payload: protocol.ResyncPayload{AfterEventID: after}}
}
