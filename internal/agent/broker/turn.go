package broker

import (
	"errors"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
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

type deferredInterrupt struct {
	commandID string
	clientID  string
	turnID    string
}

type turnWorkerResult struct {
	generation uint64
	kind       turnWorkerKind
	turnID     string
	accepted   provider.AcceptedTurn
	commandID  string
	clientID   string
	err        error
}

func (actor *conversation) commandSubmit(attachments map[*clientAttachment]struct{}, turnResults chan<- turnWorkerResult, command protocol.Command, payload protocol.SubmitPayload) (bool, protocol.BrowserErrorCode) {
	if actor.active != nil && (actor.active.request.TurnID == payload.TurnID || actor.active.request.MessageID == payload.MessageID) {
		return false, protocol.ErrorInvalidState
	}
	if actor.queue.ContainsTurnID(payload.TurnID) || actor.queue.ContainsMessageID(payload.MessageID) {
		return false, protocol.ErrorInvalidState
	}
	if (actor.workerSettled != nil && actor.active == nil) || actor.lifecycle == protocol.LifecycleUnavailable || actor.mapping.Current == nil || actor.mapping.Current.PreparedCommit != nil || actor.identity.Provider == provider.NameCodex && actor.settingsState != protocol.SettingsVerified {
		return false, protocol.ErrorInvalidState
	}
	settings, code := validateCommandSettings(actor.identity.Provider, actor.domainCatalog, payload.Settings)
	if code != "" {
		return false, code
	}

	images, descriptors, code := actor.claimTurnImages(command, payload)
	if code != "" {
		return false, code
	}
	releaseOnFailure := func(code protocol.BrowserErrorCode) protocol.BrowserErrorCode {
		if len(images) != 0 && actor.releaseMessageImages(payload.MessageID) != nil {
			return protocol.ErrorImageStorageFailure
		}
		return code
	}
	request, code := actor.convertSubmittedTurn(payload, images, settings)
	if code != "" {
		return false, releaseOnFailure(code)
	}
	if actor.active == nil && actor.queue.Empty() {
		if code := actor.prepareTurn(request); code != "" {
			zeroProviderContext(request.Context)
			return false, releaseOnFailure(code)
		}
		actor.active = &activeTurn{request: request, phase: turnStarting, originCommandID: command.CommandID, originClientID: command.ClientID}
		actor.startSubmitWorker(turnResults, request)
		return true, ""
	}
	if actor.active != nil && actor.active.phase != turnRunning && actor.active.phase != turnInterrupting && actor.active.phase != turnInterruptRequested {
		zeroProviderContext(request.Context)
		return false, releaseOnFailure(protocol.ErrorInvalidState)
	}
	contextForHead := request.Context
	if actor.queue.Empty() {
		contextForHead = nil
	}
	if contextForHead != nil {
		request.Context = nil
	}
	turn := QueuedTurn{TurnID: request.TurnID, MessageID: request.MessageID, Content: request.Content, Images: request.Images, Descriptors: descriptors, Context: request.Context, Settings: request.Settings}
	if request.Settings != nil {
		model, err := actor.domainCatalog.Resolve(*request.Settings)
		if err != nil {
			zeroProviderContext(request.Context)
			return false, releaseOnFailure(protocol.ErrorInvalidModelConfiguration)
		}
		turn.Presentation = &provider.ModelPresentation{ModelDisplayName: model.DisplayName, Selectable: true}
	}
	if err := actor.queue.Enqueue(turn); err != nil {
		zeroProviderContext(request.Context)
		zeroProviderContext(contextForHead)
		if errors.Is(err, ErrQueueFull) {
			return false, releaseOnFailure(protocol.ErrorQueueFull)
		}
		return false, releaseOnFailure(protocol.ErrorInvalidState)
	}
	zeroProviderContext(request.Context)
	if contextForHead != nil {
		if err := actor.queue.attachContextToHead(contextForHead); err != nil {
			_ = actor.queue.Remove(payload.MessageID)
			zeroProviderContext(contextForHead)
			return false, releaseOnFailure(protocol.ErrorInvalidState)
		}
	}
	if !actor.publishShared(attachments, protocol.QueuePayload{Items: actor.queue.Items()}) {
		_ = actor.queue.Remove(payload.MessageID)
		if contextForHead != nil {
			actor.queue.discardContext()
		}
		return false, releaseOnFailure(protocol.ErrorBrokerUnavailable)
	}
	if actor.active == nil {
		actor.dispatchNext(attachments, turnResults)
	}
	return false, ""
}

func (actor *conversation) convertSubmittedTurn(payload protocol.SubmitPayload, images []provider.ImageInput, settings *provider.ExecutionSettings) (provider.TurnRequest, protocol.BrowserErrorCode) {
	content, err := messageContentToProvider(payload.Content)
	if err != nil {
		return provider.TurnRequest{}, protocol.ErrorInvalidCommand
	}
	if !referencesMatchCurrentPage(content, actor.resource, actor.contextDigest) {
		return provider.TurnRequest{}, protocol.ErrorBoardRevisionUnavailable
	}
	contextOwned := (actor.active != nil && actor.active.request.Context != nil) || actor.queue.hasContext
	if actor.contextState != protocol.ContextPending {
		if payload.Context != nil {
			return provider.TurnRequest{}, protocol.ErrorInvalidState
		}
		request := provider.TurnRequest{TurnID: payload.TurnID, MessageID: payload.MessageID, Content: content, Images: images, Settings: settings}
		if request.Validate() != nil {
			return provider.TurnRequest{}, protocol.ErrorInvalidCommand
		}
		return request, ""
	}
	if contextOwned {
		if payload.Context != nil {
			return provider.TurnRequest{}, protocol.ErrorInvalidState
		}
		request := provider.TurnRequest{TurnID: payload.TurnID, MessageID: payload.MessageID, Content: content, Images: images, Settings: settings}
		if request.Validate() != nil {
			return provider.TurnRequest{}, protocol.ErrorInvalidCommand
		}
		return request, ""
	}
	if payload.Context == nil || actor.mapping.Current == nil || actor.mapping.Current.Observed == nil || actor.resource.ID == "" {
		return provider.TurnRequest{}, protocol.ErrorInvalidState
	}
	wireProvider, err := providerNameFromDomain(actor.identity.Provider)
	if err != nil {
		return provider.TurnRequest{}, protocol.ErrorInvalidState
	}
	connected := ConnectIdentity{Origin: actor.identity.Origin, Provider: wireProvider, Resource: actor.resource, ContextDigest: actor.contextDigest}
	converted, err := PageContextToProvider(*payload.Context, connected, actor.identity.Origin)
	if err != nil || string(payload.Context.Revision) != string(actor.mapping.Current.Observed.Revision) || payload.Context.Digest != actor.mapping.Current.Observed.Digest || !payload.Context.Resource.UpdatedAt.Equal(actor.mapping.Current.Observed.SourceUpdatedAt) {
		zeroProviderContext(&converted)
		return provider.TurnRequest{}, protocol.ErrorInvalidState
	}
	request := provider.TurnRequest{TurnID: payload.TurnID, MessageID: payload.MessageID, Content: content, Images: images, Context: &converted, Settings: settings}
	if request.Validate() != nil {
		zeroProviderContext(&converted)
		return provider.TurnRequest{}, protocol.ErrorInvalidCommand
	}
	return request, ""
}

func (actor *conversation) prepareTurn(request provider.TurnRequest) protocol.BrowserErrorCode {
	if actor.contextState == protocol.ContextPending {
		if request.Context == nil || actor.mapping.Current == nil || actor.mapping.Current.Observed == nil {
			return protocol.ErrorInvalidState
		}
		expected := actor.mapping.Current.Observed
		if request.Context.Digest != expected.Digest || statepkg.RevisionKind(request.Context.Revision) != expected.Revision || !request.Context.Resource.UpdatedAt.Equal(expected.SourceUpdatedAt) {
			return protocol.ErrorBoardRevisionUnavailable
		}
	} else if request.Context != nil {
		return protocol.ErrorInvalidState
	}
	if request.Context == nil {
		return ""
	}
	revision := statepkg.Revision{Digest: request.Context.Digest, Revision: statepkg.RevisionKind(request.Context.Revision), SourceUpdatedAt: request.Context.Resource.UpdatedAt}
	before := cloneMapping(actor.mapping)
	at := actor.clock.Now().UTC()
	target := preparedMapping(before, revision, request.TurnID, at)
	mapping, ok := actor.applyDurableTransition(before, target, at, func(at time.Time) (statepkg.CommitOutcome, error) {
		return actor.state.PrepareCommit(actor.identity, revision, request.TurnID, at)
	})
	if !ok {
		return protocol.ErrorStateRepairFailed
	}
	actor.mapping = mapping
	return ""
}

func (actor *conversation) startSubmitWorker(results chan<- turnWorkerResult, request provider.TurnRequest) {
	actor.beginProviderWorker(providerWorkerSubmit, "", "")
	generation := actor.generation
	go func() {
		preflight, err := actor.session.session.Preflight(actor.lifecycleCtx, provider.PreflightRequest{Turn: request})
		if err == nil {
			err = preflight.Validate()
		}
		if err != nil {
			results <- turnWorkerResult{generation: generation, kind: turnWorkerSubmit, turnID: request.TurnID, err: err}
			return
		}
		accepted, err := actor.session.session.Submit(actor.lifecycleCtx, request)
		if err == nil {
			if accepted.Validate() != nil || accepted.TurnID != request.TurnID {
				err = errors.New("invalid provider accepted turn")
			}
		}
		results <- turnWorkerResult{generation: generation, kind: turnWorkerSubmit, turnID: request.TurnID, accepted: accepted, err: err}
	}()
}

func (actor *conversation) commandInterrupt(results chan<- turnWorkerResult, command protocol.Command, payload protocol.TurnReferencePayload) (bool, protocol.BrowserErrorCode) {
	if actor.active == nil || actor.active.phase != turnRunning || actor.active.accepted == nil || actor.active.request.TurnID != payload.TurnID || actor.deferredInterrupt != nil {
		return false, protocol.ErrorInvalidState
	}
	if actor.workerSettled != nil {
		actor.deferredInterrupt = &deferredInterrupt{commandID: command.CommandID, clientID: command.ClientID, turnID: payload.TurnID}
		return true, ""
	}
	actor.startInterruptWorker(results, command.CommandID, command.ClientID)
	return true, ""
}

func (actor *conversation) startInterruptWorker(results chan<- turnWorkerResult, commandID, clientID string) {
	accepted := *actor.active.accepted
	actor.active.phase = turnInterrupting
	actor.beginProviderWorker(providerWorkerInterrupt, commandID, clientID)
	generation := actor.generation
	go func() {
		err := actor.session.session.Interrupt(actor.lifecycleCtx, accepted)
		results <- turnWorkerResult{generation: generation, kind: turnWorkerInterrupt, turnID: accepted.TurnID, commandID: commandID, clientID: clientID, err: err}
	}()
}

func (actor *conversation) handleTurnResult(attachments map[*clientAttachment]struct{}, results chan<- turnWorkerResult, result turnWorkerResult) {
	settled, resolved := actor.settleProviderWorker()
	if settled != nil {
		defer close(settled)
	}
	if result.generation != actor.generation || resolved {
		return
	}
	if actor.active == nil || actor.active.request.TurnID != result.turnID {
		if result.kind == turnWorkerInterrupt && result.commandID != "" {
			actor.completePendingCommand(attachments, result.commandID, result.clientID, protocol.ErrorTurnInterrupted)
		}
		return
	}
	if result.kind == turnWorkerInterrupt {
		actor.handleInterruptResult(attachments, results, result)
		return
	}
	actor.handleSubmitResult(attachments, results, result)
}

func (actor *conversation) handleSubmitResult(attachments map[*clientAttachment]struct{}, results chan<- turnWorkerResult, result turnWorkerResult) {
	active := actor.active
	if result.err != nil && (len(active.pendingEvents) != 0 || active.pendingOverflow) {
		result.err = provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	zeroProviderContext(active.request.Context)
	active.request.Context = nil
	if result.err != nil {
		code := MapError(result.err).Code()
		if code != protocol.ErrorAcceptanceOutcomeUnknown && len(active.request.Images) != 0 && actor.releaseMessageImages(active.request.MessageID) != nil {
			code = protocol.ErrorImageStorageFailure
		}
		if activeHasPrepared(actor.mapping.Current, active.request.TurnID) && code != protocol.ErrorAcceptanceOutcomeUnknown {
			if !actor.rejectPrepared(active.request.TurnID) {
				code = protocol.ErrorStateRepairFailed
			}
		}
		if code == protocol.ErrorAcceptanceOutcomeUnknown || activeHasPrepared(actor.mapping.Current, active.request.TurnID) {
			actor.contextState = protocol.ContextUnavailable
			active.phase = turnAcceptanceUnknown
			actor.lifecycle = protocol.LifecycleUnavailable
		} else {
			actor.active = nil
			actor.contextState = actor.contextStateFromMapping()
			actor.lifecycle = protocol.LifecycleReady
		}
		if code == protocol.ErrorInvalidModelConfiguration {
			actor.refreshCatalog(attachments)
		}
		actor.publishBrowserError(attachments, code)
		actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
		if active.originCommandID != "" {
			actor.completePendingCommand(attachments, active.originCommandID, active.originClientID, code)
		}
		actor.flushPendingProviderEvents(attachments, results)
		return
	}

	accepted := result.accepted
	if actor.identity.Provider == provider.NameCodex {
		if accepted.Settings == nil || accepted.Presentation == nil || !actor.persistEffectiveSettings(*accepted.Settings, *accepted.Presentation) {
			actor.contextState = protocol.ContextUnavailable
			actor.lifecycle = protocol.LifecycleUnavailable
			actor.publishBrowserError(attachments, protocol.ErrorStateRepairFailed)
			actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
			if active.originCommandID != "" {
				actor.completePendingCommand(attachments, active.originCommandID, active.originClientID, protocol.ErrorStateRepairFailed)
			}
			actor.flushPendingProviderEvents(attachments, results)
			return
		}
		actor.applyEffectiveSettings(*accepted.Settings, *accepted.Presentation)
	}
	active.accepted = &accepted
	active.phase = turnRunning
	if activeHasPrepared(actor.mapping.Current, active.request.TurnID) {
		if !actor.acceptPrepared(active.request.TurnID) {
			actor.contextState = protocol.ContextUnavailable
			actor.lifecycle = protocol.LifecycleUnavailable
			actor.publishBrowserError(attachments, protocol.ErrorStateRepairFailed)
			actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
			if active.originCommandID != "" {
				actor.completePendingCommand(attachments, active.originCommandID, active.originClientID, protocol.ErrorStateRepairFailed)
			}
			actor.flushPendingProviderEvents(attachments, results)
			return
		}
		actor.contextState = protocol.ContextAccepted
		actor.contextDigest = actor.mapping.Current.Committed.Digest
		actor.publishShared(attachments, protocol.ContextPayload{Digest: actor.contextDigest, State: protocol.ContextAccepted})
	}
	actor.lifecycle = protocol.LifecycleResponding
	turnID := active.request.TurnID
	actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle, TurnID: &turnID})
	if actor.identity.Provider == provider.NameCodex {
		actor.publishShared(attachments, protocol.SettingsPayload{
			SettingsState: protocol.SettingsVerified, EffectiveSettings: actor.presentedEffectiveSettings(accepted.Settings, accepted.Presentation),
			Catalog: append([]protocol.CatalogModel{}, actor.catalog...), AcceptedTurnID: &turnID,
		})
	}
	if active.originCommandID != "" {
		actor.completePendingCommand(attachments, active.originCommandID, active.originClientID, "")
		active.originCommandID = ""
		active.originClientID = ""
	}
	actor.flushPendingProviderEvents(attachments, results)
}

