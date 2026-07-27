package broker

import (
	"context"
	"sync"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/localapi"
)

// Connection is one actor-owned browser attachment.
type Connection struct {
	actor          *conversation
	attachment     *attachment
	conversationID string
	clientID       string

	closeMu sync.Mutex
	detach  *detachRequest
	closed  bool
}

func (connection *Connection) ConversationID() string {
	if connection == nil {
		return ""
	}
	return connection.conversationID
}

func (connection *Connection) Events() <-chan agentprotocol.Event {
	if connection == nil || connection.attachment == nil {
		return nil
	}
	return connection.attachment.events
}

func (connection *Connection) Command(ctx context.Context, command agentprotocol.Command) (agentprotocol.Event, error) {
	if connection == nil || connection.actor == nil || connection.attachment == nil {
		return agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorInvalidState)
	}
	if _, err := agentprotocol.EncodeCommand(command); err != nil || command.Type == agentprotocol.CommandConnect || command.ClientID != connection.clientID || command.ConversationID == nil || *command.ConversationID != connection.conversationID {
		return agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorInvalidState)
	}
	request := commandRequest{ctx: ctx, attachment: connection.attachment, command: command, response: make(chan commandResponse, 1)}
	select {
	case connection.actor.requests <- request:
	case <-connection.attachment.detached:
		return agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorInvalidState)
	case <-connection.actor.done:
		return agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorBrokerShuttingDown)
	case <-ctx.Done():
		return agentprotocol.Event{}, ctx.Err()
	}
	// Once admitted, the actor performs no blocking provider work for commands.
	// Await its single response so publication and the synchronous result cannot
	// diverge merely because cancellation raced the acknowledgement.
	response := <-request.response
	return response.event, response.err
}

// Close sends at most one detach request. Completion is recorded only after
// the actor acknowledges it, so a deadline while waiting remains retryable.
func (connection *Connection) Close(ctx context.Context) error {
	if connection == nil {
		return nil
	}
	connection.closeMu.Lock()
	defer connection.closeMu.Unlock()
	if connection.closed {
		return nil
	}
	if connection.attachment == nil || connection.actor == nil {
		connection.closed = true
		return nil
	}
	if connection.detach == nil {
		request := &detachRequest{attachment: connection.attachment, ack: make(chan struct{})}
		select {
		case connection.actor.requests <- *request:
			connection.detach = request
		case <-connection.attachment.detached:
			connection.closed = true
			return nil
		case <-connection.actor.done:
			connection.closed = true
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-connection.detach.ack:
		connection.closed = true
		return nil
	case <-ctx.Done():
		// The request remains the sole detach attempt. A retry waits on the
		// same acknowledgement rather than assuming channel closure completed it.
		return ctx.Err()
	}
}

var _ localapi.Connection = (*Connection)(nil)
