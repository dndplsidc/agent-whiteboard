package broker

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

// Conversation switches are intentionally discovered as optional, narrow
// capabilities. The base StateStore remains limited to the operations every
// actor needs.
type newConversationStore interface {
	NewConversationIfUnchanged(statepkg.Identity, statepkg.Mapping, statepkg.Session, time.Time) (statepkg.CommitOutcome, error)
}

type restoreArchiveStore interface {
	RestoreArchiveIfUnchanged(statepkg.Identity, statepkg.Mapping, string, time.Time) (statepkg.CommitOutcome, error)
}

type restoredPreparedStore interface {
	PromotePreparedIfUnchanged(statepkg.Identity, statepkg.Mapping, string, time.Time) (statepkg.CommitOutcome, error)
	ReconcilePreparedIfUnchanged(statepkg.Identity, statepkg.Mapping, string, bool, time.Time) (statepkg.CommitOutcome, error)
}

type handoffKind uint8

const (
	handoffNew handoffKind = iota
	handoffRestore
)

type handoffRequest struct {
	kind      handoffKind
	before    statepkg.Mapping
	archive   *statepkg.Session
	commandID string
	clientID  string
	old       *sessionHandle
	settings  *provider.ExecutionSettings
}

type handoffResult struct {
	commandID   string
	clientID    string
	code        protocol.BrowserErrorCode
	action      protocol.ArchiveAction
	archiveID   string
	oldStopped  bool
	oldShutdown *actorShutdown
	installed   bool
}

func (actor *conversation) commandHandoff(results chan<- handoffResult, command protocol.Command, archiveID string) protocol.BrowserErrorCode {
	if results == nil || actor.startHandoff == nil || actor.active != nil || actor.compact != nil || !actor.queue.Empty() || actor.workerSettled != nil || actor.deferredInterrupt != nil || actor.recoveryActive || actor.recoveryPending || actor.deferredObserve != nil || actor.dispatchBlocked || actor.stopping || actor.handoffActive || actor.mapping.Current == nil || actor.mapping.Current.PreparedCommit != nil {
		return protocol.ErrorInvalidState
	}
	request := handoffRequest{
		kind: handoffNew, before: cloneMapping(actor.mapping), commandID: command.CommandID,
		clientID: command.ClientID, old: actor.session,
	}
	if payload, ok := command.Payload.(protocol.NewPayload); ok {
		settings, code := validateCommandSettings(actor.identity.Provider, actor.domainCatalog, payload.Settings)
		if code != "" {
			return code
		}
		request.settings = settings
	}
	if archiveID != "" {
		request.kind = handoffRestore
		for index := range actor.mapping.Archives {
			if actor.mapping.Archives[index].ConversationID == archiveID {
				archived := actor.mapping.Archives[index]
				request.archive = &archived
				break
			}
		}
		if request.archive == nil {
			return protocol.ErrorStaleReference
		}
	}
	actor.handoffActive = true
	// Intentional shutdown must never be interpreted as a provider crash.
	actor.stopping = true
	if !actor.startHandoff(request, results) {
		actor.handoffActive = false
		actor.stopping = false
		return protocol.ErrorInvalidState
	}
	return ""
}

func (actor *conversation) handleHandoffResult(attachments map[*clientAttachment]struct{}, result handoffResult) (exit, needsShutdown bool) {
	actor.handoffActive = false
	if result.oldShutdown != nil {
		actor.shutdownAttempt = result.oldShutdown
	}
	if result.installed {
		if !actor.closeRequested {
			actor.publishShared(attachments, protocol.ArchivePayload{Action: result.action, ArchiveID: result.archiveID})
		}
		actor.completePendingCommand(attachments, result.commandID, result.clientID, "")
		actor.retired.Store(true)
		for item := range attachments {
			delete(attachments, item)
			item.finishAfterDrain(actor.shutdownTimeout)
		}
		return true, false
	}

	actor.completePendingCommand(attachments, result.commandID, result.clientID, result.code)
	if result.oldStopped {
		// The durable mutation may be applied or uncertain. No in-memory actor
		// may continue after its provider was joined; reconnect performs Load.
		actor.retired.Store(true)
		for item := range attachments {
			delete(attachments, item)
			item.finishAfterDrain(actor.shutdownTimeout)
		}
		return true, false
	}
	if actor.closeRequested {
		return false, true
	}
	actor.stopping = false
	if result.oldShutdown != nil {
		// A failed stop is retryable, but the provider generation is no longer
		// safe for commands. Broker.Close owns the next shutdown attempt.
		actor.handoffFailed = true
		actor.dispatchBlocked = true
		actor.lifecycle = protocol.LifecycleUnavailable
		actor.publishShared(attachments, actor.lifecyclePayload())
	}
	return false, false
}

