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

const attachmentEventCapacity = agentprotocol.MaxReplayEvents + 1

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

type attachment struct {
	clientID string
	events   chan agentprotocol.Event
	detached chan struct{}
	once     sync.Once
}

func (item *attachment) finish() {
	item.once.Do(func() {
		close(item.events)
		close(item.detached)
	})
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

func newConversation(identity agentstate.Identity, mapping agentstate.Mapping, session provider.Session, ids common.IDGenerator, clock common.Clock, shutdownTimeout time.Duration) (*conversation, error) {
	if mapping.Current == nil || mapping.Identity != identity || common.IsNil(session) || shutdownTimeout <= 0 {
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
	defer func() {
		for item := range attachments {
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
				for item := range attachments {
					actor.detach(attachments, item)
				}
				err := actor.session.Shutdown(request.ctx)
				if err != nil {
					request.response <- NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
					continue
				}
				actor.closed.Store(true)
				request.response <- nil
				return
			}
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
	capacity := attachmentEventCapacity
	if len(initial)+1 > capacity {
		capacity = len(initial) + 1
	}
	item := &attachment{clientID: request.clientID, events: make(chan agentprotocol.Event, capacity), detached: make(chan struct{})}
	for _, event := range initial {
		item.events <- cloneEvent(event)
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
	if _, exists := attachments[item]; !exists {
		item.finish()
		return
	}
	delete(attachments, item)
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
	select {
	case item.events <- cloneEvent(event):
	default:
		actor.detach(attachments, item)
	}
}

func (actor *conversation) attach(ctx context.Context, clientID, replayAfter string) (*Connection, error) {
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
		if response.err != nil {
			return nil, response.err
		}
		return &Connection{actor: actor, attachment: response.attachment, conversationID: actor.mapping.Current.ConversationID, clientID: clientID}, nil
	case <-ctx.Done():
		// The actor request already owns the admission decision. Wait for its
		// immediate response so a cancellation racing atomic attach cannot leak
		// an attachment, then detach it synchronously.
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
	request := closeConversationRequest{ctx: ctx, response: make(chan error, 1)}
	select {
	case actor.requests <- request:
	case <-actor.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	// Session methods are synchronously context-cooperative by contract. Once
	// sent, wait for the actor's acknowledgement so a retry never overlaps a
	// still-running Shutdown call.
	return <-request.response
}
