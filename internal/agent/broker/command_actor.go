package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
)

func (actor *conversation) handleCommand(attachments map[*clientAttachment]struct{}, turnResults chan<- turnWorkerResult, historyResults chan<- historyWorkerResult, archiveResults chan<- archiveWorkerResult, handoffResults chan<- handoffResult, interactionResults chan<- interactionWorkerResult, request commandRequest) {
	if err := request.ctx.Err(); err != nil {
		request.response <- commandResponse{err: err}
		return
	}
	if _, exists := attachments[request.attachment]; !exists || request.command.ClientID != request.attachment.clientID || request.command.ConversationID == nil || actor.mapping.Current == nil || *request.command.ConversationID != actor.mapping.Current.ConversationID {
		request.response <- commandResponse{err: NewBrokerError(protocol.ErrorInvalidState)}
		return
	}

	disposition, completed, err := actor.commands.begin(request.command)
	if err != nil {
		request.response <- commandResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
		return
	}
	switch disposition {
	case commandConflict:
		actor.replyStandalone(attachments, request, protocol.ErrorInvalidCommand)
		return
	case commandCompleted:
		actor.send(attachments, request.attachment, completed)
		request.response <- commandResponse{event: completed}
		return
	case commandPending:
		if err := actor.commands.wait(request.command.CommandID, commandWaiter{attachment: request.attachment, response: request.response}); err != nil {
			request.response <- commandResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
		}
		return
	case commandNew:
		if err := actor.commands.wait(request.command.CommandID, commandWaiter{attachment: request.attachment, response: request.response}); err != nil {
			request.response <- commandResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
			return
		}
	}

	if actor.handoffActive || actor.handoffFailed {
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
		return
	}
	if request.command.Type == protocol.CommandNew {
		if _, ok := request.command.Payload.(protocol.NewPayload); !ok {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		if code := actor.commandHandoff(handoffResults, request.command, ""); code != "" {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
		}
		return
	}

	switch payload := request.command.Payload.(type) {
	case protocol.SubmitPayload:
		pending, code := actor.commandSubmit(attachments, turnResults, request.command, payload)
		if pending {
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
	case protocol.CompactPayload:
		pending, code := actor.commandCompact(attachments, turnResults, request.command, payload)
		if pending {
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
	case protocol.QueueEditPayload:
		if actor.identity.Kind == statepkg.ResourceHTML && messageContentHasReferences(payload.Content) {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		content, err := messageContentToProvider(payload.Content)
		if err != nil || !referencesMatchCurrentPage(content, actor.resource, actor.contextDigest) {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		removedImages, err := actor.queue.EditAndRemovedImages(payload.MessageID, content)
		if err != nil {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		if len(removedImages) != 0 && actor.releaseMessageImageSubset(payload.MessageID, removedImages) != nil {
			actor.publishShared(attachments, protocol.QueuePayload{Items: actor.queue.Items()})
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorImageStorageFailure)
			return
		}
		if !actor.publishShared(attachments, protocol.QueuePayload{Items: actor.queue.Items()}) {
			actor.failPendingCommand(request.command.CommandID)
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, "")
	case protocol.MessageReferencePayload:
		if request.command.Type != protocol.CommandQueueRemove || actor.queue.Remove(payload.MessageID) != nil {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		if actor.releaseMessageImages(payload.MessageID) != nil {
			actor.publishShared(attachments, protocol.QueuePayload{Items: actor.queue.Items()})
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorImageStorageFailure)
			return
		}
		if !actor.publishShared(attachments, protocol.QueuePayload{Items: actor.queue.Items()}) {
			actor.failPendingCommand(request.command.CommandID)
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, "")
	case protocol.WorkReferencePayload:
		if request.command.Type != protocol.CommandInterrupt {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		pending, code := actor.commandInterrupt(attachments, turnResults, request.command, payload)
		if pending {
			return
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
	case protocol.PageRequestPayload:
		if request.command.Type == protocol.CommandArchiveList {
			actor.commandArchiveList(attachments, request.command, payload)
			return
		}
		if request.command.Type != protocol.CommandHistoryPage || actor.compact != nil || actor.workerSettled != nil || actor.dispatchBlocked || actor.stopping {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		actor.startHistoryWorker(historyResults, request.command.CommandID, request.command.ClientID, payload)
	case protocol.ArchiveReferencePayload:
		if request.command.Type == protocol.CommandArchiveRestore {
			if code := actor.commandHandoff(handoffResults, request.command, payload.ArchiveID); code != "" {
				actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
			}
			return
		}
		if request.command.Type != protocol.CommandArchiveDelete {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		if code := actor.commandArchiveDelete(archiveResults, request.command, payload); code != "" {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
		}
	case protocol.ResyncPayload:
		replayed, err := actor.replay.Replay(request.command.ClientID, payload.AfterEventID)
		if err != nil {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorReplayWindowUnavailable)
			return
		}
		if len(replayed) == 0 {
			snapshot, snapshotErr := actor.snapshot(actor.contextState)
			if snapshotErr != nil || actor.replay.AppendForClient(request.command.ClientID, snapshot) != nil {
				actor.failPendingCommand(request.command.CommandID)
				return
			}
			replayed = []protocol.Event{snapshot}
		}
		for _, event := range replayed {
			actor.send(attachments, request.attachment, event)
		}
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, "")
	case protocol.InteractionResponsePayload:
		if request.command.Type != protocol.CommandInteractionRespond {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
			return
		}
		if code := actor.commandInteractionRespond(attachments, interactionResults, request.command, payload); code != "" {
			actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, code)
		}
	default:
		actor.completePendingCommand(attachments, request.command.CommandID, request.command.ClientID, protocol.ErrorInvalidState)
	}
}

func (actor *conversation) completePendingCommand(attachments map[*clientAttachment]struct{}, commandID, clientID string, code protocol.BrowserErrorCode) {
	status := protocol.CommandSucceeded
	var browserError *protocol.BrowserError
	if code != "" {
		status = protocol.CommandRejected
		value := protocol.NewBrowserError(code)
		browserError = &value
	}
	result, err := actor.factory.New(protocol.CommandResultPayload{CommandID: commandID, Status: status, Error: browserError})
	if err != nil || actor.replay.AppendForClient(clientID, result) != nil {
		actor.failPendingCommand(commandID)
		return
	}
	waiters, err := actor.commands.complete(commandID, result)
	if err != nil {
		actor.failWaiters(actor.commands.abandon(commandID), NewBrokerError(protocol.ErrorBrokerUnavailable))
		return
	}
	seen := make(map[*clientAttachment]struct{}, len(waiters))
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

func (actor *conversation) replyStandalone(attachments map[*clientAttachment]struct{}, request commandRequest, code protocol.BrowserErrorCode) {
	browserError := protocol.NewBrowserError(code)
	result, err := actor.factory.New(protocol.CommandResultPayload{CommandID: request.command.CommandID, Status: protocol.CommandRejected, Error: &browserError})
	if err != nil || actor.replay.AppendForClient(request.command.ClientID, result) != nil {
		request.response <- commandResponse{err: NewBrokerError(protocol.ErrorBrokerUnavailable)}
		return
	}
	actor.send(attachments, request.attachment, result)
	request.response <- commandResponse{event: result}
}

func (actor *conversation) failPendingCommand(commandID string) {
	actor.failWaiters(actor.commands.abandon(commandID), NewBrokerError(protocol.ErrorBrokerUnavailable))
}

func (actor *conversation) failWaiters(waiters []commandWaiter, err error) {
	if err == nil {
		err = errors.New("command failed")
	}
	for _, waiter := range waiters {
		waiter.response <- commandResponse{err: err}
	}
}

func (actor *conversation) publishShared(attachments map[*clientAttachment]struct{}, payload protocol.EventPayload) bool {
	event, err := actor.factory.New(payload)
	if err != nil || actor.replay.Append(event) != nil {
		return false
	}
	for item := range attachments {
		actor.send(attachments, item, event)
	}
	return true
}

func (actor *conversation) prepareShared(payload protocol.EventPayload) (protocol.Event, preparedReplayEntry, error) {
	event, err := actor.factory.New(payload)
	if err != nil {
		return protocol.Event{}, preparedReplayEntry{}, err
	}
	prepared, err := actor.replay.prepareAppend(event, "")
	if err != nil {
		return protocol.Event{}, preparedReplayEntry{}, err
	}
	return event, prepared, nil
}

func (actor *conversation) publishPreparedShared(attachments map[*clientAttachment]struct{}, event protocol.Event, prepared preparedReplayEntry) bool {
	if actor.replay.appendPrepared(prepared) != nil {
		return false
	}
	for item := range attachments {
		actor.send(attachments, item, event)
	}
	return true
}

func (actor *conversation) publishBrowserError(attachments map[*clientAttachment]struct{}, code protocol.BrowserErrorCode) bool {
	return actor.publishShared(attachments, protocol.ErrorPayload{Error: protocol.NewBrowserError(code)})
}