func (broker *Broker) beginHandoff(slot *conversationSlot, actor *conversation, request handoffRequest, results chan<- handoffResult) bool {
	broker.mu.Lock()
	if broker.stopping || broker.registry[actor.identity] != slot || slot.actor != actor {
		broker.mu.Unlock()
		return false
	}
	broker.startHandoffWorker()
	broker.mu.Unlock()
	go func() {
		defer broker.finishHandoffWorker()
		results <- broker.runHandoff(slot, actor, request)
	}()
	return true
}

func (broker *Broker) startHandoffWorker() {
	broker.handoffMu.Lock()
	if broker.handoffCount == 0 {
		broker.handoffIdle = make(chan struct{})
	}
	broker.handoffCount++
	broker.handoffMu.Unlock()
}

func (broker *Broker) finishHandoffWorker() {
	broker.handoffMu.Lock()
	broker.handoffCount--
	if broker.handoffCount == 0 {
		close(broker.handoffIdle)
	}
	broker.handoffMu.Unlock()
}

func (broker *Broker) waitHandoffs(ctx context.Context) error {
	broker.handoffMu.Lock()
	if broker.handoffCount == 0 {
		broker.handoffMu.Unlock()
		return nil
	}
	idle := broker.handoffIdle
	broker.handoffMu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (broker *Broker) runHandoff(slot *conversationSlot, oldActor *conversation, request handoffRequest) handoffResult {
	result := handoffResult{
		commandID: request.commandID, clientID: request.clientID,
		code: protocol.ErrorStateRepairFailed,
	}
	if request.before.Validate(oldActor.identity) != nil || request.before.Current == nil || request.old == nil {
		return result
	}

	var candidate *sessionHandle
	var current statepkg.Session
	var conversationID string
	var candidateDrainer *temporaryDrainer
	cleanupCandidate := func(canDelete bool) {
		if candidateDrainer != nil {
			candidateDrainer.stop()
			candidateDrainer = nil
		}
		if request.kind == handoffRestore {
			if candidate != nil {
				broker.retainStop(oldActor.identity, candidate)
			}
			return
		}
		if !canDelete {
			if candidate != nil {
				broker.retainStop(oldActor.identity, candidate)
			}
			return
		}
		if candidate == nil {
			broker.cleanupWorkspace(oldActor.identity, conversationID)
			return
		}
		broker.compensateCreate(oldActor.identity, candidate, candidate.native.Ref, conversationID)
	}

	if request.kind == handoffNew {
		if _, ok := broker.state.(newConversationStore); !ok {
			return result
		}
		if code := broker.handoffReadiness(oldActor); code != "" {
			result.code = code
			return result
		}
		id, err := broker.ids.NewID()
		if err != nil || common.ValidateID(id) != nil || mappingContainsConversation(request.before, id) {
			return result
		}
		conversationID = id
		workspace, err := broker.state.EnsureWorkspace(id)
		if err != nil {
			return result
		}
		createRequest := provider.CreateRequest{Provider: oldActor.identity.Provider, Access: accessForProvider(oldActor.identity.Provider), Workspace: workspace, Settings: request.settings}
		if createRequest.Validate() != nil {
			if exactMapping(broker.state, oldActor.identity, request.before) {
				cleanupCandidate(true)
			}
			return result
		}
		session, err := oldActor.driver.Create(broker.lifecycleCtx, createRequest)
		candidate = captureSession(session)
		if err != nil || broker.lifecycleCtx.Err() != nil {
			result.code = MapError(err).Code()
			cleanupCandidate(exactMapping(broker.state, oldActor.identity, request.before))
			return result
		}
		native, err := validateProviderSession(candidate, nil, oldActor.identity.Provider)
		if err != nil {
			result.code = protocol.ErrorProviderProtocolFailure
			cleanupCandidate(exactMapping(broker.state, oldActor.identity, request.before))
			return result
		}
		ref, err := statepkg.NativeSessionRef(native.Ref.Value())
		if err != nil || ref != native.Ref {
			result.code = protocol.ErrorProviderProtocolFailure
			cleanupCandidate(exactMapping(broker.state, oldActor.identity, request.before))
			return result
		}
		if mappingContainsNative(request.before, ref) {
			result.code = protocol.ErrorProviderProtocolFailure
			if exactMapping(broker.state, oldActor.identity, request.before) {
				broker.retainStop(oldActor.identity, candidate)
				candidate = nil
				broker.cleanupWorkspace(oldActor.identity, conversationID)
			} else {
				broker.retainStop(oldActor.identity, candidate)
				candidate = nil
			}
			return result
		}
		at := broker.clock.Now().UTC()
		if at.IsZero() {
			cleanupCandidate(exactMapping(broker.state, oldActor.identity, request.before))
			return result
		}
		current, err = stateSessionFromNative(id, native, at)
		if err != nil {
			result.code = protocol.ErrorProviderProtocolFailure
			cleanupCandidate(exactMapping(broker.state, oldActor.identity, request.before))
			return result
		}
		candidateDrainer = startTemporaryDrainer(candidate.events)
	} else {
		if _, ok := broker.state.(restoreArchiveStore); !ok || request.archive == nil {
			return result
		}
		if code := broker.handoffReadiness(oldActor); code != "" {
			result.code = code
			return result
		}
		current = *request.archive
		conversationID = current.ConversationID
		workspace, err := broker.state.EnsureWorkspace(conversationID)
		if err != nil {
			return result
		}
		resumeRequest := provider.ResumeRequest{
			Provider: oldActor.identity.Provider, Access: accessForProvider(oldActor.identity.Provider),
			NativeSession: current.NativeSession, Workspace: workspace,
		}
		if resumeRequest.Validate() != nil {
			return result
		}
		session, err := oldActor.driver.Resume(broker.lifecycleCtx, resumeRequest)
		candidate = captureSession(session)
		if err != nil || broker.lifecycleCtx.Err() != nil {
			result.code = MapError(err).Code()
			cleanupCandidate(false)
			return result
		}
		if _, err := validateProviderSession(candidate, &current.NativeSession, oldActor.identity.Provider); err != nil {
			result.code = protocol.ErrorProviderProtocolFailure
			cleanupCandidate(false)
			return result
		}
		candidateDrainer = startTemporaryDrainer(candidate.events)
	}

	at := broker.clock.Now().UTC()
	if request.kind == handoffNew {
		at = current.CreatedAt
	}
	if at.IsZero() {
		cleanupCandidate(request.kind == handoffNew && exactMapping(broker.state, oldActor.identity, request.before))
		return result
	}

	shutdown := newActorShutdown(request.old, nil)
	result.oldShutdown = shutdown
	if shutdown.run(broker.lifecycleCtx, broker.shutdownTimeout) != nil {
		result.code = protocol.ErrorProviderProtocolFailure
		cleanupCandidate(request.kind == handoffNew && exactMapping(broker.state, oldActor.identity, request.before))
		return result
	}
	result.oldStopped = true

	var target statepkg.Mapping
	var class mappingClassification
	var err error
	if request.kind == handoffNew {
		target = newCurrentMapping(request.before, current, at)
		target, class, err = broker.commitNewExact(request.before, target, current, at)
		result.action = protocol.ArchiveCreated
		result.archiveID = request.before.Current.ConversationID
	} else {
		target, _ = restoredCurrentMapping(request.before, conversationID, at)
		target, class, err = broker.commitRestoreExact(request.before, target, conversationID, at)
		result.action = protocol.ArchiveRestored
		result.archiveID = conversationID
	}
	if err != nil {
		cleanupCandidate(request.kind == handoffNew && class == mappingPrecondition)
		return result
	}

	if candidateDrainer != nil {
		candidateDrainer.stop()
		candidateDrainer = nil
	}
	var replacement *conversation
	var actorErr error
	if request.kind == handoffRestore && target.Current != nil && target.Current.PreparedCommit != nil {
		if _, ok := broker.state.(restoredPreparedStore); !ok {
			cleanupCandidate(false)
			return result
		}
		switch target.Current.PreparedCommit.Phase {
		case statepkg.CommitAccepted:
			target, actorErr = broker.promoteRestoredAcceptedExact(target)
		case statepkg.CommitPrepared:
			target, actorErr = broker.reconcileRestoredPreparedExact(broker.lifecycleCtx, target, candidate)
		default:
			actorErr = errors.New("invalid restored prepared phase")
		}
		if actorErr != nil {
			cleanupCandidate(false)
			return result
		}
		replacement, actorErr = broker.newConversation(oldActor.identity, target, candidate)
		if actorErr != nil {
			// broker.newConversation retained the candidate.
			return result
		}
	} else {
		replacement, actorErr = broker.newConversation(oldActor.identity, target, candidate)
		if actorErr != nil {
			// broker.newConversation retains the published candidate on failure.
			return result
		}
	}
	if broker.installReplacement(slot, oldActor, replacement) {
		result.installed = true
		return result
	}
	replacement.activateRun()

	// A lost registry CAS must not strand an unregistered actor. Keep it
	// retryably owned until its provider process and event consumer join.
	broker.mu.Lock()
	broker.orphans[replacement] = struct{}{}
	broker.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), broker.shutdownTimeout)
	if replacement.close(ctx) == nil {
		broker.mu.Lock()
		delete(broker.orphans, replacement)
		broker.mu.Unlock()
	}
	cancel()
	return result
}

