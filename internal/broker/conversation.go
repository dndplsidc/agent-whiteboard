package broker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type conversation struct {
	identity          agentstate.Identity
	mapping           agentstate.Mapping
	state             StateStore
	session           provider.Session
	factory           *EventFactory
	replay            *ReplayLog
	requests          chan any
	done              chan struct{}
	clock             common.Clock
	resource          agentprotocol.Resource
	contextDigest     string
	contextState      agentprotocol.ContextState
	lifecycle         agentprotocol.LifecycleState
	queue             *Queue
	commands          commandLedger
	active            *activeTurn
	workerSettled     chan struct{}
	shutdownAttempt   *actorShutdown
	deferredInterrupt *deferredInterrupt
	lifecycleCtx      context.Context
	stopping          bool
	dispatchBlocked   bool
	dispatchPending   bool
	shutdownTimeout   time.Duration

	closeMu sync.Mutex
	closed  atomic.Bool
}

type queuedAttachmentEvent struct {
	event agentprotocol.Event
	bytes int
}

type attachment struct {
	clientID string
	events   chan agentprotocol.Event
	detached chan struct{}
	stop     chan struct{}
	wake     chan struct{}
	pumpDone chan struct{}

	mu       sync.Mutex
	queue    []queuedAttachmentEvent
	bytes    int
	stopped  bool
	stopOnce sync.Once
}

