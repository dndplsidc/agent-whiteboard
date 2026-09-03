package broker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

type conversation struct {
	identity                 statepkg.Identity
	mapping                  statepkg.Mapping
	state                    StateStore
	attachments              AttachmentStore
	driver                   provider.Driver
	retainSession            func(*sessionHandle)
	session                  *sessionHandle
	generation               uint64
	factory                  *EventFactory
	replay                   *ReplayLog
	requests                 chan any
	done                     chan struct{}
	clock                    common.Clock
	timers                   TimerFactory
	idleTimeout              time.Duration
	resource                 protocol.Resource
	contextDigest            string
	contextState             protocol.ContextState
	lifecycle                protocol.LifecycleState
	catalog                  []protocol.CatalogModel
	domainCatalog            provider.ModelCatalog
	settingsState            protocol.SettingsState
	settingsCapable          bool
	effectiveSettings        *provider.ExecutionSettings
	effectivePresentation    *provider.ModelPresentation
	skillsState              *protocol.SkillsState
	skills                   []protocol.SkillDescriptor
	maxSelectedSkills        *int
	supportsCompact          bool
	busyPolicy               protocol.BusyTurnPolicy
	queue                    *Queue
	commands                 commandLedger
	pendingInteractions      map[string]*pendingInteraction
	nextInteractionToken     uint64
	interactionCallBudget    chan struct{}
	pendingInteractionBytes  int
	active                   *activeTurn
	compact                  *activeCompact
	workerSettled            chan struct{}
	workerKind               providerWorkerKind
	workerCommandID          string
	workerClientID           string
	workerResolved           bool
	shutdownAttempt          *actorShutdown
	deferredInterrupt        *deferredInterrupt
	lifecycleCtx             context.Context
	recoveryCancel           context.CancelFunc
	recoveryResults          chan<- recoveryWorkerResult
	recoveryActive           bool
	recoveryAttempted        uint64
	recoveryUnavailable      bool
	recoveryPending          bool
	recoveryPendingTrigger   recoveryTrigger
	deferredObserve          *deferredObservation
	stopping                 bool
	dispatchBlocked          bool
	dispatchPending          bool
	shutdownTimeout          time.Duration
	startHandoff             func(handoffRequest, chan<- handoffResult) bool
	handoffActive            bool
	handoffFailed            bool
	closeRequested           bool
	afterProviderEventsClose func()

	closeMu  sync.Mutex
	activate sync.Once
	start    chan struct{}
	closed   atomic.Bool
	retired  atomic.Bool
}

type queuedAttachmentEvent struct {
	event protocol.Event
	bytes int
}

type clientAttachment struct {
	clientID string
	events   chan protocol.Event
	detached chan struct{}
	stop     chan struct{}
	wake     chan struct{}
	pumpDone chan struct{}

	mu       sync.Mutex
	queue    []queuedAttachmentEvent
	bytes    int
	stopped  bool
	closing  bool
	stopOnce sync.Once
}

func newAttachment(clientID string, initial []protocol.Event) (*clientAttachment, error) {
	item := &clientAttachment{
		clientID: clientID, events: make(chan protocol.Event), detached: make(chan struct{}),
		stop: make(chan struct{}), wake: make(chan struct{}, 1), pumpDone: make(chan struct{}),
	}
	for _, event := range initial {
		encoded, err := protocol.EncodeEvent(event)
		if err != nil || len(item.queue)+1 > MaxReplayEvents || item.bytes+len(encoded) > MaxReplayBytes {
			return nil, errors.New("invalid attachment replay")
		}
		item.queue = append(item.queue, queuedAttachmentEvent{event: cloneEvent(event), bytes: len(encoded)})
		item.bytes += len(encoded)
	}
	go item.pump()
	if len(item.queue) != 0 {
		item.signal()
	}
	return item, nil
}

func (item *clientAttachment) signal() {
	select {
	case item.wake <- struct{}{}:
	default:
	}
}

func (item *clientAttachment) enqueue(event protocol.Event) bool {
	encoded, err := protocol.EncodeEvent(event)
	if err != nil {
		return false
	}
	item.mu.Lock()
	if item.stopped || item.closing || len(item.queue)+1 > MaxReplayEvents || item.bytes+len(encoded) > MaxReplayBytes {
		item.mu.Unlock()
		return false
	}
	item.queue = append(item.queue, queuedAttachmentEvent{event: cloneEvent(event), bytes: len(encoded)})
	item.bytes += len(encoded)
	item.mu.Unlock()
	item.signal()
	return true
}

func (item *clientAttachment) pump() {
	defer close(item.pumpDone)
	defer close(item.detached)
	defer close(item.events)
	for {
		item.mu.Lock()
		if item.stopped || (item.closing && len(item.queue) == 0) {
			item.mu.Unlock()
			return
		}
		if len(item.queue) == 0 {
			item.mu.Unlock()
			select {
			case <-item.wake:
				continue
			case <-item.stop:
				return
			}
		}
		entry := item.queue[0]
		item.mu.Unlock()

		select {
		case item.events <- cloneEvent(entry.event):
			item.mu.Lock()
			if len(item.queue) != 0 {
				item.queue[0] = queuedAttachmentEvent{}
				item.queue = item.queue[1:]
				item.bytes -= entry.bytes
			}
			item.mu.Unlock()
		case <-item.stop:
			return
		}
	}
}

