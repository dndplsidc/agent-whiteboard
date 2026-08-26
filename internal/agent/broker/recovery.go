package broker

import (
	"context"
	"errors"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
)

type recoveryTrigger uint8

const (
	recoveryTriggerClosure recoveryTrigger = iota
	recoveryTriggerTerminal
)

type recoveryWorkerResult struct {
	generation uint64
	handle     *sessionHandle
	mapping    statepkg.Mapping
	err        error
}

func (actor *conversation) startRecovery(attachments map[*clientAttachment]struct{}, results chan<- recoveryWorkerResult, trigger recoveryTrigger) {
	if actor.stopping || actor.recoveryActive || actor.recoveryAttempted == actor.generation || results == nil {
		return
	}
	actor.expireTerminatedSessionInteractions(attachments)
	if actor.workerSettled != nil && actor.workerKind == providerWorkerArchive {
		if !actor.recoveryPending {
			actor.recoveryPending = true
			actor.recoveryPendingTrigger = trigger
		}
		return
	}
	actor.recoveryAttempted = actor.generation
	actor.recoveryActive = true
	actor.dispatchBlocked = true
	actor.dispatchPending = false

	if trigger == recoveryTriggerClosure {
		actor.publishBrowserError(attachments, protocol.ErrorProviderCrashed)
	}
	if actor.workerSettled != nil && !actor.workerResolved {
		switch actor.workerKind {
		case providerWorkerHistory:
			actor.completePendingCommand(attachments, actor.workerCommandID, actor.workerClientID, protocol.ErrorProviderCrashed)
			actor.workerResolved = true
		case providerWorkerInterrupt:
			actor.completePendingCommand(attachments, actor.workerCommandID, actor.workerClientID, protocol.ErrorTurnInterrupted)
			actor.workerResolved = true
		case providerWorkerCompact:
			actor.completePendingCommand(attachments, actor.workerCommandID, actor.workerClientID, protocol.ErrorProviderCrashed)
			actor.workerResolved = true
		}
	}
	if actor.active != nil {
		turnID := actor.active.request.TurnID
		actor.publishShared(attachments, protocol.InterruptionPayload{TurnID: turnID, Reason: protocol.InterruptionProviderExit})
		if actor.active.originCommandID != "" {
			actor.completePendingCommand(attachments, actor.active.originCommandID, actor.active.originClientID, protocol.ErrorTurnInterrupted)
		}
		zeroProviderContext(actor.active.request.Context)
		actor.active = nil
	}
	if actor.compact != nil {
		if actor.compact.accepted != nil {
			actor.publishShared(attachments, protocol.CompactionPayload{WorkID: actor.compact.request.WorkID, Status: protocol.CompactionFailed})
		}
		if actor.compact.originCommandID != "" {
			actor.completePendingCommand(attachments, actor.compact.originCommandID, actor.compact.originClientID, protocol.ErrorProviderCrashed)
		}
		actor.compact = nil
	}
	if actor.deferredInterrupt != nil {
		actor.completePendingCommand(attachments, actor.deferredInterrupt.commandID, actor.deferredInterrupt.clientID, protocol.ErrorTurnInterrupted)
		actor.deferredInterrupt = nil
	}
	actor.lifecycle = protocol.LifecycleUnavailable
	actor.publishShared(attachments, actor.lifecyclePayload())

	generation := actor.generation
	mapping := cloneMapping(actor.mapping)
	old := actor.session
	actor.shutdownAttempt = newActorShutdown(old, actor.workerSettled)
	ctx, cancel := context.WithCancel(actor.lifecycleCtx)
	actor.recoveryCancel = cancel
	go actor.runRecovery(ctx, generation, mapping, actor.shutdownAttempt, results)
}