func newAttachment(clientID string, initial []agentprotocol.Event) (*attachment, error) {
	item := &attachment{
		clientID: clientID, events: make(chan agentprotocol.Event), detached: make(chan struct{}),
		stop: make(chan struct{}), wake: make(chan struct{}, 1), pumpDone: make(chan struct{}),
	}
	for _, event := range initial {
		encoded, err := agentprotocol.EncodeEvent(event)
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

func (item *attachment) signal() {
	select {
	case item.wake <- struct{}{}:
	default:
	}
}

func (item *attachment) enqueue(event agentprotocol.Event) bool {
	encoded, err := agentprotocol.EncodeEvent(event)
	if err != nil {
		return false
	}
	item.mu.Lock()
	if item.stopped || len(item.queue)+1 > MaxReplayEvents || item.bytes+len(encoded) > MaxReplayBytes {
		item.mu.Unlock()
		return false
	}
	item.queue = append(item.queue, queuedAttachmentEvent{event: cloneEvent(event), bytes: len(encoded)})
	item.bytes += len(encoded)
	item.mu.Unlock()
	item.signal()
	return true
}

func (item *attachment) pump() {
	defer close(item.pumpDone)
	defer close(item.detached)
	defer close(item.events)
	for {
		item.mu.Lock()
		if item.stopped {
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

func (item *attachment) finish() {
	item.stopOnce.Do(func() {
		item.mu.Lock()
		item.stopped = true
		item.mu.Unlock()
		close(item.stop)
	})
	<-item.pumpDone
}

type attachRequest struct {
	ctx           context.Context
	clientID      string
	replayAfter   string
	resource      agentprotocol.Resource
	contextDigest string
	response      chan attachResponse
}
type attachResponse struct {
	attachment *attachment
	err        error
}
type detachRequest struct {
	attachment *attachment
	ack        chan struct{}
}
type commandRequest struct {
	ctx        context.Context
	attachment *attachment
	command    agentprotocol.Command
	response   chan commandResponse
}
type commandResponse struct {
	event agentprotocol.Event
	err   error
}
type closeConversationRequest struct {
	ctx      context.Context
	response chan error
}
type shutdownWorkerResult struct {
	response chan error
	err      error
}

func newConversation(identity agentstate.Identity, mapping agentstate.Mapping, session provider.Session, state StateStore, ids common.IDGenerator, clock common.Clock, lifecycleCtx context.Context, shutdownTimeout time.Duration) (*conversation, error) {
	if mapping.Validate(identity) != nil || mapping.Current == nil || common.IsNil(state) || common.IsNil(session) || common.IsNil(clock) || lifecycleCtx == nil || shutdownTimeout <= 0 {
		return nil, errors.New("invalid conversation actor")
	}
	factory, err := NewEventFactory(mapping.Current.ConversationID, ids, clock)
	if err != nil {
		return nil, err
	}
	actor := &conversation{
		identity: identity, mapping: cloneMapping(mapping), state: state, session: session,
		factory: factory, replay: NewReplayLog(), requests: make(chan any),
		done: make(chan struct{}), clock: clock, contextState: agentprotocol.ContextPending,
		lifecycle: agentprotocol.LifecycleReady, queue: NewQueue(), commands: newCommandLedger(),
		lifecycleCtx: lifecycleCtx, shutdownTimeout: shutdownTimeout,
	}
	if actor.mapping.Current.Observed != nil {
		actor.contextDigest = actor.mapping.Current.Observed.Digest
		actor.contextState = agentprotocol.ContextPending
	} else if actor.mapping.Current.Committed != nil {
		actor.contextDigest = actor.mapping.Current.Committed.Digest
		actor.contextState = agentprotocol.ContextUnchanged
	}
	go actor.run()
	return actor, nil
}

func (actor *conversation) run() {
	providerEvents := actor.session.Events()
	attachments := make(map[*attachment]struct{})
	shutdownResults := make(chan shutdownWorkerResult, 1)
	turnResults := make(chan turnWorkerResult, 1)
	historyResults := make(chan historyWorkerResult, 1)
	shutdownActive := false
	startShutdown := func(request *closeConversationRequest, attempt *actorShutdown) {
		go func() {
			err := attempt.run(request.ctx, actor.shutdownTimeout)
			shutdownResults <- shutdownWorkerResult{response: request.response, err: err}
		}()
	}
	defer func() {
		actor.queue.Clear()
		if actor.active != nil {
			zeroProviderContext(actor.active.request.Context)
			actor.active = nil
		}
		for item := range attachments {
			delete(attachments, item)
			item.finish()
		}
		close(actor.done)
	}()
	for {
		select {
		case raw := <-actor.requests:
			switch request := raw.(type) {
			case attachRequest:
				actor.handleAttach(attachments, request)
			case detachRequest:
				actor.detach(attachments, request.attachment)
				close(request.ack)
			case commandRequest:
				actor.handleCommand(attachments, turnResults, historyResults, request)
			case closeConversationRequest:
				if shutdownActive {
					request.response <- NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
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
				startShutdown(&request, actor.shutdownAttempt)
			}
		case result := <-turnResults:
			actor.handleTurnResult(attachments, turnResults, result)
		case result := <-historyResults:
			actor.handleHistoryResult(attachments, turnResults, result)
		case result := <-shutdownResults:
			shutdownActive = false
			if result.err != nil {
				result.response <- NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
				continue
			}
			actor.closed.Store(true)
			result.response <- nil
			return
		case event, open := <-providerEvents:
			if !open {
				providerEvents = nil
				continue
			}
			actor.handleProviderEvent(attachments, turnResults, event)
		}
	}
}

func (actor *conversation) handleAttach(attachments map[*attachment]struct{}, request attachRequest) {
	if err := request.ctx.Err(); err != nil {
		request.response <- attachResponse{err: err}
		return
	}
	var replayed []agentprotocol.Event
	if request.replayAfter != "" {
		var replayErr error
		replayed, replayErr = actor.replay.Replay(request.clientID, request.replayAfter)
		if replayErr != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorReplayWindowUnavailable)}
			return
		}
	}
	contextState, contextChanged, contextEvent, err := actor.observeAttach(request.resource, request.contextDigest)
	if err != nil {
		request.response <- attachResponse{err: err}
		return
	}

	initial := replayed
	if contextChanged {
		actor.queue.discardContext()
		if err := actor.replay.Append(contextEvent); err != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
			return
		}
		for attached := range attachments {
			actor.send(attachments, attached, contextEvent)
		}
		snapshot, snapshotErr := actor.snapshot(contextState)
		if snapshotErr != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
			return
		}
		if appendErr := actor.replay.AppendForClient(request.clientID, snapshot); appendErr != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
			return
		}
		if request.replayAfter == "" {
			initial = []agentprotocol.Event{snapshot}
		} else {
			initial, err = actor.replay.Replay(request.clientID, request.replayAfter)
			if err != nil {
				request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorReplayWindowUnavailable)}
				return
			}
		}
	} else if request.replayAfter == "" || len(initial) == 0 {
		snapshot, snapshotErr := actor.snapshot(contextState)
		if snapshotErr != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
			return
		}
		if appendErr := actor.replay.AppendForClient(request.clientID, snapshot); appendErr != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
			return
		}
		initial = append(initial, snapshot)
	}
	item, err := newAttachment(request.clientID, initial)
	if err != nil {
		request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
		return
	}
	attachments[item] = struct{}{}
	request.response <- attachResponse{attachment: item}
}

