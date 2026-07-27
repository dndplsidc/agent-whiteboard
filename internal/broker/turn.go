package broker

import (
	"errors"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type turnPhase uint8

const (
	turnStarting turnPhase = iota
	turnRunning
	turnInterrupting
	turnInterruptRequested
	turnAcceptanceUnknown
)

type activeTurn struct {
	request         provider.TurnRequest
	accepted        *provider.AcceptedTurn
	phase           turnPhase
	originCommandID string
	originClientID  string
	pendingEvents   []provider.Event
	pendingBytes    int
	pendingOverflow bool
}

type turnWorkerKind uint8

const (
	turnWorkerSubmit turnWorkerKind = iota
	turnWorkerInterrupt
)

type turnWorkerResult struct {
	kind      turnWorkerKind
	turnID    string
	accepted  provider.AcceptedTurn
	commandID string
	clientID  string
	err       error
}

func (actor *conversation) commandSubmit(attachments map[*attachment]struct{}, turnResults chan<- turnWorkerResult, command agentprotocol.Command, payload agentprotocol.SubmitPayload) (bool, agentprotocol.BrowserErrorCode) {
	if actor.active != nil && (actor.active.request.TurnID == payload.TurnID || actor.active.request.MessageID == payload.MessageID) {
		return false, agentprotocol.ErrorInvalidState
	}
	if actor.queue.ContainsTurnID(payload.TurnID) || actor.queue.ContainsMessageID(payload.MessageID) {
		return false, agentprotocol.ErrorInvalidState
	}
	if actor.lifecycle == agentprotocol.LifecycleUnavailable || actor.mapping.Current == nil || actor.mapping.Current.PreparedCommit != nil {
		return false, agentprotocol.ErrorInvalidState
	}

	request, code := actor.convertSubmittedTurn(payload)
	if code != "" {
		return false, code
	}
	if actor.active == nil && actor.queue.Empty() {
		if code := actor.prepareTurn(request); code != "" {
			zeroProviderContext(request.Context)
			return false, code
		}
		actor.active = &activeTurn{request: request, phase: turnStarting, originCommandID: command.CommandID, originClientID: command.ClientID}
		actor.startSubmitWorker(turnResults, request)
		return true, ""
	}
	if actor.active != nil && actor.active.phase != turnRunning && actor.active.phase != turnInterrupting && actor.active.phase != turnInterruptRequested {
		zeroProviderContext(request.Context)
		return false, agentprotocol.ErrorInvalidState
	}
	contextForHead := request.Context
	if actor.queue.Empty() {
		contextForHead = nil
	}
	if contextForHead != nil {
		request.Context = nil
	}
	turn := QueuedTurn{TurnID: request.TurnID, MessageID: request.MessageID, Message: request.Message, Context: request.Context}
	if err := actor.queue.Enqueue(turn); err != nil {
		zeroProviderContext(request.Context)
		zeroProviderContext(contextForHead)
		if errors.Is(err, ErrQueueFull) {
			return false, agentprotocol.ErrorQueueFull
		}
		return false, agentprotocol.ErrorInvalidState
	}
	zeroProviderContext(request.Context)
	if contextForHead != nil {
		if err := actor.queue.attachContextToHead(contextForHead); err != nil {
			_ = actor.queue.Remove(payload.MessageID)
			zeroProviderContext(contextForHead)
			return false, agentprotocol.ErrorInvalidState
		}
	}
	if !actor.publishShared(attachments, agentprotocol.QueuePayload{Items: actor.queue.Items()}) {
		_ = actor.queue.Remove(payload.MessageID)
		if contextForHead != nil {
			actor.queue.discardContext()
		}
		return false, agentprotocol.ErrorBrokerUnavailable
	}
	if actor.active == nil {
		actor.dispatchNext(attachments, turnResults)
	}
	return false, ""
}

func (actor *conversation) convertSubmittedTurn(payload agentprotocol.SubmitPayload) (provider.TurnRequest, agentprotocol.BrowserErrorCode) {
	contextOwned := (actor.active != nil && actor.active.request.Context != nil) || actor.queue.hasContext
	if actor.contextState != agentprotocol.ContextPending {
		if payload.Context != nil {
			return provider.TurnRequest{}, agentprotocol.ErrorInvalidState
		}
		request := provider.TurnRequest{TurnID: payload.TurnID, MessageID: payload.MessageID, Message: payload.Message}
		if request.Validate() != nil {
			return provider.TurnRequest{}, agentprotocol.ErrorInvalidCommand
		}
		return request, ""
	}
	if contextOwned {
		if payload.Context != nil {
			return provider.TurnRequest{}, agentprotocol.ErrorInvalidState
		}
		request := provider.TurnRequest{TurnID: payload.TurnID, MessageID: payload.MessageID, Message: payload.Message}
		if request.Validate() != nil {
			return provider.TurnRequest{}, agentprotocol.ErrorInvalidCommand
		}
		return request, ""
	}
	if payload.Context == nil || actor.mapping.Current == nil || actor.mapping.Current.Observed == nil || actor.resource.ID == "" {
		return provider.TurnRequest{}, agentprotocol.ErrorInvalidState
	}
	connected := ConnectIdentity{Origin: actor.identity.Origin, Provider: agentprotocol.ProviderPi, Resource: actor.resource, ContextDigest: actor.contextDigest}
	converted, err := PageContextToProvider(*payload.Context, connected, actor.identity.Origin)
	if err != nil || string(payload.Context.Revision) != string(actor.mapping.Current.Observed.Revision) || payload.Context.Digest != actor.mapping.Current.Observed.Digest || !payload.Context.Resource.UpdatedAt.Equal(actor.mapping.Current.Observed.SourceUpdatedAt) {
		zeroProviderContext(&converted)
		return provider.TurnRequest{}, agentprotocol.ErrorInvalidState
	}
	request := provider.TurnRequest{TurnID: payload.TurnID, MessageID: payload.MessageID, Message: payload.Message, Context: &converted}
	if request.Validate() != nil {
		zeroProviderContext(&converted)
		return provider.TurnRequest{}, agentprotocol.ErrorInvalidCommand
	}
	return request, ""
}

func (actor *conversation) prepareTurn(request provider.TurnRequest) agentprotocol.BrowserErrorCode {
	if actor.contextState == agentprotocol.ContextPending {
		if request.Context == nil || actor.mapping.Current == nil || actor.mapping.Current.Observed == nil {
			return agentprotocol.ErrorInvalidState
		}
		expected := actor.mapping.Current.Observed
		if request.Context.Digest != expected.Digest || agentstate.RevisionKind(request.Context.Revision) != expected.Revision || !request.Context.Resource.UpdatedAt.Equal(expected.SourceUpdatedAt) {
			return agentprotocol.ErrorBoardRevisionUnavailable
		}
	} else if request.Context != nil {
		return agentprotocol.ErrorInvalidState
	}
	if request.Context == nil {
		return ""
	}
	revision := agentstate.Revision{Digest: request.Context.Digest, Revision: agentstate.RevisionKind(request.Context.Revision), SourceUpdatedAt: request.Context.Resource.UpdatedAt}
	before := cloneMapping(actor.mapping)
	at := actor.clock.Now().UTC()
	target := preparedMapping(before, revision, request.TurnID, at)
	mapping, ok := actor.applyDurableTransition(before, target, at, func(at time.Time) (agentstate.CommitOutcome, error) {
		return actor.state.PrepareCommit(actor.identity, revision, request.TurnID, at)
	})
	if !ok {
		return agentprotocol.ErrorStateRepairFailed
	}
	actor.mapping = mapping
	return ""
}

func (actor *conversation) startSubmitWorker(results chan<- turnWorkerResult, request provider.TurnRequest) {
	actor.workerSettled = make(chan struct{})
	go func() {
		preflight, err := actor.session.Preflight(actor.lifecycleCtx, provider.PreflightRequest{Turn: request})
		if err == nil {
			err = preflight.Validate()
		}
		if err != nil {
			results <- turnWorkerResult{kind: turnWorkerSubmit, turnID: request.TurnID, err: err}
			return
		}
		accepted, err := actor.session.Submit(actor.lifecycleCtx, request)
		if err == nil {
			if accepted.Validate() != nil || accepted.TurnID != request.TurnID {
				err = errors.New("invalid provider accepted turn")
			}
		}
		results <- turnWorkerResult{kind: turnWorkerSubmit, turnID: request.TurnID, accepted: accepted, err: err}
	}()
}

func (actor *conversation) commandInterrupt(results chan<- turnWorkerResult, command agentprotocol.Command, payload agentprotocol.TurnReferencePayload) (bool, agentprotocol.BrowserErrorCode) {
	if actor.active == nil || actor.active.phase != turnRunning || actor.active.accepted == nil || actor.active.request.TurnID != payload.TurnID {
		return false, agentprotocol.ErrorInvalidState
	}
	accepted := *actor.active.accepted
	actor.active.phase = turnInterrupting
	actor.workerSettled = make(chan struct{})
	go func() {
		err := actor.session.Interrupt(actor.lifecycleCtx, accepted)
		results <- turnWorkerResult{kind: turnWorkerInterrupt, turnID: accepted.TurnID, commandID: command.CommandID, clientID: command.ClientID, err: err}
	}()
	return true, ""
}

func (actor *conversation) handleTurnResult(attachments map[*attachment]struct{}, results chan<- turnWorkerResult, result turnWorkerResult) {
	settled := actor.workerSettled
	actor.workerSettled = nil
	if settled != nil {
		defer close(settled)
	}
	if actor.active == nil || actor.active.request.TurnID != result.turnID {
		return
	}
	if result.kind == turnWorkerInterrupt {
		actor.handleInterruptResult(attachments, results, result)
		return
	}
	actor.handleSubmitResult(attachments, results, result)
}

func (actor *conversation) handleSubmitResult(attachments map[*attachment]struct{}, results chan<- turnWorkerResult, result turnWorkerResult) {
	active := actor.active
	if result.err != nil && (len(active.pendingEvents) != 0 || active.pendingOverflow) {
		result.err = provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	zeroProviderContext(active.request.Context)
	active.request.Context = nil
	if result.err != nil {
		code := MapError(result.err).Code()
		if activeHasPrepared(actor.mapping.Current, active.request.TurnID) && code != agentprotocol.ErrorAcceptanceOutcomeUnknown {
			if !actor.rejectPrepared(active.request.TurnID) {
				code = agentprotocol.ErrorStateRepairFailed
			}
		}
		if code == agentprotocol.ErrorAcceptanceOutcomeUnknown || activeHasPrepared(actor.mapping.Current, active.request.TurnID) {
			actor.contextState = agentprotocol.ContextUnavailable
			active.phase = turnAcceptanceUnknown
			actor.lifecycle = agentprotocol.LifecycleUnavailable
		} else {
			actor.active = nil
			actor.contextState = actor.contextStateFromMapping()
			actor.lifecycle = agentprotocol.LifecycleReady
		}
		actor.publishBrowserError(attachments, code)
		actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
		if active.originCommandID != "" {
			actor.completePendingCommand(attachments, active.originCommandID, active.originClientID, code)
		}
		actor.flushPendingProviderEvents(attachments, results)
		return
	}

	accepted := result.accepted
	active.accepted = &accepted
	active.phase = turnRunning
	if activeHasPrepared(actor.mapping.Current, active.request.TurnID) {
		if !actor.acceptPrepared(active.request.TurnID) {
			actor.contextState = agentprotocol.ContextUnavailable
			actor.lifecycle = agentprotocol.LifecycleUnavailable
			actor.publishBrowserError(attachments, agentprotocol.ErrorStateRepairFailed)
			actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
			if active.originCommandID != "" {
				actor.completePendingCommand(attachments, active.originCommandID, active.originClientID, agentprotocol.ErrorStateRepairFailed)
			}
			actor.flushPendingProviderEvents(attachments, results)
			return
		}
		actor.contextState = agentprotocol.ContextAccepted
		actor.contextDigest = actor.mapping.Current.Committed.Digest
		actor.publishShared(attachments, agentprotocol.ContextPayload{Digest: actor.contextDigest, State: agentprotocol.ContextAccepted})
	}
	actor.lifecycle = agentprotocol.LifecycleResponding
	turnID := active.request.TurnID
	actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle, TurnID: &turnID})
	if active.originCommandID != "" {
		actor.completePendingCommand(attachments, active.originCommandID, active.originClientID, "")
		active.originCommandID = ""
		active.originClientID = ""
	}
	actor.flushPendingProviderEvents(attachments, results)
}