func (actor *conversation) handleInterruptResult(attachments map[*clientAttachment]struct{}, results chan<- turnWorkerResult, result turnWorkerResult) {
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
	mapping, ok := actor.applyDurableTransition(before, target, at, func(at time.Time) (statepkg.CommitOutcome, error) {
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
	mapping, ok := actor.applyDurableTransition(before, accepted, at, func(at time.Time) (statepkg.CommitOutcome, error) {
		return actor.state.MarkPreparedAccepted(actor.identity, turnID, at)
	})
	if !ok {
		return false
	}
	actor.mapping = mapping
	before = cloneMapping(mapping)
	at = actor.clock.Now().UTC()
	promoted := promotedMapping(before, at)
	mapping, ok = actor.applyDurableTransition(before, promoted, at, func(at time.Time) (statepkg.CommitOutcome, error) {
		return actor.state.PromotePrepared(actor.identity, turnID, at)
	})
	if ok {
		actor.mapping = mapping
	}
	return ok
}

func (actor *conversation) applyDurableTransition(before, target statepkg.Mapping, at time.Time, operation func(time.Time) (statepkg.CommitOutcome, error)) (statepkg.Mapping, bool) {
	if at.IsZero() {
		return statepkg.Mapping{}, false
	}
	outcome, mutationErr := operation(at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, true
	}
	loaded, class := classifyLoadedState(actor.state, actor.identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, true
	}
	if class != mappingPrecondition || outcome == statepkg.CommitApplied || !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, false
	}
	outcome, mutationErr = operation(at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, true
	}
	loaded, class = classifyLoadedState(actor.state, actor.identity, before, target)
	if class != mappingTarget || !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, false
	}
	return loaded, true
}

