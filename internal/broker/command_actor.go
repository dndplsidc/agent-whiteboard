package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
)

func (actor *conversation) handleCommand(attachments map[*attachment]struct{}, turnResults chan<- turnWorkerResult, historyResults chan<- historyWorkerResult, archiveResults chan<- archiveWorkerResult, handoffResults chan<- handoffResult, interactionResults chan<- interactionWorkerResult, request commandRequest) {
	if err := request.ctx.Err(); err != nil {
		request.response <- commandResponse{err: err}
		return
	}
	if _, exists := attachments[request.attachment]; !exists || request.command.ClientID != request.attachment.clientID || request.command.ConversationID == nil || actor.mapping.Current == nil || *request.command.ConversationID != actor.mapping.Current.ConversationID {
		request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorInvalidState)}
		return
	}

	disposition, completed, err := actor.commands.begin(request.command)
	if err != nil {
		request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
		return
	}
	switch disposition {
	case commandConflict:
		actor.replyStandalone(attachments, request, agentprotocol.ErrorInvalidCommand)
		return
	case commandCompleted:
		actor.send(attachments, request.attachment, completed)
		request.response <- commandResponse{event: completed}
		return
	case commandPending:
		if err := actor.commands.wait(request.command.CommandID, commandWaiter{attachment: request.attachment, response: request.response}); err != nil {
			request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
		}
		return
	case commandNew:
		if err := actor.commands.wait(request.command.CommandID, commandWaiter{attachment: request.attachment, response: request.response}); err != nil {
			request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
			return
		}
	}

	if actor.handoffActive || actor.handoffFailed {
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
		return
	}
	if request.command.Type == agentprotocol.CommandNew {
		if _, ok := request.command.Payload.(agentprotocol.EmptyPayload); !ok {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
			return
		}
		if code := actor.commandHandoff(handoffResults, request.command, ""); code != "" {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
		}
		return
	}

	switch payload := request.command.Payload.(type) {
	case agentprotocol.SubmitPayload:
		pending, code := actor.commandSubmit(attachments, turnResults, request.command, payload)
		if pending {
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
	case agentprotocol.QueueEditPayload:
		content, err := messageContentToProvider(payload.Content)
		if err != nil || !referencesMatchCurrentPage(content, actor.resource, actor.contextDigest) || actor.queue.Edit(payload.MessageID, content) != nil {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
			return
		}
		if !actor.publishShared(attachments, agentprotocol.QueuePayload{Items: actor.queue.Items()}) {
			actor.failPendingCommand(request.command.CommandID)
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, "")
	case agentprotocol.MessageReferencePayload:
		if request.command.Type != agentprotocol.CommandQueueRemove || actor.queue.Remove(payload.MessageID) != nil {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
			return
		}
		if actor.releaseMessageImages(payload.MessageID) != nil {
			actor.publishShared(attachments, agentprotocol.QueuePayload{Items: actor.queue.Items()})
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorImageStorageFailure)
			return
		}
		if !actor.publishShared(attachments, agentprotocol.QueuePayload{Items: actor.queue.Items()}) {
			actor.failPendingCommand(request.command.CommandID)
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, "")
	case agentprotocol.TurnReferencePayload:
		if request.command.Type != agentprotocol.CommandInterrupt {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
			return
		}
		pending, code := actor.commandInterrupt(turnResults, request.command, payload)
		if pending {
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
	case agentprotocol.PageRequestPayload:
		if request.command.Type == agentprotocol.CommandArchiveList {
			actor.commandArchiveList(attachments, request.command, payload)
			return
		}
		if request.command.Type != agentprotocol.CommandHistoryPage || actor.workerSettled != nil || actor.dispatchBlocked || actor.stopping {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
			return
		}
		actor.startHistoryWorker(historyResults, request.command.CommandID, request.command.ClientID, payload)
	case agentprotocol.ArchiveReferencePayload:
		if request.command.Type == agentprotocol.CommandArchiveRestore {
			if code := actor.commandHandoff(handoffResults, request.command, payload.ArchiveID); code != "" {
				actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
			}
			return
		}
		if request.command.Type != agentprotocol.CommandArchiveDelete {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
			return
		}
		if code := actor.commandArchiveDelete(archiveResults, request.command, payload); code != "" {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
		}
	case agentprotocol.ResyncPayload:
		replayed, err := actor.replay.Replay(request.command.ClientID, payload.AfterEventID)
		if err != nil {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorReplayWindowUnavailable)
			return
		}
		if len(replayed) == 0 {
			snapshot, snapshotErr := actor.snapshot(actor.contextState)
			if snapshotErr != nil || actor.replay.AppendForClient(request.command.ClientID, snapshot) != nil {
				actor.failPendingCommand(request.command.CommandID)
				return
			}
			replayed = []agentprotocol.Event{snapshot}
		}
		for _, event := range replayed {
			actor.send(attachments, request.attachment, event)
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, "")
	case agentprotocol.InteractionResponsePayload:
		if request.command.Type != agentprotocol.CommandInteractionRespond {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
			return
		}
		if code := actor.commandInteractionRespond(interactionResults, request.command, payload); code != "" {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
		}
	default:
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, agentprotocol.ErrorInvalidState)
	}
}