func (item *clientAttachment) finish() {
	item.stopOnce.Do(func() {
		item.mu.Lock()
		item.stopped = true
		item.mu.Unlock()
		close(item.stop)
	})
	<-item.pumpDone
}

// finishAfterDrain closes an attachment after already-published handoff events
// have been delivered. A slow browser is forcibly detached after the bound.
func (item *clientAttachment) finishAfterDrain(timeout time.Duration) {
	item.mu.Lock()
	if !item.stopped {
		item.closing = true
	}
	item.mu.Unlock()
	item.signal()
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-item.pumpDone:
			return
		case <-timer.C:
			item.finish()
		}
	}()
}

type attachRequest struct {
	ctx           context.Context
	clientID      string
	replayAfter   string
	resource      protocol.Resource
	contextDigest string
	response      chan attachResponse
}
type attachResponse struct {
	attachment *clientAttachment
	err        error
}
type detachRequest struct {
	attachment *clientAttachment
	ack        chan struct{}
}
type commandRequest struct {
	ctx        context.Context
	attachment *clientAttachment
	command    protocol.Command
	response   chan commandResponse
}
type commandResponse struct {
	event protocol.Event
	err   error
}
type closeConversationRequest struct {
	ctx      context.Context
	response chan error
}
type deferredObservation struct {
	resource protocol.Resource
	digest   string
}

type providerWorkerKind uint8

const (
	providerWorkerNone providerWorkerKind = iota
	providerWorkerSubmit
	providerWorkerInterrupt
	providerWorkerHistory
	providerWorkerArchive
	providerWorkerCompact
)

type shutdownWorkerResult struct {
	generation uint64
	response   chan error
	cancel     context.CancelFunc
	err        error
}

const attachmentSweepInterval = 5 * time.Minute

func newConversation(identity statepkg.Identity, mapping statepkg.Mapping, session *sessionHandle, state StateStore, attachments AttachmentStore, driver provider.Driver, retainSession func(*sessionHandle), ids common.IDGenerator, clock common.Clock, timers TimerFactory, lifecycleCtx context.Context, idleTimeout, shutdownTimeout time.Duration) (*conversation, error) {
	if mapping.Validate(identity) != nil || mapping.Current == nil || session == nil || common.IsNil(session.session) || common.IsNil(state) || common.IsNil(driver) || retainSession == nil || common.IsNil(clock) || common.IsNil(timers) || lifecycleCtx == nil || idleTimeout <= 0 || shutdownTimeout <= 0 {
		return nil, errors.New("invalid conversation actor")
	}
	factory, err := NewEventFactory(mapping.Current.ConversationID, ids, clock)
	if err != nil {
		return nil, err
	}
	domainCatalog, err := loadModelCatalog(lifecycleCtx, session.session)
	if err != nil {
		return nil, err
	}
	catalog, err := protocolCatalog(domainCatalog)
	if err != nil {
		return nil, err
	}
	actor := &conversation{
		identity: identity, mapping: cloneMapping(mapping), state: state, attachments: attachments, driver: driver, retainSession: retainSession,
		session: session, generation: 1,
		factory: factory, replay: NewReplayLog(), requests: make(chan any),
		done: make(chan struct{}), start: make(chan struct{}), clock: clock, timers: timers, idleTimeout: idleTimeout,
		contextState: protocol.ContextPending, catalog: catalog, domainCatalog: domainCatalog,
		lifecycle: protocol.LifecycleReady, queue: NewQueue(), commands: newCommandLedger(), skills: []protocol.SkillDescriptor{},
		pendingInteractions:   make(map[string]*pendingInteraction),
		interactionCallBudget: make(chan struct{}, maxPendingInteractions),
		lifecycleCtx:          lifecycleCtx, shutdownTimeout: shutdownTimeout,
	}
	actor.loadSessionFeatures(lifecycleCtx)
	if settingsSession, ok := session.session.(provider.SettingsSession); ok {
		settings, presentation, discoveryErr := settingsSession.EffectiveSettings(lifecycleCtx)
		if discoveryErr != nil || settings.Validate() != nil || presentation.Validate() != nil || !domainCatalog.Compatibility(settings).Compatible {
			return nil, errors.New("settings-capable session lacks authoritative effective settings")
		}
		actor.settingsCapable = true
		actor.applyEffectiveSettings(settings, presentation)
		current := actor.mapping.Current
		if current.Settings == nil || current.Presentation == nil || *current.Settings != settings || *current.Presentation != presentation {
			if !actor.persistEffectiveSettings(settings, presentation) {
				return nil, errors.New("authoritative effective settings could not be persisted")
			}
		}
	}
	if actor.mapping.Current.Observed != nil {
		actor.contextDigest = actor.mapping.Current.Observed.Digest
		actor.contextState = protocol.ContextPending
	} else if actor.mapping.Current.Committed != nil {
		actor.contextDigest = actor.mapping.Current.Committed.Digest
		actor.contextState = protocol.ContextUnchanged
	}
	go actor.run()
	return actor, nil
}