func (actor *conversation) handleInterruptResult(attachments map[*attachment]struct{}, results chan<- turnWorkerResult, result turnWorkerResult) {
	if result.err != nil {
		actor.active.phase = turnRunning
		code := MapError(result.err).Code()
		actor.completePendingCommand(attachments, result.commandID, result.clientID, code)
		actor.flushPendingProviderEvents(attachments, results)
		return
	}
	actor.active.phase = turnInterruptRequested
	actor.completePendingCommand(attachments, result.commandID, result.clientID, "")
	actor.flushPendingProviderEvents(attachments, results)
}

func (actor *conversation) rejectPrepared(turnID string) bool {
	before := cloneMapping(actor.mapping)
	at := actor.clock.Now().UTC()
	target := reconciledMapping(before, false, at)
	mapping, ok := actor.applyDurableTransition(before, target, at, func(at time.Time) (agentstate.CommitOutcome, error) {
		return actor.state.ReconcilePrepared(actor.identity, turnID, false, at)
	})
	if ok {
		actor.mapping = mapping
	}
	return ok
}

func (actor *conversation) acceptPrepared(turnID string) bool {
	before := cloneMapping(actor.mapping)
	at := actor.clock.Now().UTC()
	accepted := acceptedMapping(before, turnID, at)
	mapping, ok := actor.applyDurableTransition(before, accepted, at, func(at time.Time) (agentstate.CommitOutcome, error) {
		return actor.state.MarkPreparedAccepted(actor.identity, turnID, at)
	})
	if !ok {
		return false
	}
	actor.mapping = mapping
	before = cloneMapping(mapping)
	at = actor.clock.Now().UTC()
	promoted := promotedMapping(before, at)
	mapping, ok = actor.applyDurableTransition(before, promoted, at, func(at time.Time) (agentstate.CommitOutcome, error) {
		return actor.state.PromotePrepared(actor.identity, turnID, at)
	})
	if ok {
		actor.mapping = mapping
	}
	return ok
}

