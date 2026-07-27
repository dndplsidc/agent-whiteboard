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

	// mu owns only admission state and the identity registry. No state, actor,
	// or provider operation is performed while it is held.
	mu       sync.Mutex
	stopping bool
	closed   bool
	registry map[agentstate.Identity]*conversationSlot
	closeMu  sync.Mutex
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
	return &Broker{
		state: config.State, driver: config.Driver,
		ids: &serializedIDs{raw: config.IDs}, clock: config.Clock,
		shutdownTimeout: config.ShutdownTimeout,
		registry:        make(map[agentstate.Identity]*conversationSlot),
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
		actor, startErr := broker.startConversation(ctx, identity)
		broker.mu.Lock()
		slot.actor = actor
		slot.err = startErr
		if startErr != nil && broker.registry[identity] == slot {
			delete(broker.registry, identity)
		}
		close(slot.ready)
		broker.mu.Unlock()
	} else {
		select {
		case <-slot.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
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
	connection, err := slot.actor.attach(ctx, command.ClientID, payload.ReplayAfter)
	if err != nil {
		return nil, err
	}
	return connection, nil
}

func (broker *Broker) startConversation(ctx context.Context, identity agentstate.Identity) (*conversation, error) {
	mapping, err := broker.state.Load(identity)
	if err == nil {
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
		_ = broker.state.RemoveWorkspace(conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	session, createErr := broker.driver.Create(ctx, request)
	if createErr != nil {
		if !common.IsNil(session) {
			native := session.NativeSession()
			broker.compensateCreate(session, native.Ref, conversationID)
		} else {
			_ = broker.state.RemoveWorkspace(conversationID)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, MapError(createErr)
	}
	native, err := validateProviderSession(session, nil)
	if err != nil {
		broker.compensateCreate(session, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	ref, err := agentstate.NativeSessionRef(native.Ref.Value())
	if err != nil || ref != native.Ref {
		broker.compensateCreate(session, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
	}
	at := broker.clock.Now().UTC()
	if at.IsZero() {
		broker.compensateCreate(session, native.Ref, conversationID)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	current := agentstate.Session{
		ConversationID: conversationID, NativeSession: ref,
		CreatedAt: at, UpdatedAt: at, ProviderLabel: string(native.Provider), ModelLabel: native.Model,
	}
	outcome, commitErr := broker.state.Create(identity, current, at)
	if outcome == agentstate.CommitApplied && commitErr == nil {
		mapping := agentstate.Mapping{SchemaVersion: agentstate.SchemaVersion, Identity: identity, Current: &current, CreatedAt: at, UpdatedAt: at}
		return broker.newConversation(identity, mapping, session)
	}
	if outcome == agentstate.CommitApplied || outcome == agentstate.CommitUncertain {
		loaded, loadErr := broker.state.Load(identity)
		if loadErr == nil && mappingHasCurrent(loaded, identity, current) {
			return broker.newConversation(identity, loaded, session)
		}
		if outcome == agentstate.CommitUncertain && errors.Is(loadErr, os.ErrNotExist) {
			broker.compensateCreate(session, native.Ref, conversationID)
			return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
		}
		// A present mismatched record, or a failed inspection, cannot prove that
		// publishing was unapplied. Stop only the live process; deleting native
		// state or the workspace would be destructive.
		broker.shutdownOnly(session)
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if outcome == agentstate.CommitNotApplied {
		broker.compensateCreate(session, native.Ref, conversationID)
	}
	// Unknown outcomes cannot authorize destructive compensation. The live
	// process is still stopped so its unread event stream cannot be stranded.
	if outcome != agentstate.CommitNotApplied {
		broker.shutdownOnly(session)
	}
	return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
}

func (broker *Broker) resumeConversation(ctx context.Context, identity agentstate.Identity, mapping agentstate.Mapping) (*conversation, error) {
	if mapping.Identity != identity || mapping.Current == nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
	}
	if mapping.Current.PreparedCommit != nil {
		return nil, NewBrokerError(agentprotocol.ErrorStateRepairFailed)
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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, MapError(err)
	}
	if _, err := validateProviderSession(session, &mapping.Current.NativeSession); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), broker.shutdownTimeout)
		_ = session.Shutdown(shutdownCtx)
		cancel()
		return nil, NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
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
	return mapping.Identity == identity && mapping.Current != nil &&
		mapping.Current.ConversationID == current.ConversationID &&
		mapping.Current.NativeSession == current.NativeSession &&
		mapping.Current.PreparedCommit == nil
}

func (broker *Broker) shutdownOnly(session provider.Session) {
	if common.IsNil(session) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), broker.shutdownTimeout)
	_ = session.Shutdown(ctx)
	cancel()
}

func (broker *Broker) compensateCreate(session provider.Session, ref provider.NativeSessionRef, conversationID string) {
	broker.shutdownOnly(session)
	if ref.Valid() {
		ctx, cancel := context.WithTimeout(context.Background(), broker.shutdownTimeout)
		_ = broker.driver.Delete(ctx, provider.DeleteRequest{Provider: provider.NamePi, NativeSession: ref})
		cancel()
	}
	_ = broker.state.RemoveWorkspace(conversationID)
}

func (broker *Broker) newConversation(identity agentstate.Identity, mapping agentstate.Mapping, session provider.Session) (*conversation, error) {
	actor, err := newConversation(identity, mapping, session, broker.ids, broker.clock, broker.shutdownTimeout)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), broker.shutdownTimeout)
		_ = session.Shutdown(ctx)
		cancel()
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
	if len(errs) == 0 {
		broker.mu.Lock()
		broker.closed = true
		broker.mu.Unlock()
	}
	return errors.Join(errs...)
}

var _ StateStore = (*agentstate.Store)(nil)
var _ localapi.Backend = (*Broker)(nil)