func (actor *conversation) activateRun() {
	actor.activate.Do(func() { close(actor.start) })
}

func (actor *conversation) run() {
	<-actor.start
	providerEvents := actor.session.events
	attachments := make(map[*clientAttachment]struct{})
	shutdownResults := make(chan shutdownWorkerResult, 1)
	turnResults := make(chan turnWorkerResult, 1)
	historyResults := make(chan historyWorkerResult, 1)
	archiveResults := make(chan archiveWorkerResult, 1)
	interactionResults := make(chan interactionWorkerResult, provider.MaxInteractionAnswers)
	handoffResults := make(chan handoffResult, 1)
	recoveryResults := make(chan recoveryWorkerResult, 1)
	actor.recoveryResults = recoveryResults
	shutdownActive := false
	providerClosurePending := false
	providerClosureHasTerminal := false
	var shutdownWaiters []chan error
	var idleTimer Timer
	var attachmentSweepTimer Timer
	if !common.IsNil(actor.attachments) {
		attachmentSweepTimer = actor.timers.NewTimer(attachmentSweepInterval)
	}
	startShutdown := func(request *closeConversationRequest, attempt *actorShutdown, cancel context.CancelFunc) {
		generation := actor.generation
		go func() {
			err := attempt.run(request.ctx, actor.shutdownTimeout)
			shutdownResults <- shutdownWorkerResult{generation: generation, response: request.response, cancel: cancel, err: err}
		}()
	}
	defer func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		if attachmentSweepTimer != nil {
			attachmentSweepTimer.Stop()
		}
		for _, messageID := range actor.queue.imageMessageIDs() {
			_ = actor.releaseMessageImages(messageID)
		}
		actor.queue.Clear()
		if actor.active != nil {
			zeroProviderContext(actor.active.request.Context)
			actor.active = nil
		}
		actor.compact = nil
		for item := range attachments {
			delete(attachments, item)
			item.finish()
		}
		close(actor.done)
	}()
	for {
		if len(attachments) == 0 && len(actor.pendingInteractions) != 0 {
			actor.cancelPendingInteractions(attachments, interactionResults)
		}
		idle := len(attachments) == 0 && actor.active == nil && actor.compact == nil && actor.queue.Empty() && actor.workerSettled == nil && actor.deferredInterrupt == nil && !actor.dispatchBlocked && !actor.recoveryActive && !actor.handoffActive && !shutdownActive && !actor.stopping
		if idle && idleTimer == nil {
			idleTimer = actor.timers.NewTimer(actor.idleTimeout)
		} else if !idle && idleTimer != nil {
			idleTimer.Stop()
			idleTimer = nil
		}
		var idleChannel <-chan time.Time
		if idleTimer != nil {
			idleChannel = idleTimer.C()
		}
		var attachmentSweepChannel <-chan time.Time
		if attachmentSweepTimer != nil {
			attachmentSweepChannel = attachmentSweepTimer.C()
		}
		select {
		case raw := <-actor.requests:
			switch request := raw.(type) {
			case attachRequest:
				actor.handleAttach(attachments, request)
			case detachRequest:
				actor.detach(attachments, request.attachment)
				close(request.ack)
			case commandRequest:
				actor.handleCommand(attachments, turnResults, historyResults, archiveResults, handoffResults, interactionResults, request)
			case closeConversationRequest:
				if actor.handoffActive {
					shutdownWaiters = append(shutdownWaiters, request.response)
					actor.closeRequested = true
					actor.stopping = true
					for item := range attachments {
						actor.detach(attachments, item)
					}
					continue
				}
				if shutdownActive || actor.recoveryActive {
					shutdownWaiters = append(shutdownWaiters, request.response)
					actor.stopping = true
					for item := range attachments {
						actor.detach(attachments, item)
					}
					if actor.recoveryCancel != nil {
						actor.recoveryCancel()
					}
					continue
				}
				actor.stopping = true
				for item := range attachments {
					actor.detach(attachments, item)
				}
				if actor.shutdownAttempt == nil {
					actor.shutdownAttempt = newActorShutdown(actor.session, actor.workerSettled)
				}
				shutdownActive = true
				startShutdown(&request, actor.shutdownAttempt, nil)
			}
		case result := <-turnResults:
			actor.handleTurnResult(attachments, turnResults, result)
			if providerClosurePending && actor.workerSettled == nil {
				trigger := recoveryTriggerClosure
				if providerClosureHasTerminal {
					trigger = recoveryTriggerTerminal
				}
				providerClosurePending = false
				providerClosureHasTerminal = false
				actor.startRecovery(attachments, recoveryResults, trigger)
			}
		case result := <-historyResults:
			actor.handleHistoryResult(attachments, turnResults, result)
		case result := <-archiveResults:
			actor.handleArchiveResult(attachments, recoveryResults, result)
		case result := <-interactionResults:
			actor.handleInteractionResult(attachments, result)
		case result := <-handoffResults:
			exit, needsShutdown := actor.handleHandoffResult(attachments, result)
			if exit {
				actor.closed.Store(true)
				for _, waiter := range shutdownWaiters {
					waiter <- nil
				}
				return
			}
			if needsShutdown && !shutdownActive {
				if actor.shutdownAttempt == nil {
					actor.shutdownAttempt = newActorShutdown(actor.session, actor.workerSettled)
				}
				response := make(chan error, 1)
				if len(shutdownWaiters) != 0 {
					response = shutdownWaiters[0]
					shutdownWaiters = shutdownWaiters[1:]
				}
				shutdownCtx, cancel := context.WithTimeout(context.Background(), actor.shutdownTimeout)
				request := &closeConversationRequest{ctx: shutdownCtx, response: response}
				shutdownActive = true
				startShutdown(request, actor.shutdownAttempt, cancel)
			}
		case result := <-shutdownResults:
			shutdownActive = false
			if result.cancel != nil {
				result.cancel()
			}
			if result.generation != actor.generation {
				result.response <- NewBrokerError(protocol.ErrorProviderProtocolFailure)
				continue
			}
			if result.err != nil {
				failure := NewBrokerError(protocol.ErrorProviderProtocolFailure)
				result.response <- failure
				for _, waiter := range shutdownWaiters {
					waiter <- failure
				}
				shutdownWaiters = nil
				continue
			}
			actor.closed.Store(true)
			result.response <- nil
			for _, waiter := range shutdownWaiters {
				waiter <- nil
			}
			return
		case result := <-recoveryResults:
			if result.generation != actor.generation || !actor.recoveryActive {
				if result.handle != nil {
					actor.retainSession(result.handle)
				}
				continue
			}
			actor.recoveryActive = false
			if actor.recoveryCancel != nil {
				actor.recoveryCancel()
				actor.recoveryCancel = nil
			}
			var deferredObservationErr error
			if result.err == nil && result.handle != nil {
				actor.session = result.handle
				actor.mapping = result.mapping
				actor.generation++
				actor.shutdownAttempt = nil
				providerEvents = result.handle.events
				actor.refreshContextFromMapping()
				actor.loadSessionFeatures(actor.lifecycleCtx)
				if !actor.reloadSessionSettings() {
					result.err = errors.New("recovered provider settings unavailable")
				}
				if !actor.stopping {
					deferredObservationErr = actor.applyDeferredObservation(attachments)
				}
			}
			if actor.stopping {
				if result.err != nil {
					providerEvents = nil
				}
				if actor.shutdownAttempt == nil {
					actor.shutdownAttempt = newActorShutdown(actor.session, actor.workerSettled)
				}
				if len(shutdownWaiters) != 0 {
					response := shutdownWaiters[0]
					shutdownWaiters = shutdownWaiters[1:]
					shutdownCtx, cancel := context.WithTimeout(context.Background(), actor.shutdownTimeout)
					request := &closeConversationRequest{ctx: shutdownCtx, response: response}
					shutdownActive = true
					startShutdown(request, actor.shutdownAttempt, cancel)
				}
				continue
			}
			if result.err != nil {
				providerEvents = nil
				actor.deferredObserve = nil
				actor.recoveryUnavailable = true
				actor.lifecycle = protocol.LifecycleUnavailable
				actor.publishBrowserError(attachments, protocol.ErrorProviderRecoveryFailed)
				actor.publishShared(attachments, actor.lifecyclePayload())
				continue
			}
			if deferredObservationErr != nil {
				actor.recoveryUnavailable = true
				actor.lifecycle = protocol.LifecycleUnavailable
				actor.publishBrowserError(attachments, protocol.ErrorProviderRecoveryFailed)
				actor.publishShared(attachments, actor.lifecyclePayload())
				continue
			}
			actor.recoveryUnavailable = false
			actor.dispatchBlocked = false
			actor.lifecycle = protocol.LifecycleReady
			if actor.queue.Empty() {
				actor.publishShared(attachments, actor.lifecyclePayload())
			} else {
				actor.dispatchNext(attachments, turnResults)
			}
		case <-idleChannel:
			idleTimer = nil
			if len(attachments) != 0 || actor.active != nil || actor.compact != nil || !actor.queue.Empty() || actor.workerSettled != nil || actor.deferredInterrupt != nil || actor.dispatchBlocked || actor.recoveryActive || actor.handoffActive || shutdownActive || actor.stopping {
				continue
			}
			actor.stopping = true
			actor.retired.Store(true)
			if actor.shutdownAttempt == nil {
				actor.shutdownAttempt = newActorShutdown(actor.session, nil)
			}
			shutdownActive = true
			idleShutdownCtx, cancel := context.WithTimeout(context.Background(), actor.shutdownTimeout)
			request := &closeConversationRequest{ctx: idleShutdownCtx, response: make(chan error, 1)}
			startShutdown(request, actor.shutdownAttempt, cancel)
		case <-attachmentSweepChannel:
			_ = actor.attachments.Sweep(actor.lifecycleCtx, actor.mapping.Current.ConversationID)
			attachmentSweepTimer = actor.timers.NewTimer(attachmentSweepInterval)
		case event, open := <-providerEvents:
			if !open {
				providerEvents = nil
				if actor.afterProviderEventsClose != nil {
					actor.afterProviderEventsClose()
				}
				if actor.workerSettled != nil && actor.workerKind == providerWorkerSubmit && actor.active != nil && actor.active.phase == turnStarting {
					for _, pending := range actor.active.pendingEvents {
						providerClosureHasTerminal = providerClosureHasTerminal || providerEventTerminal(pending)
					}
					if providerClosureHasTerminal {
						// An authoritative buffered terminal must settle after Submit
						// acceptance and before closure recovery. Otherwise a closed
						// channel can reject an already accepted prompt.
						providerClosurePending = true
						continue
					}
				}
				trigger := recoveryTriggerClosure
				if actor.active == nil && actor.lifecycle == protocol.LifecycleInterrupted {
					trigger = recoveryTriggerTerminal
				}
				actor.startRecovery(attachments, recoveryResults, trigger)
				continue
			}
			if actor.recoveryActive {
				continue
			}
			actor.handleProviderEvent(attachments, turnResults, event)
		}
	}
}

