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
	identity        agentstate.Identity
	mapping         agentstate.Mapping
	session         provider.Session
	factory         *EventFactory
	replay          *ReplayLog
	requests        chan any
	done            chan struct{}
	shutdownTimeout time.Duration

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
	ctx         context.Context
	clientID    string
	replayAfter string
	response    chan attachResponse
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

func newConversation(identity agentstate.Identity, mapping agentstate.Mapping, session provider.Session, ids common.IDGenerator, clock common.Clock, shutdownTimeout time.Duration) (*conversation, error) {
	if mapping.Validate(identity) != nil || mapping.Current == nil || common.IsNil(session) || shutdownTimeout <= 0 {
		return nil, errors.New("invalid conversation actor")
	}
	factory, err := NewEventFactory(mapping.Current.ConversationID, ids, clock)
	if err != nil {
		return nil, err
	}
	actor := &conversation{
		identity: identity, mapping: mapping, session: session,
		factory: factory, replay: NewReplayLog(), requests: make(chan any),
		done: make(chan struct{}), shutdownTimeout: shutdownTimeout,
	}
	go actor.run()
	return actor, nil
}

func (actor *conversation) run() {
	providerEvents := actor.session.Events()
	attachments := make(map[*attachment]struct{})
	shutdownResults := make(chan shutdownWorkerResult, 1)
	shutdownActive := false
	defer func() {
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
				actor.handleCommand(attachments, request)
			case closeConversationRequest:
				if shutdownActive {
					request.response <- NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
					continue
				}
				for item := range attachments {
					actor.detach(attachments, item)
				}
				shutdownActive = true
				go func() {
					err := actor.session.Shutdown(request.ctx)
					shutdownResults <- shutdownWorkerResult{response: request.response, err: err}
				}()
			}
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
			actor.handleProviderEvent(attachments, event)
		}
	}
}

func (actor *conversation) handleAttach(attachments map[*attachment]struct{}, request attachRequest) {
	if err := request.ctx.Err(); err != nil {
		request.response <- attachResponse{err: err}
		return
	}
	var initial []agentprotocol.Event
	if request.replayAfter != "" {
		replayed, err := actor.replay.Replay(request.clientID, request.replayAfter)
		if err != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorReplayWindowUnavailable)}
			return
		}
		initial = replayed
	}
	if request.replayAfter == "" || len(initial) == 0 {
		snapshot, err := actor.snapshot()
		if err != nil {
			request.response <- attachResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
			return
		}
		if err := actor.replay.AppendForClient(request.clientID, snapshot); err != nil {
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

func (actor *conversation) snapshot() (agentprotocol.Event, error) {
	return actor.factory.New(agentprotocol.SnapshotPayload{
		Lifecycle: agentprotocol.LifecycleReady, Queue: []agentprotocol.QueueItem{},
		ContextState: agentprotocol.ContextPending, ActiveTurnID: nil,
	})
}

func (actor *conversation) detach(attachments map[*attachment]struct{}, item *attachment) {
	if _, exists := attachments[item]; exists {
		delete(attachments, item)
	}
	item.finish()
}

func (actor *conversation) handleCommand(attachments map[*attachment]struct{}, request commandRequest) {
	if err := request.ctx.Err(); err != nil {
		request.response <- commandResponse{err: err}
		return
	}
	if _, exists := attachments[request.attachment]; !exists || request.command.ClientID != request.attachment.clientID || request.command.ConversationID == nil || actor.mapping.Current == nil || *request.command.ConversationID != actor.mapping.Current.ConversationID {
		request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorInvalidState)}
		return
	}
	browserError := agentprotocol.NewBrowserError(agentprotocol.ErrorInvalidState)
	result, err := actor.factory.New(agentprotocol.CommandResultPayload{CommandID: request.command.CommandID, Status: agentprotocol.CommandRejected, Error: &browserError})
	if err != nil {
		request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
		return
	}
	if err := actor.replay.AppendForClient(request.attachment.clientID, result); err != nil {
		request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
		return
	}
	actor.send(attachments, request.attachment, result)
	request.response <- commandResponse{event: cloneEvent(result)}
}

func (actor *conversation) handleProviderEvent(attachments map[*attachment]struct{}, source provider.Event) {
	event, err := actor.factory.FromProvider(source)
	if err != nil {
		event, err = actor.factory.New(agentprotocol.ErrorPayload{Error: agentprotocol.NewBrowserError(agentprotocol.ErrorProviderMalformedStream)})
		if err != nil {
			return
		}
	}
	if actor.replay.Append(event) != nil {
		return
	}
	for item := range attachments {
		actor.send(attachments, item, event)
	}
}

func (actor *conversation) send(attachments map[*attachment]struct{}, item *attachment, event agentprotocol.Event) {
	if !item.enqueue(event) {
		actor.detach(attachments, item)
	}
}

func (actor *conversation) attach(ctx context.Context, clientID, replayAfter string) (*Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := attachRequest{ctx: ctx, clientID: clientID, replayAfter: replayAfter, response: make(chan attachResponse, 1)}
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
	return <-request.response
}