func (actor *conversation) contextStateFromMapping() protocol.ContextState {
	if actor.mapping.Current == nil {
		return protocol.ContextUnavailable
	}
	if actor.mapping.Current.PreparedCommit != nil {
		return protocol.ContextUnavailable
	}
	if actor.mapping.Current.Observed != nil {
		return protocol.ContextPending
	}
	if actor.mapping.Current.Committed != nil {
		return protocol.ContextUnchanged
	}
	return protocol.ContextPending
}

func activeHasPrepared(session *statepkg.Session, turnID string) bool {
	return session != nil && session.PreparedCommit != nil && session.PreparedCommit.TurnID == turnID
}

func (actor *conversation) finishActive(attachments map[*clientAttachment]struct{}, results chan<- turnWorkerResult, terminalLifecycle protocol.LifecycleState) {
	if actor.active != nil {
		zeroProviderContext(actor.active.request.Context)
	}
	actor.active = nil
	if actor.dispatchBlocked {
		actor.lifecycle = protocol.LifecycleUnavailable
		actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	if actor.mapping.Current != nil && actor.mapping.Current.PreparedCommit != nil {
		actor.lifecycle = protocol.LifecycleUnavailable
		actor.contextState = protocol.ContextUnavailable
		actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	if actor.stopping || actor.queue.Empty() {
		actor.lifecycle = terminalLifecycle
		actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	if actor.workerSettled != nil {
		actor.dispatchPending = true
		actor.lifecycle = protocol.LifecycleReady
		actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	actor.dispatchNext(attachments, results)
}

func (actor *conversation) dispatchNext(attachments map[*clientAttachment]struct{}, results chan<- turnWorkerResult) {
	candidate, ok := actor.queue.peek()
	if !ok {
		return
	}
	if code := actor.prepareTurn(candidate); code != "" {
		if code == protocol.ErrorInvalidState || code == protocol.ErrorBoardRevisionUnavailable {
			if code == protocol.ErrorBoardRevisionUnavailable {
				actor.queue.discardContext()
				actor.publishBrowserError(attachments, code)
			}
			actor.lifecycle = protocol.LifecycleReady
		} else {
			actor.lifecycle = protocol.LifecycleUnavailable
			actor.publishBrowserError(attachments, code)
		}
		actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
		return
	}
	request, ok := actor.queue.Dequeue()
	if !ok {
		actor.lifecycle = protocol.LifecycleUnavailable
		actor.publishBrowserError(attachments, protocol.ErrorBrokerUnavailable)
		return
	}
	actor.active = &activeTurn{request: request, phase: turnStarting}
	actor.lifecycle = protocol.LifecycleConnecting
	actor.publishShared(attachments, protocol.QueuePayload{Items: actor.queue.Items()})
	actor.publishShared(attachments, protocol.LifecyclePayload{State: actor.lifecycle})
	actor.startSubmitWorker(results, request)
}