func (actor *conversation) beginProviderWorker(kind providerWorkerKind, commandID, clientID string) {
	actor.workerSettled = make(chan struct{})
	actor.workerKind = kind
	actor.workerCommandID = commandID
	actor.workerClientID = clientID
	actor.workerResolved = false
}

func (actor *conversation) settleProviderWorker() (chan struct{}, bool) {
	settled := actor.workerSettled
	resolved := actor.workerResolved
	actor.workerSettled = nil
	actor.workerKind = providerWorkerNone
	actor.workerCommandID = ""
	actor.workerClientID = ""
	actor.workerResolved = false
	return settled, resolved
}

func (actor *conversation) deferObservation(resource protocol.Resource, digest string) error {
	if validateProtocolResource(resource) != nil || !validDigest(digest) {
		return NewBrokerError(protocol.ErrorBoardRevisionMalformed)
	}
	var latestTime time.Time
	var latestDigest string
	if actor.mapping.Current != nil {
		if latest := latestRevision(actor.mapping.Current); latest != nil {
			latestTime = latest.SourceUpdatedAt
			latestDigest = latest.Digest
		}
	}
	if actor.deferredObserve != nil && (latestTime.IsZero() || actor.deferredObserve.resource.UpdatedAt.After(latestTime)) {
		latestTime = actor.deferredObserve.resource.UpdatedAt
		latestDigest = actor.deferredObserve.digest
	}
	switch {
	case !latestTime.IsZero() && resource.UpdatedAt.Before(latestTime):
		return NewBrokerError(protocol.ErrorBoardRevisionUnavailable)
	case !latestTime.IsZero() && resource.UpdatedAt.Equal(latestTime) && digest != latestDigest:
		return NewBrokerError(protocol.ErrorBoardRevisionMalformed)
	}
	if actor.deferredObserve == nil || resource.UpdatedAt.After(actor.deferredObserve.resource.UpdatedAt) || (resource.UpdatedAt.Equal(actor.deferredObserve.resource.UpdatedAt) && digest == actor.deferredObserve.digest) {
		actor.deferredObserve = &deferredObservation{resource: cloneProtocolResource(resource), digest: digest}
	}
	// A queued context for an older revision must never cross the recovery
	// boundary, even if durable observation of the replacement later fails.
	actor.queue.discardContext()
	return nil
}

