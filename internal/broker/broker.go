package broker

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/localapi"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

// StateStore is the complete durable surface needed by this broker phase.
// In particular, it cannot persist page context, queue items, or transcripts.
type StateStore interface {
	Load(agentstate.Identity) (agentstate.Mapping, error)
	Create(agentstate.Identity, agentstate.Session, time.Time) (agentstate.CommitOutcome, error)
	ObserveRevision(agentstate.Identity, agentstate.Revision, time.Time) (agentstate.CommitOutcome, error)
	PromotePrepared(agentstate.Identity, string, time.Time) (agentstate.CommitOutcome, error)
	ReconcilePrepared(agentstate.Identity, string, bool, time.Time) (agentstate.CommitOutcome, error)
	EnsureWorkspace(string) (string, error)
	RemoveWorkspace(string) error
}

type Config struct {
	State           StateStore
	Driver          provider.Driver
	IDs             common.IDGenerator
	Clock           common.Clock
	ShutdownTimeout time.Duration
}

type Broker struct {
	state           StateStore
	driver          provider.Driver
	ids             *serializedIDs
	clock           common.Clock
	shutdownTimeout time.Duration
	lifecycleCtx    context.Context
	cancelLifecycle context.CancelFunc

	// mu owns only admission state and the identity registry. No state, actor,
	// or provider operation is performed while it is held.
	mu       sync.Mutex
	stopping bool
	closed   bool
	registry map[agentstate.Identity]*conversationSlot

	cleanupMu sync.Mutex
	cleanups  map[*pendingCleanup]struct{}
	closeMu   sync.Mutex
}

type pendingCleanup struct {
	mu             sync.Mutex
	identity       agentstate.Identity
	session        provider.Session
	child          provider.ManagedChild
	events         <-chan provider.Event
	ref            provider.NativeSessionRef
	conversationID string
	processStopped bool
	deleteRequired bool
	deleteDone     bool
	workspaceOwned bool
	workspaceDone  bool
}

type conversationSlot struct {
	ready chan struct{}
	actor *conversation
	err   error
}

type serializedIDs struct {
	mu  sync.Mutex
	raw common.IDGenerator
}

func (ids *serializedIDs) NewID() (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	return ids.raw.NewID()
}

func New(config Config) (*Broker, error) {
	if common.IsNil(config.State) {
		return nil, errors.New("broker state store is required")
	}
	if common.IsNil(config.Driver) {
		return nil, errors.New("broker provider driver is required")
	}
	if common.IsNil(config.IDs) {
		return nil, errors.New("broker ID generator is required")
	}
	if common.IsNil(config.Clock) {
		return nil, errors.New("broker clock is required")
	}
	if config.ShutdownTimeout <= 0 {
		return nil, errors.New("broker shutdown timeout must be positive")
	}
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	return &Broker{
		state: config.State, driver: config.Driver,
		ids: &serializedIDs{raw: config.IDs}, clock: config.Clock,
		shutdownTimeout: config.ShutdownTimeout,
		lifecycleCtx:    lifecycleCtx, cancelLifecycle: cancelLifecycle,
		registry: make(map[agentstate.Identity]*conversationSlot),
		cleanups: make(map[*pendingCleanup]struct{}),
	}, nil
}