func (actor *conversation) applyDurableTransition(before, target agentstate.Mapping, at time.Time, operation func(time.Time) (agentstate.CommitOutcome, error)) (agentstate.Mapping, bool) {
	if at.IsZero() {
		return agentstate.Mapping{}, false
	}
	outcome, mutationErr := operation(at)
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		return target, true
	}
	loaded, class := classifyLoadedState(actor.state, actor.identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, true
	}
	if class != mappingPrecondition || outcome == agentstate.CommitApplied || !knownCommitOutcome(outcome) {
		return agentstate.Mapping{}, false
	}
	outcome, mutationErr = operation(at)
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		return target, true
	}
	loaded, class = classifyLoadedState(actor.state, actor.identity, before, target)
	if class != mappingTarget || !knownCommitOutcome(outcome) {
		return agentstate.Mapping{}, false
	}
	return loaded, true
}

func (actor *conversation) contextStateFromMapping() agentprotocol.ContextState {
	if actor.mapping.Current == nil {
		return agentprotocol.ContextUnavailable
	}
	if actor.mapping.Current.PreparedCommit != nil {
		return agentprotocol.ContextUnavailable
	}
	if actor.mapping.Current.Observed != nil {
		return agentprotocol.ContextPending
	}
	if actor.mapping.Current.Committed != nil {
		return agentprotocol.ContextUnchanged
	}
	return agentprotocol.ContextPending
}