func (actor *conversation) applyDeferredObservation(attachments map[*clientAttachment]struct{}) error {
	deferred := actor.deferredObserve
	actor.deferredObserve = nil
	if deferred == nil {
		return nil
	}
	_, changed, event, err := actor.observeAttach(deferred.resource, deferred.digest)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	actor.queue.discardContext()
	if err := actor.replay.Append(event); err != nil {
		return NewBrokerError(protocol.ErrorBrokerUnavailable)
	}
	for attached := range attachments {
		actor.send(attachments, attached, event)
	}
	return nil
}

func (actor *conversation) handleAttach(attachments map[*clientAttachment]struct{}, request attachRequest) {
	if actor.handoffActive {
		request.response <- attachResponse{err: NewBrokerError(protocol.ErrorInvalidState)}
		return
	}
	if actor.retired.Load() {
		request.response <- attachResponse{err: errActorRetired}
		return
	}
	if err := request.ctx.Err(); err != nil {
		request.response <- attachResponse{err: err}
		return
	}
	var replayed []protocol.Event
	if request.replayAfter != "" {
		var replayErr error
		replayed, replayErr = actor.replay.Replay(request.clientID, request.replayAfter)
		if replayErr != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorReplayWindowUnavailable)}
			return
		}
	}
	contextState := actor.contextState
	contextChanged := false
	var contextEvent protocol.Event
	var err error
	if actor.recoveryActive || (actor.workerSettled != nil && actor.workerKind == providerWorkerArchive) {
		if err = actor.deferObservation(request.resource, request.contextDigest); err != nil {
			request.response <- attachResponse{err: err}
			return
		}
	} else if actor.recoveryUnavailable {
		if validateProtocolResource(request.resource) != nil || !validDigest(request.contextDigest) {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBoardRevisionMalformed)}
			return
		}
		if actor.resource.ID == "" || request.contextDigest != actor.contextDigest || !request.resource.UpdatedAt.Equal(actor.resource.UpdatedAt) {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorStateRepairFailed)}
			return
		}
	} else {
		contextState, contextChanged, contextEvent, err = actor.observeAttach(request.resource, request.contextDigest)
		if err != nil {
			request.response <- attachResponse{err: err}
			return
		}
	}

	initial := replayed
	if contextChanged {
		actor.queue.discardContext()
		if err := actor.replay.Append(contextEvent); err != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
			return
		}
		for attached := range attachments {
			actor.send(attachments, attached, contextEvent)
		}
		snapshot, snapshotErr := actor.snapshot(contextState)
		if snapshotErr != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
			return
		}
		if appendErr := actor.replay.AppendForClient(request.clientID, snapshot); appendErr != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
			return
		}
		if request.replayAfter == "" {
			initial = []protocol.Event{snapshot}
		} else {
			initial, err = actor.replay.Replay(request.clientID, request.replayAfter)
			if err != nil {
				request.response <- attachResponse{err: NewBrokerError(protocol.ErrorReplayWindowUnavailable)}
				return
			}
		}
	} else if request.replayAfter == "" || len(initial) == 0 {
		snapshot, snapshotErr := actor.snapshot(contextState)
		if snapshotErr != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
			return
		}
		if appendErr := actor.replay.AppendForClient(request.clientID, snapshot); appendErr != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
			return
		}
		initial = append(initial, snapshot)
	}
	seenInteractions := make(map[string]struct{})
	for _, event := range initial {
		if payload, ok := event.Payload.(protocol.InteractionRequestPayload); ok {
			seenInteractions[payload.RequestID] = struct{}{}
		}
	}
	for requestID, pending := range actor.pendingInteractions {
		if _, seen := seenInteractions[requestID]; seen {
			continue
		}
		payload, conversionErr := interactionRequestFromProvider(pending.request)
		if conversionErr != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorProviderMalformedStream)}
			return
		}
		event, eventErr := actor.factory.New(payload)
		if eventErr != nil || actor.replay.AppendForClient(request.clientID, event) != nil {
			request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
			return
		}
		initial = append(initial, event)
	}
	item, err := newAttachment(request.clientID, initial)
	if err != nil {
		request.response <- attachResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
		return
	}
	attachments[item] = struct{}{}
	request.response <- attachResponse{attachment: item}
}