func (broker *Broker) Connect(ctx context.Context, origin string, command agentprotocol.Command) (localapi.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := agentprotocol.EncodeCommand(command); err != nil || command.Type != agentprotocol.CommandConnect {
		return nil, NewBrokerError(agentprotocol.ErrorInvalidCommand)
	}
	payload, ok := command.Payload.(agentprotocol.ConnectPayload)
	if !ok {
		return nil, NewBrokerError(agentprotocol.ErrorInvalidCommand)
	}
	identity, err := IdentityFromConnect(origin, payload, origin)
	if err != nil {
		return nil, NewBrokerError(agentprotocol.ErrorInvalidCommand)
	}

	broker.mu.Lock()
	if broker.stopping {
		broker.mu.Unlock()
		return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
	}
	slot := broker.registry[identity]
	creator := slot == nil
	if creator {
		slot = &conversationSlot{ready: make(chan struct{})}
		broker.registry[identity] = slot
	}
	broker.mu.Unlock()

	if creator {
		go broker.initializeSlot(slot, identity)
	}
	select {
	case <-slot.ready:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if slot.err != nil {
		return nil, slot.err
	}
	if slot.actor == nil {
		return nil, NewBrokerError(agentprotocol.ErrorBrokerUnavailable)
	}
	broker.mu.Lock()
	stopping := broker.stopping
	broker.mu.Unlock()
	if stopping {
		return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
	}
	connection, err := slot.actor.attach(ctx, command.ClientID, payload.ReplayAfter, payload.Resource, payload.ContextDigest)
	if err != nil {
		return nil, err
	}
	return connection, nil
}

func (broker *Broker) initializeSlot(slot *conversationSlot, identity agentstate.Identity) {
	actor, startErr := broker.startConversation(broker.lifecycleCtx, identity)
	if startErr != nil && broker.lifecycleCtx.Err() != nil {
		startErr = NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
	}
	broker.mu.Lock()
	slot.actor = actor
	slot.err = startErr
	if startErr != nil && broker.registry[identity] == slot {
		delete(broker.registry, identity)
	}
	close(slot.ready)
	broker.mu.Unlock()
}

func (broker *Broker) startConversation(ctx context.Context, identity agentstate.Identity) (*conversation, error) {
	if !broker.retryIdentityCleanups(ctx, identity) {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	mapping, err := broker.state.Load(identity)
	if err == nil {
		if mapping.Validate(identity) != nil {
			return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		return broker.resumeConversation(ctx, identity, mapping)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	return broker.createConversation(ctx, identity)
}

func (broker *Broker) readiness(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	readiness := broker.driver.Readiness(ctx)
	if failure, unavailable := MapReadiness(readiness); unavailable {
		return failure
	}
	return nil
}

func (broker *Broker) createConversation(ctx context.Context, identity agentstate.Identity) (*conversation, error) {
	if err := broker.readiness(ctx); err != nil {
		return nil, err
	}
	conversationID, err := broker.ids.NewID()
	if err != nil || common.ValidateID(conversationID) != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	workspace, err := broker.state.EnsureWorkspace(conversationID)
	if err != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	request := provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Workspace: workspace}
	if request.Validate() != nil {
		broker.cleanupWorkspace(identity, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	session, createErr := broker.driver.Create(ctx, request)
	if createErr != nil {
		if common.IsNil(session) {
			broker.cleanupWorkspace(identity, conversationID)
		} else {
			native := session.NativeSession()
			broker.compensateCreate(identity, session, native.Ref, conversationID)
		}
		if ctx.Err() != nil {
			return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
		}
		return nil, MapError(createErr)
	}
	native, err := validateProviderSession(session, nil)
	if err != nil {
		broker.compensateCreate(identity, session, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	ref, err := agentstate.NativeSessionRef(native.Ref.Value())
	if err != nil || ref != native.Ref {
		broker.compensateCreate(identity, session, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	at := broker.clock.Now().UTC()
	if at.IsZero() {
		broker.compensateCreate(identity, session, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	current := agentstate.Session{
		ConversationID: conversationID, NativeSession: ref,
		CreatedAt: at, UpdatedAt: at, ProviderLabel: string(native.Provider), ModelLabel: native.Model,
	}
	outcome, commitErr := broker.state.Create(identity, current, at)
	if outcome == agentstate.CommitApplied && commitErr == nil {
		mapping := agentstate.Mapping{SchemaVersion: agentstate.SchemaVersion, Identity: identity, Current: &current, Archives: []agentstate.Session{}, CreatedAt: at, UpdatedAt: at}
		return broker.newConversation(identity, mapping, session)
	}
	if outcome == agentstate.CommitApplied || outcome == agentstate.CommitUncertain {
		loaded, loadErr := broker.state.Load(identity)
		if loadErr == nil && loaded.Validate(identity) == nil && mappingHasCurrent(loaded, identity, current) {
			return broker.newConversation(identity, loaded, session)
		}
		if outcome == agentstate.CommitUncertain && errors.Is(loadErr, os.ErrNotExist) {
			broker.compensateCreate(identity, session, native.Ref, conversationID)
			return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		// A present mismatched record, or a failed inspection, cannot prove that
		// publishing was unapplied. Stop only the live process; deleting native
		// state or the workspace would be destructive.
		broker.retainStop(identity, session)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if outcome == agentstate.CommitNotApplied {
		broker.compensateCreate(identity, session, native.Ref, conversationID)
	}
	// Unknown outcomes cannot authorize destructive compensation. The live
	// process is still stopped so its unread event stream cannot be stranded.
	if outcome != agentstate.CommitNotApplied {
		broker.retainStop(identity, session)
	}
	return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
}

func (broker *Broker) resumeConversation(ctx context.Context, identity agentstate.Identity, mapping agentstate.Mapping) (*conversation, error) {
	if mapping.Validate(identity) != nil || mapping.Current == nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if prepared := mapping.Current.PreparedCommit; prepared != nil && prepared.Phase == agentstate.CommitAccepted {
		repaired, err := broker.promoteAccepted(identity, mapping)
		if err != nil {
			return nil, err
		}
		mapping = repaired
	}
	if _, err := agentstate.NativeSessionRef(mapping.Current.NativeSession.Value()); err != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if err := broker.readiness(ctx); err != nil {
		return nil, err
	}
	workspace, err := broker.state.EnsureWorkspace(mapping.Current.ConversationID)
	if err != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	request := provider.ResumeRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, NativeSession: mapping.Current.NativeSession, Workspace: workspace}
	if request.Validate() != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	session, err := broker.driver.Resume(ctx, request)
	if err != nil {
		if !common.IsNil(session) {
			broker.retainStop(identity, session)
		}
		if ctx.Err() != nil {
			return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
		}
		return nil, MapError(err)
	}
	if _, err := validateProviderSession(session, &mapping.Current.NativeSession); err != nil {
		broker.retainStop(identity, session)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	if mapping.Current.PreparedCommit != nil {
		return broker.reconcilePrepared(ctx, identity, mapping, session)
	}
	return broker.newConversation(identity, mapping, session)
}

func validateProviderSession(session provider.Session, expected *provider.NativeSessionRef) (provider.NativeSession, error) {
	if common.IsNil(session) {
		return provider.NativeSession{}, errors.New("nil provider session")
	}
	native := session.NativeSession()
	if native.Validate() != nil || session.Model() != native.Model || common.IsNil(session.Child()) || session.Events() == nil {
		return native, errors.New("invalid provider session")
	}
	ref, err := agentstate.NativeSessionRef(native.Ref.Value())
	if err != nil || ref != native.Ref {
		return native, errors.New("invalid provider native reference")
	}
	if expected != nil && native.Ref != *expected {
		return native, errors.New("provider native reference mismatch")
	}
	return native, nil
}

func mappingHasCurrent(mapping agentstate.Mapping, identity agentstate.Identity, current agentstate.Session) bool {
	if mapping.Validate(identity) != nil || mapping.SchemaVersion != agentstate.SchemaVersion || mapping.Current == nil || mapping.Archives == nil || len(mapping.Archives) != 0 {
		return false
	}
	loaded := mapping.Current
	return mapping.Identity == identity && mapping.CreatedAt == current.CreatedAt && mapping.UpdatedAt == current.UpdatedAt &&
		loaded.ConversationID == current.ConversationID && loaded.NativeSession == current.NativeSession &&
		loaded.CreatedAt == current.CreatedAt && loaded.UpdatedAt == current.UpdatedAt &&
		loaded.ProviderLabel == current.ProviderLabel && loaded.ModelLabel == current.ModelLabel &&
		loaded.Committed == nil && loaded.Observed == nil && loaded.PreparedCommit == nil &&
		current.Committed == nil && current.Observed == nil && current.PreparedCommit == nil
}

func (broker *Broker) cleanupWorkspace(identity agentstate.Identity, conversationID string) {
	cleanup := &pendingCleanup{identity: identity, conversationID: conversationID, processStopped: true, deleteDone: true, workspaceOwned: true}
	broker.retainCleanup(cleanup)
}

func (broker *Broker) retainStop(identity agentstate.Identity, session provider.Session) {
	if common.IsNil(session) {
		return
	}
	cleanup := &pendingCleanup{identity: identity, session: session, events: session.Events()}
	if child := session.Child(); !common.IsNil(child) {
		cleanup.child = child
	}
	broker.retainCleanup(cleanup)
}

func (broker *Broker) compensateCreate(identity agentstate.Identity, session provider.Session, ref provider.NativeSessionRef, conversationID string) {
	if common.IsNil(session) {
		broker.cleanupWorkspace(identity, conversationID)
		return
	}
	cleanup := &pendingCleanup{
		identity: identity, session: session, events: session.Events(), ref: ref, conversationID: conversationID,
		deleteRequired: true, workspaceOwned: true,
	}
	if child := session.Child(); !common.IsNil(child) {
		cleanup.child = child
	}
	broker.retainCleanup(cleanup)
}

func (broker *Broker) retryIdentityCleanups(ctx context.Context, identity agentstate.Identity) bool {
	broker.cleanupMu.Lock()
	pending := make([]*pendingCleanup, 0)
	for cleanup := range broker.cleanups {
		if cleanup.identity == identity {
			pending = append(pending, cleanup)
		}
	}
	broker.cleanupMu.Unlock()
	for _, cleanup := range pending {
		if !broker.runCleanup(ctx, cleanup) {
			return false
		}
		broker.cleanupMu.Lock()
		delete(broker.cleanups, cleanup)
		broker.cleanupMu.Unlock()
	}
	return true
}

func (broker *Broker) retainCleanup(cleanup *pendingCleanup) {
	broker.cleanupMu.Lock()
	broker.cleanups[cleanup] = struct{}{}
	broker.cleanupMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), broker.shutdownTimeout)
	complete := broker.runCleanup(ctx, cleanup)
	cancel()
	if complete {
		broker.cleanupMu.Lock()
		delete(broker.cleanups, cleanup)
		broker.cleanupMu.Unlock()
	}
}

func (broker *Broker) runCleanup(ctx context.Context, cleanup *pendingCleanup) bool {
	cleanup.mu.Lock()
	defer cleanup.mu.Unlock()
	if !cleanup.processStopped {
		cleanup.processStopped = stopPreActor(ctx, cleanup.session, cleanup.events, cleanup.child)
	}
	if !cleanup.processStopped {
		return false
	}
	if cleanup.deleteRequired && !cleanup.deleteDone {
		if !cleanup.ref.Valid() {
			return false
		}
		if err := broker.driver.Delete(ctx, provider.DeleteRequest{Provider: provider.NamePi, NativeSession: cleanup.ref}); err != nil {
			return false
		}
		cleanup.deleteDone = true
	}
	if cleanup.workspaceOwned && !cleanup.workspaceDone {
		if cleanup.deleteRequired && !cleanup.deleteDone {
			return false
		}
		if err := broker.state.RemoveWorkspace(cleanup.conversationID); err != nil {
			return false
		}
		cleanup.workspaceDone = true
	}
	return (!cleanup.deleteRequired || cleanup.deleteDone) && (!cleanup.workspaceOwned || cleanup.workspaceDone)
}

func stopPreActor(ctx context.Context, session provider.Session, events <-chan provider.Event, child provider.ManagedChild) bool {
	if common.IsNil(session) {
		return true
	}
	stopDrain := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case _, open := <-events:
				if !open {
					events = nil
				}
			case <-stopDrain:
				return
			}
		}
	}()
	shutdownErr := session.Shutdown(ctx)
	stopped := shutdownErr == nil
	if !stopped && !common.IsNil(child) {
		_ = child.Terminate()
		if child.Kill() == nil {
			_ = child.Wait()
			stopped = true
		}
	}
	close(stopDrain)
	<-drainDone
	return stopped
}

func (broker *Broker) newConversation(identity agentstate.Identity, mapping agentstate.Mapping, session provider.Session) (*conversation, error) {
	actor, err := newConversation(identity, mapping, session, broker.state, broker.ids, broker.clock, broker.shutdownTimeout)
	if err != nil {
		broker.retainStop(identity, session)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	return actor, nil
}

// Close stops admissions before synchronously asking every actor to detach its
// clients and shut down its provider session. Failed actors remain retryable.
func (broker *Broker) Close(ctx context.Context) error {
	if broker == nil {
		return nil
	}
	broker.closeMu.Lock()
	defer broker.closeMu.Unlock()

	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return nil
	}
	broker.stopping = true
	broker.cancelLifecycle()
	slots := make([]*conversationSlot, 0, len(broker.registry))
	for _, slot := range broker.registry {
		slots = append(slots, slot)
	}
	broker.mu.Unlock()

	closeCtx, cancel := context.WithTimeout(ctx, broker.shutdownTimeout)
	defer cancel()
	var errs []error
	actors := make([]*conversation, 0, len(slots))
	seen := make(map[*conversation]struct{}, len(slots))
	for _, slot := range slots {
		select {
		case <-slot.ready:
			if slot.actor != nil {
				if _, duplicate := seen[slot.actor]; !duplicate {
					seen[slot.actor] = struct{}{}
					actors = append(actors, slot.actor)
				}
			}
		case <-closeCtx.Done():
			errs = append(errs, closeCtx.Err())
		}
	}
	for _, actor := range actors {
		if err := actor.close(closeCtx); err != nil {
			errs = append(errs, err)
		}
	}
	broker.cleanupMu.Lock()
	cleanups := make([]*pendingCleanup, 0, len(broker.cleanups))
	for cleanup := range broker.cleanups {
		cleanups = append(cleanups, cleanup)
	}
	broker.cleanupMu.Unlock()
	for _, cleanup := range cleanups {
		if broker.runCleanup(closeCtx, cleanup) {
			broker.cleanupMu.Lock()
			delete(broker.cleanups, cleanup)
			broker.cleanupMu.Unlock()
		} else {
			errs = append(errs, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure))
		}
	}
	if len(errs) == 0 {
		broker.mu.Lock()
		broker.closed = true
		broker.mu.Unlock()
	}
	return errors.Join(errs...)
}

var _ StateStore = (*agentstate.Store)(nil)
var _ localapi.Backend = (*Broker)(nil)