func (actor *conversation) runRecovery(ctx context.Context, generation uint64, mapping statepkg.Mapping, oldShutdown *actorShutdown, results chan<- recoveryWorkerResult) {
	result := recoveryWorkerResult{generation: generation, err: errors.New("provider recovery failed")}
	if oldShutdown == nil || oldShutdown.run(ctx, actor.shutdownTimeout) != nil || ctx.Err() != nil {
		results <- result
		return
	}
	if mapping.Validate(actor.identity) != nil || mapping.Current == nil {
		results <- result
		return
	}
	workspace, err := actor.state.EnsureWorkspace(mapping.Current.ConversationID)
	if err != nil || ctx.Err() != nil {
		results <- result
		return
	}
	request := provider.ResumeRequest{
		Provider: actor.identity.Provider, Access: accessForProvider(actor.identity.Provider),
		NativeSession: mapping.Current.NativeSession, Workspace: workspace,
	}
	if request.Validate() != nil {
		results <- result
		return
	}
	session, resumeErr := actor.driver.Resume(ctx, request)
	handle := captureSession(session)
	if resumeErr != nil || ctx.Err() != nil {
		if handle != nil {
			actor.retainSession(handle)
		}
		results <- result
		return
	}
	if _, err := validateProviderSession(handle, &mapping.Current.NativeSession, actor.identity.Provider); err != nil {
		actor.retainSession(handle)
		results <- result
		return
	}
	if prepared := mapping.Current.PreparedCommit; prepared != nil {
		switch prepared.Phase {
		case statepkg.CommitAccepted:
			mapping, err = actor.promoteRecoveryAccepted(mapping)
		case statepkg.CommitPrepared:
			mapping, err = actor.reconcileRecoveryPrepared(ctx, mapping, handle)
		default:
			err = errors.New("invalid recovery commit phase")
		}
		if err != nil {
			actor.retainSession(handle)
			results <- result
			return
		}
	}
	if ctx.Err() != nil {
		actor.retainSession(handle)
		results <- result
		return
	}
	result.handle = handle
	result.mapping = mapping
	result.err = nil
	results <- result
}

func (actor *conversation) reconcileRecoveryPrepared(ctx context.Context, before statepkg.Mapping, handle *sessionHandle) (statepkg.Mapping, error) {
	if before.Current == nil || before.Current.PreparedCommit == nil || before.Current.PreparedCommit.Phase != statepkg.CommitPrepared {
		return statepkg.Mapping{}, errors.New("invalid prepared recovery state")
	}
	prepared := before.Current.PreparedCommit
	drainer := startTemporaryDrainer(handle.events)
	defer drainer.stop()
	state, err := handle.session.Reconcile(ctx, provider.TurnReference{TurnID: prepared.TurnID})
	if err != nil || ctx.Err() != nil || !state.Valid() || !state.Definitive() {
		return statepkg.Mapping{}, errors.New("provider recovery reconciliation failed")
	}
	accepted := state != provider.TurnNotAccepted
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return statepkg.Mapping{}, errors.New("invalid recovery mutation time")
	}
	target := reconciledMapping(before, accepted, at)
	outcome, mutationErr := actor.state.ReconcilePrepared(actor.identity, prepared.TurnID, accepted, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class := classifyLoadedState(actor.state, actor.identity, before, target)
	if class != mappingTarget || !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, errors.New("ambiguous recovery reconciliation")
	}
	return loaded, nil
}

func (actor *conversation) promoteRecoveryAccepted(before statepkg.Mapping) (statepkg.Mapping, error) {
	if before.Validate(actor.identity) != nil || before.Current == nil || before.Current.PreparedCommit == nil || before.Current.PreparedCommit.Phase != statepkg.CommitAccepted {
		return statepkg.Mapping{}, errors.New("invalid accepted recovery state")
	}
	turnID := before.Current.PreparedCommit.TurnID
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return statepkg.Mapping{}, errors.New("invalid recovery mutation time")
	}
	target := promotedMapping(before, at)
	outcome, mutationErr := actor.state.PromotePrepared(actor.identity, turnID, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class := classifyLoadedState(actor.state, actor.identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	if class != mappingPrecondition || !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, errors.New("ambiguous accepted recovery promotion")
	}
	at = actor.clock.Now().UTC()
	if at.IsZero() {
		return statepkg.Mapping{}, errors.New("invalid recovery mutation time")
	}
	target = promotedMapping(loaded, at)
	outcome, mutationErr = actor.state.PromotePrepared(actor.identity, turnID, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class = classifyLoadedState(actor.state, actor.identity, loaded, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	return statepkg.Mapping{}, errors.New("accepted recovery promotion failed")
}

func (actor *conversation) refreshContextFromMapping() {
	actor.contextState = actor.contextStateFromMapping()
	actor.contextDigest = ""
	if actor.mapping.Current == nil {
		return
	}
	if actor.mapping.Current.Observed != nil {
		actor.contextDigest = actor.mapping.Current.Observed.Digest
	} else if actor.mapping.Current.Committed != nil {
		actor.contextDigest = actor.mapping.Current.Committed.Digest
	}
}