func (actor *conversation) snapshot(contextState protocol.ContextState) (protocol.Event, error) {
	settingsState, effectiveSettings, catalog := actor.settingsSnapshot()
	var skillsState *protocol.SkillsState
	if actor.skillsState != nil {
		state := *actor.skillsState
		skillsState = &state
	}
	payload := protocol.SnapshotPayload{
		Lifecycle: actor.lifecycle, Queue: actor.queue.Items(),
		ContextState: contextState, ActiveWork: actor.activeWork(), SupportsImages: actor.session.capabilities.Images,
		SupportsArchiveDelete: supportsArchiveDelete(actor.driver),
		SettingsState:         settingsState, EffectiveSettings: effectiveSettings, Catalog: catalog,
		SkillsState: skillsState, Skills: append([]protocol.SkillDescriptor{}, actor.skills...), MaxSelectedSkills: cloneInt(actor.maxSelectedSkills), SupportsCompact: actor.supportsCompact,
		BusyPolicy: actor.busyPolicy, ComposerAdmission: actor.composerAdmission(),
	}
	wireProvider, err := providerNameFromDomain(actor.identity.Provider)
	if err != nil {
		return protocol.Event{}, errors.New("invalid provider snapshot identity")
	}
	if validationErr := payload.ValidateForProvider(wireProvider); validationErr != nil {
		return protocol.Event{}, errors.New("invalid provider snapshot state")
	}
	return actor.factory.New(payload)
}

func supportsArchiveDelete(driver provider.Driver) bool {
	deleter, ok := driver.(provider.NativeSessionDeleter)
	return ok && !common.IsNil(deleter)
}

func (actor *conversation) detach(attachments map[*clientAttachment]struct{}, item *clientAttachment) {
	if _, exists := attachments[item]; exists {
		delete(attachments, item)
	}
	item.finish()
}