func (broker *Broker) handoffReadiness(actor *conversation) protocol.BrowserErrorCode {
	if actor == nil || common.IsNil(actor.driver) {
		return protocol.ErrorProviderProtocolFailure
	}
	if err := broker.readiness(broker.lifecycleCtx, actor.identity.Provider, actor.driver); err != nil {
		var coded browserErrorCoder
		if errors.As(err, &coded) {
			return coded.BrowserErrorCode()
		}
		return protocol.ErrorProviderProtocolFailure
	}
	return ""
}

func (broker *Broker) installReplacement(expectedSlot *conversationSlot, expectedActor, replacement *conversation) bool {
	broker.mu.Lock()
	if broker.stopping || broker.registry[expectedActor.identity] != expectedSlot || expectedSlot.actor != expectedActor {
		broker.mu.Unlock()
		return false
	}
	slot := &conversationSlot{ready: make(chan struct{}), actor: replacement}
	// Keep the displaced actor discoverable until it consumes the handoff
	// result and its watcher observes done.
	broker.orphans[expectedActor] = struct{}{}
	replacement.startHandoff = func(request handoffRequest, results chan<- handoffResult) bool {
		return broker.beginHandoff(slot, replacement, request, results)
	}
	close(slot.ready)
	broker.registry[expectedActor.identity] = slot
	broker.mu.Unlock()
	replacement.activateRun()
	go broker.watchActor(expectedActor.identity, slot, replacement)
	return true
}