func activeHasPrepared(session *agentstate.Session, turnID string) bool {
	return session != nil && session.PreparedCommit != nil && session.PreparedCommit.TurnID == turnID
}

func (actor *conversation) finishActive(attachments map[*attachment]struct{}, results chan<- turnWorkerResult, terminalLifecycle agentprotocol.LifecycleState) {
	if actor.active != nil {
		zeroProviderContext(actor.active.request.Context)
	}
	actor.active = nil
	if actor.dispatchBlocked {
		actor.lifecycle = agentprotocol.LifecycleUnavailable
		actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	if actor.mapping.Current != nil && actor.mapping.Current.PreparedCommit != nil {
		actor.lifecycle = agentprotocol.LifecycleUnavailable
		actor.contextState = agentprotocol.ContextUnavailable
		actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	if actor.stopping || actor.queue.Empty() {
		actor.lifecycle = terminalLifecycle
		actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	actor.dispatchNext(attachments, results)
}

func (actor *conversation) dispatchNext(attachments map[*attachment]struct{}, results chan<- turnWorkerResult) {
	candidate, ok := actor.queue.peek()
	if !ok {
		return
	}
	if code := actor.prepareTurn(candidate); code != "" {
		if code == agentprotocol.ErrorInvalidState || code == agentprotocol.ErrorBoardRevisionUnavailable {
			if code == agentprotocol.ErrorBoardRevisionUnavailable {
				actor.queue.discardContext()
				actor.publishBrowserError(attachments, code)
			}
			actor.lifecycle = agentprotocol.LifecycleReady
		} else {
			actor.lifecycle = agentprotocol.LifecycleUnavailable
			actor.publishBrowserError(attachments, code)
		}
		actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	request, ok := actor.queue.Dequeue()
	if !ok {
		actor.lifecycle = agentprotocol.LifecycleUnavailable
		actor.publishBrowserError(attachments, agentprotocol.ErrorBrokerUnavailable)
		return
	}
	actor.active = &activeTurn{request: request, phase: turnStarting}
	actor.lifecycle = agentprotocol.LifecycleConnecting
	actor.publishShared(attachments, agentprotocol.QueuePayload{Items: actor.queue.Items()})
	actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
	actor.startSubmitWorker(results, request)
}