func (actor *conversation) completePendingCommand(attachments map[*attachment]struct{}, commandID, clientID string, code agentprotocol.BrowserErrorCode) {
	status := agentprotocol.CommandSucceeded
	var browserError *agentprotocol.BrowserError
	if code != "" {
		status = agentprotocol.CommandRejected
		value := agentprotocol.NewBrowserError(code)
		browserError = &value
	}
	result, err := actor.factory.New(agentprotocol.CommandResultPayload{CommandID: commandID, Status: status, Error: browserError})
	if err != nil || actor.replay.AppendForClient(clientID, result) != nil {
		actor.failPendingCommand(commandID)
		return
	}
	waiters, err := actor.commands.complete(commandID, result)
	if err != nil {
		actor.failWaiters(actor.commands.abandon(commandID), NewBrokerError(agentprotocol.ErrorBrokerUnavailable))
		return
	}
	seen := make(map[*attachment]struct{}, len(waiters))
	for _, waiter := range waiters {
		if waiter.attachment != nil {
			if _, duplicate := seen[waiter.attachment]; !duplicate {
				seen[waiter.attachment] = struct{}{}
				if _, attached := attachments[waiter.attachment]; attached {
					actor.send(attachments, waiter.attachment, result)
				}
			}
		}
		waiter.response <- commandResponse{event: cloneEvent(result)}
	}
}

func (actor *conversation) replyStandalone(attachments map[*attachment]struct{}, request commandRequest, code agentprotocol.BrowserErrorCode) {
	browserError := agentprotocol.NewBrowserError(code)
	result, err := actor.factory.New(agentprotocol.CommandResultPayload{CommandID: request.command.CommandID, Status: agentprotocol.CommandRejected, Error: &browserError})
	if err != nil || actor.replay.AppendForClient(request.command.ClientID, result) != nil {
		request.response <- commandResponse{err: NewBrokerError(agentprotocol.ErrorBrokerUnavailable)}
		return
	}
	actor.send(attachments, request.attachment, result)
	request.response <- commandResponse{event: result}
}

func (actor *conversation) failPendingCommand(commandID string) {
	actor.failWaiters(actor.commands.abandon(commandID), NewBrokerError(agentprotocol.ErrorBrokerUnavailable))
}

func (actor *conversation) failWaiters(waiters []commandWaiter, err error) {
	if err == nil {
		err = errors.New("command failed")
	}
	for _, waiter := range waiters {
		waiter.response <- commandResponse{err: err}
	}
}

func (actor *conversation) publishShared(attachments map[*attachment]struct{}, payload agentprotocol.EventPayload) bool {
	event, err := actor.factory.New(payload)
	if err != nil || actor.replay.Append(event) != nil {
		return false
	}
	for item := range attachments {
		actor.send(attachments, item, event)
	}
	return true
}

func (actor *conversation) publishBrowserError(attachments map[*attachment]struct{}, code agentprotocol.BrowserErrorCode) bool {
	return actor.publishShared(attachments, agentprotocol.ErrorPayload{Error: agentprotocol.NewBrowserError(code)})
}