func (actor *conversation) snapshot(contextState agentprotocol.ContextState) (agentprotocol.Event, error) {
	var activeTurnID *string
	if actor.lifecycle == agentprotocol.LifecycleResponding && actor.active != nil {
		turnID := actor.active.request.TurnID
		activeTurnID = &turnID
	}
	return actor.factory.New(agentprotocol.SnapshotPayload{
		Lifecycle: actor.lifecycle, Queue: actor.queue.Items(),
		ContextState: contextState, ActiveTurnID: activeTurnID,
	})
}

func (actor *conversation) detach(attachments map[*attachment]struct{}, item *attachment) {
	if _, exists := attachments[item]; exists {
		delete(attachments, item)
	}
	item.finish()
}

func (actor *conversation) handleProviderEvent(attachments map[*attachment]struct{}, turnResults chan<- turnWorkerResult, source provider.Event) {
	if source.Validate() != nil || !actor.providerEventMatchesActive(source) {
		actor.publishBrowserError(attachments, agentprotocol.ErrorProviderMalformedStream)
		return
	}
	terminal := providerEventTerminal(source)
	if actor.active != nil && ((actor.active.phase == turnStarting && actor.providerEventTargetsActive(source)) || (actor.active.phase == turnInterrupting && terminal)) {
		actor.bufferProviderEvent(source)
		return
	}
	actor.publishProviderEvent(attachments, turnResults, source)
}

func (actor *conversation) publishProviderEvent(attachments map[*attachment]struct{}, turnResults chan<- turnWorkerResult, source provider.Event) {
	event, err := actor.factory.FromProvider(source)
	if err != nil {
		actor.publishBrowserError(attachments, agentprotocol.ErrorProviderMalformedStream)
		return
	}
	if actor.replay.Append(event) != nil {
		return
	}
	for item := range attachments {
		actor.send(attachments, item, event)
	}
	if source.Kind == provider.EventTerminalFailure && source.TurnID == "" {
		actor.handleSessionTerminalFailure(attachments)
		return
	}
	if providerEventTerminal(source) && actor.active != nil {
		lifecycle := agentprotocol.LifecycleReady
		if source.Kind == provider.EventInterruption || source.Kind == provider.EventTerminalFailure {
			lifecycle = agentprotocol.LifecycleInterrupted
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
	size := len(source.Text) + len(source.TurnID) + len(source.MessageID) + 64
	if len(actor.active.pendingEvents)+1 > MaxReplayEvents || actor.active.pendingBytes+size > MaxReplayBytes {
		actor.active.pendingOverflow = true
		actor.active.pendingEvents = nil
		actor.active.pendingBytes = 0
		return
	}
	actor.active.pendingEvents = append(actor.active.pendingEvents, source)
	actor.active.pendingBytes += size
}

func (actor *conversation) flushPendingProviderEvents(attachments map[*attachment]struct{}, turnResults chan<- turnWorkerResult) {
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
		actor.lifecycle = agentprotocol.LifecycleUnavailable
		actor.publishBrowserError(attachments, agentprotocol.ErrorProviderMalformedStream)
		actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
	}
	for _, source := range events {
		if actor.active == nil || !actor.providerEventMatchesActive(source) {
			actor.publishBrowserError(attachments, agentprotocol.ErrorProviderMalformedStream)
			continue
		}
		actor.publishProviderEvent(attachments, turnResults, source)
	}
}

func (actor *conversation) providerEventMatchesActive(source provider.Event) bool {
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

func (actor *conversation) handleSessionTerminalFailure(attachments map[*attachment]struct{}) {
	actor.dispatchBlocked = true
	if actor.active != nil {
		turnID := actor.active.request.TurnID
		actor.publishShared(attachments, agentprotocol.InterruptionPayload{TurnID: turnID, Reason: agentprotocol.InterruptionProviderExit})
		zeroProviderContext(actor.active.request.Context)
		actor.active = nil
	}
	actor.lifecycle = agentprotocol.LifecycleUnavailable
	actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
}

func (actor *conversation) send(attachments map[*attachment]struct{}, item *attachment, event agentprotocol.Event) {
	if !item.enqueue(event) {
		actor.detach(attachments, item)
	}
}

func (actor *conversation) attach(ctx context.Context, clientID, replayAfter string, resource agentprotocol.Resource, contextDigest string) (*Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := attachRequest{ctx: ctx, clientID: clientID, replayAfter: replayAfter, resource: cloneProtocolResource(resource), contextDigest: contextDigest, response: make(chan attachResponse, 1)}
	select {
	case actor.requests <- request:
	case <-actor.done:
		return nil, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
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