func (broker *Broker) commitNewExact(before, target statepkg.Mapping, current statepkg.Session, at time.Time) (statepkg.Mapping, mappingClassification, error) {
	state := broker.state.(newConversationStore)
	return broker.commitHandoffExact(before, target, func() (statepkg.CommitOutcome, error) {
		return state.NewConversationIfUnchanged(before.Identity, before, current, at)
	})
}

func (broker *Broker) commitRestoreExact(before, target statepkg.Mapping, archiveID string, at time.Time) (statepkg.Mapping, mappingClassification, error) {
	state := broker.state.(restoreArchiveStore)
	return broker.commitHandoffExact(before, target, func() (statepkg.CommitOutcome, error) {
		return state.RestoreArchiveIfUnchanged(before.Identity, before, archiveID, at)
	})
}

func (broker *Broker) commitHandoffExact(before, target statepkg.Mapping, mutate func() (statepkg.CommitOutcome, error)) (statepkg.Mapping, mappingClassification, error) {
	outcome, mutationErr := mutate()
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, mappingTarget, nil
	}
	loaded, class := classifyLoadedState(broker.state, before.Identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, class, nil
	}
	if class != mappingPrecondition || !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, class, errors.New("conversation handoff outcome is ambiguous")
	}
	outcome, mutationErr = mutate()
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, mappingTarget, nil
	}
	loaded, class = classifyLoadedState(broker.state, before.Identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, class, nil
	}
	return statepkg.Mapping{}, class, errors.New("conversation handoff mutation failed")
}

func (broker *Broker) promoteRestoredAcceptedExact(before statepkg.Mapping) (statepkg.Mapping, error) {
	prepared := before.Current.PreparedCommit
	if prepared == nil || prepared.Phase != statepkg.CommitAccepted {
		return statepkg.Mapping{}, errors.New("invalid restored accepted state")
	}
	state := broker.state.(restoredPreparedStore)
	return broker.commitRestoredRepairExact(before, func(expected statepkg.Mapping, at time.Time) (statepkg.Mapping, statepkg.CommitOutcome, error) {
		target := promotedMapping(expected, at)
		outcome, err := state.PromotePreparedIfUnchanged(before.Identity, expected, prepared.TurnID, at)
		return target, outcome, err
	})
}