func (actor *conversation) handleProviderEvent(attachments map[*clientAttachment]struct{}, turnResults chan<- turnWorkerResult, source provider.Event) {
	if source.Validate() != nil {
		actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
		return
	}
	if source.Kind == provider.EventSkillCatalog {
		actor.publishSkillCatalog(attachments, source)
		return
	}
	// An uncorrelated settings event describes authoritative process-scoped
	// state. It is independent of prompt admission, including while a submit is
	// starting, and must never be buffered as evidence that the prompt was sent.
	if source.Kind == provider.EventSettings && source.TurnID == "" {
		actor.publishSettingsEvent(attachments, source)
		return
	}
	if source.Kind == provider.EventCompact {
		if actor.compact != nil && actor.compact.phase == compactStarting && source.Compact != nil && source.Compact.WorkID == actor.compact.request.WorkID && actor.compact.pendingTerminal == nil {
			copyOfSource := source
			copyOfCompact := *source.Compact
			copyOfSource.Compact = &copyOfCompact
			actor.compact.pendingTerminal = &copyOfSource
			return
		}
		actor.publishCompactTerminal(attachments, source)
		return
	}
	if !actor.providerEventMatchesActive(source) {
		actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
		return
	}
	terminal := providerEventTerminal(source)
	if source.Kind == provider.EventTerminalFailure && source.TurnID == "" {
		actor.publishProviderEvent(attachments, turnResults, source)
		return
	}
	if actor.active != nil && ((actor.active.phase == turnStarting && actor.providerEventTargetsActive(source)) || (actor.active.phase == turnInterrupting && terminal)) {
		actor.bufferProviderEvent(source)
		return
	}
	actor.publishProviderEvent(attachments, turnResults, source)
}

func (actor *conversation) publishProviderEvent(attachments map[*clientAttachment]struct{}, turnResults chan<- turnWorkerResult, source provider.Event) {
	if source.Kind == provider.EventSettings {
		actor.publishSettingsEvent(attachments, source)
		return
	}
	if source.Kind == provider.EventInteractionRequest {
		if _, capable := actor.session.session.(provider.InteractiveSession); !capable {
			actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
			return
		}
		if actor.rememberInteraction(*source.Interaction) != nil {
			actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
			return
		}
	}
	if source.Kind == provider.EventInteractionResolved {
		pending := actor.pendingInteractions[source.Resolution.RequestID]
		if pending == nil {
			return
		}
		if pending.request.Kind != source.Resolution.Kind {
			actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
			return
		}
		actor.retireInteraction(attachments, source.Resolution.RequestID, pending.token, "", false, protocol.ErrorInvalidState)
	}
	if providerEventTerminal(source) {
		actor.expirePendingInteractions(attachments)
	}
	var event protocol.Event
	var err error
	if source.Kind == provider.EventUserMessage {
		var images []protocol.ImageDescriptor
		images, err = actor.messageImages(source.MessageID)
		if err == nil {
			var content protocol.MessageContent
			content, err = messageContentFromProvider(source.Content, images)
			if err == nil {
				event, err = actor.factory.New(protocol.UserMessagePayload{TurnID: source.TurnID, MessageID: source.MessageID, Content: content, Images: ordinaryImageDescriptors(source.Content, images), CreatedAt: source.Timestamp})
			}
		}
	} else {
		event, err = actor.factory.FromProvider(source)
	}
	if err != nil {
		if source.Kind == provider.EventInteractionRequest {
			actor.forgetInteraction(source.Interaction.ID)
		}
		code := protocol.ErrorProviderMalformedStream
		if source.Kind == provider.EventUserMessage {
			code = protocol.ErrorImageStorageFailure
		}
		actor.publishBrowserError(attachments, code)
		return
	}
	if actor.replay.Append(event) != nil {
		if source.Kind == provider.EventInteractionRequest {
			actor.forgetInteraction(source.Interaction.ID)
		}
		return
	}
	for item := range attachments {
		actor.send(attachments, item, event)
	}
	if source.Kind == provider.EventTerminalFailure && source.TurnID == "" {
		actor.startRecovery(attachments, actor.recoveryResults, recoveryTriggerTerminal)
		return
	}
	if providerEventTerminal(source) && actor.active != nil {
		lifecycle := protocol.LifecycleReady
		if source.Kind == provider.EventInterruption || source.Kind == provider.EventTerminalFailure {
			lifecycle = protocol.LifecycleInterrupted
		}
		actor.finishActive(attachments, turnResults, lifecycle)
	}
}

func providerEventTerminal(source provider.Event) bool {
	return source.Kind == provider.EventCompletion || source.Kind == provider.EventInterruption || source.Kind == provider.EventTerminalFailure
}

