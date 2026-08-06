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
	AcknowledgeCommittedRevision(agentstate.Identity, agentstate.Revision, time.Time) (agentstate.CommitOutcome, error)
	PrepareCommit(agentstate.Identity, agentstate.Revision, string, time.Time) (agentstate.CommitOutcome, error)
	MarkPreparedAccepted(agentstate.Identity, string, time.Time) (agentstate.CommitOutcome, error)
	PromotePrepared(agentstate.Identity, string, time.Time) (agentstate.CommitOutcome, error)
	ReconcilePrepared(agentstate.Identity, string, bool, time.Time) (agentstate.CommitOutcome, error)
	EnsureWorkspace(string) (string, error)
	RemoveWorkspace(string) error
}

type Config struct {
	State   StateStore
	Drivers provider.Registry
	// Driver is retained as a Pi-only compatibility seam for embedders while
	// composition migrates to Drivers. Supplying both is invalid.
	Driver          provider.Driver
	IDs             common.IDGenerator
	Clock           common.Clock
	Timers          TimerFactory
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

type Broker struct {
	state           StateStore
	drivers         provider.Registry
	ids             *serializedIDs
	clock           common.Clock
	timers          TimerFactory
	idleTimeout     time.Duration
	shutdownTimeout time.Duration
	lifecycleCtx    context.Context
	cancelLifecycle context.CancelFunc

	// mu owns only admission state and the identity registry. No state, actor,
	// or provider operation is performed while it is held.
	mu       sync.Mutex
	stopping bool
	closed   bool
	registry map[agentstate.Identity]*conversationSlot
	orphans  map[*conversation]struct{}

	cleanupMu    sync.Mutex
	cleanups     map[*pendingCleanup]struct{}
	closeMu      sync.Mutex
	handoffMu    sync.Mutex
	handoffCount int
	handoffIdle  chan struct{}
}

// sessionHandle owns the resources exposed by one provider session. Accessors
// that return process-owned resources are called only while capturing the
// handle; all later broker paths use these captured values.
type sessionHandle struct {
	session provider.Session
	native  provider.NativeSession
	events  <-chan provider.Event
	child   provider.ManagedChild
}

func captureSession(session provider.Session) *sessionHandle {
	if common.IsNil(session) {
		return nil
	}
	return &sessionHandle{
		session: session,
		native:  session.NativeSession(),
		events:  session.Events(),
		child:   session.Child(),
	}
}

type pendingCleanup struct {
	mu             sync.Mutex
	running        bool
	done           chan struct{}
	identity       agentstate.Identity
	handle         *sessionHandle
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
	registry := config.Drivers
	if !common.IsNil(config.Driver) {
		if len(registry.Names()) != 0 {
			return nil, errors.New("broker provider configuration is ambiguous")
		}
		var err error
		registry, err = provider.NewRegistry(map[provider.Name]provider.Driver{provider.NamePi: config.Driver})
		if err != nil {
			return nil, errors.New("broker provider driver is required")
		}
	}
	if len(registry.Names()) == 0 {
		return nil, errors.New("broker provider driver is required")
	}
	if common.IsNil(config.IDs) {
		return nil, errors.New("broker ID generator is required")
	}
	if common.IsNil(config.Clock) {
		return nil, errors.New("broker clock is required")
	}
	if common.IsNil(config.Timers) {
		return nil, errors.New("broker timer factory is required")
	}
	if config.IdleTimeout <= 0 {
		return nil, errors.New("broker idle timeout must be positive")
	}
	if config.ShutdownTimeout <= 0 {
		return nil, errors.New("broker shutdown timeout must be positive")
	}
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	return &Broker{
		state: config.State, drivers: registry,
		ids: &serializedIDs{raw: config.IDs}, clock: config.Clock,
		timers: config.Timers, idleTimeout: config.IdleTimeout,
		shutdownTimeout: config.ShutdownTimeout,
		lifecycleCtx:    lifecycleCtx, cancelLifecycle: cancelLifecycle,
		registry: make(map[agentstate.Identity]*conversationSlot),
		orphans:  make(map[*conversation]struct{}),
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

	for {
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
		connection, attachErr := slot.actor.attach(ctx, command.ClientID, payload.ReplayAfter, payload.Resource, payload.ContextDigest)
		if !errors.Is(attachErr, errActorRetired) {
			if attachErr != nil {
				return nil, attachErr
			}
			return connection, nil
		}
		select {
		case <-slot.actor.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		broker.mu.Lock()
		if broker.registry[identity] == slot {
			delete(broker.registry, identity)
		}
		broker.mu.Unlock()
	}

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
	if actor != nil {
		actor.startHandoff = func(request handoffRequest, results chan<- handoffResult) bool {
			return broker.beginHandoff(slot, actor, request, results)
		}
		actor.activateRun()
		go broker.watchActor(identity, slot, actor)
	}
}

func (broker *Broker) watchActor(identity agentstate.Identity, slot *conversationSlot, actor *conversation) {
	<-actor.done
	broker.mu.Lock()
	if broker.registry[identity] == slot && slot.actor == actor {
		delete(broker.registry, identity)
	}
	delete(broker.orphans, actor)
	broker.mu.Unlock()
}

func (broker *Broker) startConversation(ctx context.Context, identity agentstate.Identity) (*conversation, error) {
	driver := broker.drivers.Lookup(identity.Provider)
	if common.IsNil(driver) {
		return nil, NewBrokerError(agentprotocol.ErrorProviderMissing)
	}
	if !broker.retryIdentityCleanups(ctx, identity) {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	mapping, err := broker.state.Load(identity)
	if err == nil {
		if mapping.Validate(identity) != nil {
			return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		return broker.resumeConversation(ctx, identity, mapping, driver)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	return broker.createConversation(ctx, identity, driver)
}

func (broker *Broker) readiness(ctx context.Context, name provider.Name, driver provider.Driver) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	readiness := driver.Readiness(ctx)
	if readiness.Provider != name {
		return NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	if failure, unavailable := MapReadiness(readiness); unavailable {
		return failure
	}
	return nil
}

func (broker *Broker) createConversation(ctx context.Context, identity agentstate.Identity, driver provider.Driver) (*conversation, error) {
	if err := broker.readiness(ctx, identity.Provider, driver); err != nil {
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
	request := provider.CreateRequest{Provider: identity.Provider, Access: accessForProvider(identity.Provider), Workspace: workspace}
	if request.Validate() != nil {
		broker.cleanupWorkspace(identity, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	session, createErr := driver.Create(ctx, request)
	handle := captureSession(session)
	if createErr != nil {
		if handle == nil {
			broker.cleanupWorkspace(identity, conversationID)
		} else {
			broker.compensateCreate(identity, handle, handle.native.Ref, conversationID)
		}
		if ctx.Err() != nil {
			return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
		}
		return nil, MapError(createErr)
	}
	native, err := validateProviderSession(handle, nil, identity.Provider)
	if err != nil {
		broker.compensateCreate(identity, handle, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	ref, err := agentstate.NativeSessionRef(native.Ref.Value())
	if err != nil || ref != native.Ref {
		broker.compensateCreate(identity, handle, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	at := broker.clock.Now().UTC()
	if at.IsZero() {
		broker.compensateCreate(identity, handle, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	current := agentstate.Session{
		ConversationID: conversationID, NativeSession: ref,
		CreatedAt: at, UpdatedAt: at, ProviderLabel: string(native.Provider), ModelLabel: native.Model,
	}
	outcome, commitErr := broker.state.Create(identity, current, at)
	if outcome == agentstate.CommitApplied && commitErr == nil {
		mapping := agentstate.Mapping{SchemaVersion: agentstate.SchemaVersion, Identity: identity, Current: &current, Archives: []agentstate.Session{}, CreatedAt: at, UpdatedAt: at}
		return broker.newConversation(identity, mapping, handle)
	}
	if outcome == agentstate.CommitApplied || outcome == agentstate.CommitUncertain {
		loaded, loadErr := broker.state.Load(identity)
		if loadErr == nil && loaded.Validate(identity) == nil && mappingHasCurrent(loaded, identity, current) {
			return broker.newConversation(identity, loaded, handle)
		}
		if outcome == agentstate.CommitUncertain && errors.Is(loadErr, os.ErrNotExist) {
			broker.compensateCreate(identity, handle, native.Ref, conversationID)
			return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		// A present mismatched record, or a failed inspection, cannot prove that
		// publishing was unapplied. Stop only the live process; deleting native
		// state or the workspace would be destructive.
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if outcome == agentstate.CommitNotApplied {
		broker.compensateCreate(identity, handle, native.Ref, conversationID)
	}
	// Unknown outcomes cannot authorize destructive compensation. The live
	// process is still stopped so its unread event stream cannot be stranded.
	if outcome != agentstate.CommitNotApplied {
		broker.retainStop(identity, handle)
	}
	return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
}

func (broker *Broker) resumeConversation(ctx context.Context, identity agentstate.Identity, mapping agentstate.Mapping, driver provider.Driver) (*conversation, error) {
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
	if err := broker.readiness(ctx, identity.Provider, driver); err != nil {
		return nil, err
	}
	workspace, err := broker.state.EnsureWorkspace(mapping.Current.ConversationID)
	if err != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	request := provider.ResumeRequest{Provider: identity.Provider, Access: accessForProvider(identity.Provider), NativeSession: mapping.Current.NativeSession, Workspace: workspace}
	if request.Validate() != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	session, err := driver.Resume(ctx, request)
	handle := captureSession(session)
	if err != nil {
		if handle != nil {
			broker.retainStop(identity, handle)
		}
		if ctx.Err() != nil {
			return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
		}
		return nil, MapError(err)
	}
	if _, err := validateProviderSession(handle, &mapping.Current.NativeSession, identity.Provider); err != nil {
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	if mapping.Current.PreparedCommit != nil {
		return broker.reconcilePrepared(ctx, identity, mapping, handle)
	}
	return broker.newConversation(identity, mapping, handle)
}

func validateProviderSession(handle *sessionHandle, expected *provider.NativeSessionRef, expectedProvider provider.Name) (provider.NativeSession, error) {
	if handle == nil || common.IsNil(handle.session) {
		return provider.NativeSession{}, errors.New("nil provider session")
	}
	native := handle.native
	if native.Validate() != nil || native.Provider != expectedProvider || handle.session.Model() != native.Model || common.IsNil(handle.child) || handle.events == nil {
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

func (broker *Broker) retainStop(identity agentstate.Identity, handle *sessionHandle) {
	if handle == nil || common.IsNil(handle.session) {
		return
	}
	broker.retainCleanup(&pendingCleanup{identity: identity, handle: handle})
}

func (broker *Broker) compensateCreate(identity agentstate.Identity, handle *sessionHandle, ref provider.NativeSessionRef, conversationID string) {
	if handle == nil || common.IsNil(handle.session) {
		broker.cleanupWorkspace(identity, conversationID)
		return
	}
	broker.retainCleanup(&pendingCleanup{
		identity: identity, handle: handle, ref: ref, conversationID: conversationID,
		deleteRequired: true, workspaceOwned: true,
	})
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
	if cleanup == nil {
		return true
	}
	for {
		cleanup.mu.Lock()
		if !cleanup.running {
			cleanup.running = true
			cleanup.done = make(chan struct{})
			cleanup.mu.Unlock()
			break
		}
		done := cleanup.done
		cleanup.mu.Unlock()
		select {
		case <-done:
			continue
		case <-ctx.Done():
			return false
		}
	}

	complete := broker.performCleanup(ctx, cleanup)
	cleanup.mu.Lock()
	cleanup.running = false
	close(cleanup.done)
	cleanup.mu.Unlock()
	return complete
}

// performCleanup has one serialized caller, but it never holds cleanup.mu
// across provider, child, filesystem, or state operations. Another Close can
// therefore honor its context while a noncooperative cleanup remains owned.
func (broker *Broker) performCleanup(ctx context.Context, cleanup *pendingCleanup) bool {
	if !cleanup.processStopped {
		cleanup.processStopped = stopPreActor(ctx, cleanup.handle)
	}
	if !cleanup.processStopped {
		return false
	}
	if cleanup.deleteRequired && !cleanup.deleteDone {
		if !cleanup.ref.Valid() {
			return false
		}
		driver := broker.drivers.Lookup(cleanup.identity.Provider)
		if common.IsNil(driver) || driver.Delete(ctx, provider.DeleteRequest{Provider: cleanup.identity.Provider, NativeSession: cleanup.ref}) != nil {
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

func stopPreActor(ctx context.Context, handle *sessionHandle) bool {
	if handle == nil || common.IsNil(handle.session) {
		return true
	}
	session, events, child := handle.session, handle.events, handle.child
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

func (broker *Broker) newConversation(identity agentstate.Identity, mapping agentstate.Mapping, handle *sessionHandle) (*conversation, error) {
	driver := broker.drivers.Lookup(identity.Provider)
	if common.IsNil(driver) {
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(agentprotocol.ErrorProviderMissing)
	}
	actor, err := newConversation(identity, mapping, handle, broker.state, driver, func(candidate *sessionHandle) {
		broker.retainStop(identity, candidate)
	}, broker.ids, broker.clock, broker.timers, broker.lifecycleCtx, broker.idleTimeout, broker.shutdownTimeout)
	if err != nil {
		broker.retainStop(identity, handle)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	return actor, nil
}

func accessForProvider(name provider.Name) provider.AccessMode {
	if name == provider.NameCodex {
		return provider.AccessConfigured
	}
	return provider.AccessContentOnly
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
	broker.mu.Lock()
	for actor := range broker.orphans {
		seen[actor] = struct{}{}
		actors = append(actors, actor)
	}
	broker.mu.Unlock()
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
		} else {
			broker.mu.Lock()
			delete(broker.orphans, actor)
			broker.mu.Unlock()
		}
	}
	// No handoff can start after stopping is published. Wait only within the
	// caller's shutdown budget; a timed-out handoff remains actor-owned and
	// makes Close retryable.
	if err := broker.waitHandoffs(closeCtx); err != nil {
		errs = append(errs, err)
	}
	// A handoff may have lost its registry CAS after the initial snapshot and
	// retained an actor for retryable shutdown. Join that late orphan too.
	broker.mu.Lock()
	lateOrphans := make([]*conversation, 0, len(broker.orphans))
	for actor := range broker.orphans {
		lateOrphans = append(lateOrphans, actor)
	}
	broker.mu.Unlock()
	for _, actor := range lateOrphans {
		if err := actor.close(closeCtx); err != nil {
			errs = append(errs, err)
		} else {
			broker.mu.Lock()
			delete(broker.orphans, actor)
			broker.mu.Unlock()
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