func (broker *Broker) reconcileRestoredPreparedExact(ctx context.Context, before statepkg.Mapping, candidate *sessionHandle) (statepkg.Mapping, error) {
	prepared := before.Current.PreparedCommit
	if prepared == nil || prepared.Phase != statepkg.CommitPrepared || candidate == nil {
		return statepkg.Mapping{}, errors.New("invalid restored prepared state")
	}
	drainer := startTemporaryDrainer(candidate.events)
	stateValue, err := candidate.session.Reconcile(ctx, provider.TurnReference{TurnID: prepared.TurnID})
	drainer.stop()
	if err != nil || ctx.Err() != nil || !stateValue.Valid() || !stateValue.Definitive() {
		return statepkg.Mapping{}, errors.New("restored reconciliation failed")
	}
	accepted := stateValue != provider.TurnNotAccepted
	state := broker.state.(restoredPreparedStore)
	return broker.commitRestoredRepairExact(before, func(expected statepkg.Mapping, at time.Time) (statepkg.Mapping, statepkg.CommitOutcome, error) {
		target := reconciledMapping(expected, accepted, at)
		outcome, mutationErr := state.ReconcilePreparedIfUnchanged(before.Identity, expected, prepared.TurnID, accepted, at)
		return target, outcome, mutationErr
	})
}

func (broker *Broker) commitRestoredRepairExact(before statepkg.Mapping, mutate func(statepkg.Mapping, time.Time) (statepkg.Mapping, statepkg.CommitOutcome, error)) (statepkg.Mapping, error) {
	at := broker.clock.Now().UTC()
	if at.IsZero() {
		return statepkg.Mapping{}, errors.New("invalid restored repair time")
	}
	target, outcome, mutationErr := mutate(before, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class := classifyLoadedState(broker.state, before.Identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	if class != mappingPrecondition || !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, errors.New("restored repair outcome is ambiguous")
	}
	at = broker.clock.Now().UTC()
	if at.IsZero() {
		return statepkg.Mapping{}, errors.New("invalid restored repair retry time")
	}
	target, outcome, mutationErr = mutate(loaded, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class = classifyLoadedState(broker.state, before.Identity, loaded, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	return statepkg.Mapping{}, errors.New("restored repair mutation failed")
}

func newCurrentMapping(before statepkg.Mapping, current statepkg.Session, at time.Time) statepkg.Mapping {
	result := cloneMapping(before)
	if result.Current != nil {
		result.Archives = append(result.Archives, *result.Current)
	}
	copyOfCurrent := current
	result.Current = &copyOfCurrent
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result
}

func restoredCurrentMapping(before statepkg.Mapping, archiveID string, at time.Time) (statepkg.Mapping, bool) {
	result := cloneMapping(before)
	index := -1
	for candidate := range result.Archives {
		if result.Archives[candidate].ConversationID == archiveID {
			index = candidate
			break
		}
	}
	if index < 0 {
		return statepkg.Mapping{}, false
	}
	restored := result.Archives[index]
	result.Archives = append(result.Archives[:index], result.Archives[index+1:]...)
	if result.Current != nil {
		result.Archives = append(result.Archives, *result.Current)
	}
	result.Current = &restored
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result, true
}

func mappingContainsConversation(mapping statepkg.Mapping, conversationID string) bool {
	if mapping.Current != nil && mapping.Current.ConversationID == conversationID {
		return true
	}
	for _, archived := range mapping.Archives {
		if archived.ConversationID == conversationID {
			return true
		}
	}
	return false
}

func mappingContainsNative(mapping statepkg.Mapping, ref provider.NativeSessionRef) bool {
	if mapping.Current != nil && mapping.Current.NativeSession == ref {
		return true
	}
	for _, archived := range mapping.Archives {
		if archived.NativeSession == ref {
			return true
		}
	}
	return false
}

func exactMapping(state StateStore, identity statepkg.Identity, expected statepkg.Mapping) bool {
	loaded, err := state.Load(identity)
	return err == nil && loaded.Validate(identity) == nil && reflect.DeepEqual(loaded, expected)
}

var _ newConversationStore = (*statepkg.Store)(nil)
var _ restoreArchiveStore = (*statepkg.Store)(nil)