func (actor *conversation) bufferProviderEvent(source provider.Event) {
	if actor.active == nil || actor.active.pendingOverflow {
		return
	}
	size := bufferedProviderEventSize(source)
	if len(actor.active.pendingEvents)+1 > MaxReplayEvents || actor.active.pendingBytes+size > MaxReplayBytes {
		actor.active.pendingOverflow = true
		actor.active.pendingEvents = nil
		actor.active.pendingBytes = 0
		return
	}
	actor.active.pendingEvents = append(actor.active.pendingEvents, source)
	actor.active.pendingBytes += size
}

func bufferedProviderEventSize(source provider.Event) int {
	size := len(source.Text) + source.Content.SemanticBytes() + len(source.TurnID) + len(source.MessageID) + 64
	if source.Tool != nil {
		size += len(source.Tool.ID) + len(source.Tool.TurnID) + len(source.Tool.Kind) + len(source.Tool.Status) + len(source.Tool.Title) + len(source.Tool.Summary) + len(source.Tool.Detail)
	}
	if source.Resolution != nil {
		size += len(source.Resolution.RequestID) + len(source.Resolution.Kind) + len(source.Resolution.OptionID) + 16
	}
	if source.Interaction == nil {
		return size
	}
	return size + interactionRequestSize(*source.Interaction)
}

func (actor *conversation) flushPendingProviderEvents(attachments map[*clientAttachment]struct{}, turnResults chan<- turnWorkerResult) {
	if actor.active == nil {
		return
	}
	events := actor.active.pendingEvents
	overflow := actor.active.pendingOverflow
	actor.active.pendingEvents = nil
	actor.active.pendingBytes = 0
	actor.active.pendingOverflow = false
	if overflow {
		actor.dispatchBlocked = true
		actor.lifecycle = protocol.LifecycleUnavailable
		actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
		actor.publishShared(attachments, actor.lifecyclePayload())
	}
	for _, source := range events {
		if actor.active == nil || !actor.providerEventMatchesActive(source) {
			actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
			continue
		}
		actor.publishProviderEvent(attachments, turnResults, source)
	}
}

func (actor *conversation) providerEventMatchesActive(source provider.Event) bool {
	if source.Kind == provider.EventSettings {
		return actor.active != nil && source.TurnID == actor.active.request.TurnID
	}
	if source.Kind == provider.EventInteractionResolved {
		return true
	}
	if source.Kind == provider.EventInteractionRequest && source.TurnID == "" {
		return true
	}
	if source.Kind == provider.EventActivity && source.TurnID == "" {
		return true
	}
	if source.Kind == provider.EventTerminalFailure && source.TurnID == "" {
		return true
	}
	return actor.providerEventTargetsActive(source)
}

func (actor *conversation) providerEventTargetsActive(source provider.Event) bool {
	if actor.active == nil {
		return false
	}
	return source.TurnID == actor.active.request.TurnID || (source.Kind == provider.EventTerminalFailure && source.TurnID == "")
}

func (actor *conversation) send(attachments map[*clientAttachment]struct{}, item *clientAttachment, event protocol.Event) {
	if !item.enqueue(event) {
		actor.detach(attachments, item)
	}
}

func (actor *conversation) attach(ctx context.Context, clientID, replayAfter string, resource protocol.Resource, contextDigest string) (*Connection, error) {
	if actor.retired.Load() {
		return nil, errActorRetired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := attachRequest{ctx: ctx, clientID: clientID, replayAfter: replayAfter, resource: cloneProtocolResource(resource), contextDigest: contextDigest, response: make(chan attachResponse, 1)}
	select {
	case actor.requests <- request:
	case <-actor.done:
		if actor.retired.Load() {
			return nil, errActorRetired
		}
		return nil, NewBrokerError(protocol.ErrorBrokerShuttingDown)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case response := <-request.response:
		if err := ctx.Err(); err != nil {
			if response.attachment != nil {
				connection := &Connection{actor: actor, attachment: response.attachment, conversationID: actor.mapping.Current.ConversationID, clientID: clientID}
				cleanupCtx, cancel := context.WithTimeout(context.Background(), actor.shutdownTimeout)
				_ = connection.Close(cleanupCtx)
				cancel()
			}
			return nil, err
		}
		if response.err != nil {
			return nil, response.err
		}
		return &Connection{actor: actor, attachment: response.attachment, conversationID: actor.mapping.Current.ConversationID, clientID: clientID}, nil
	case <-ctx.Done():
		response := <-request.response
		if response.attachment != nil {
			connection := &Connection{actor: actor, attachment: response.attachment, conversationID: actor.mapping.Current.ConversationID, clientID: clientID}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), actor.shutdownTimeout)
			_ = connection.Close(cleanupCtx)
			cancel()
		}
		return nil, ctx.Err()
	}
}

func (actor *conversation) close(ctx context.Context) error {
	if actor.closed.Load() {
		return nil
	}
	actor.closeMu.Lock()
	defer actor.closeMu.Unlock()
	if actor.closed.Load() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	request := closeConversationRequest{ctx: ctx, response: make(chan error, 1)}
	select {
	case actor.requests <- request:
	case <-actor.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.response:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-actor.done:
		return nil
	}
}
