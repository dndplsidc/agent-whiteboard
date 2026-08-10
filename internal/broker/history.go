package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type historyWorkerResult struct {
	generation uint64
	commandID  string
	clientID   string
	page       provider.HistoryPage
	limit      int
	err        error
}

func (actor *conversation) startHistoryWorker(results chan<- historyWorkerResult, commandID, clientID string, payload agentprotocol.PageRequestPayload) {
	limit := agentprotocol.NormalizePageSize(payload.Limit)
	actor.beginProviderWorker(providerWorkerHistory, commandID, clientID)
	generation := actor.generation
	go func() {
		page, err := actor.session.session.History(actor.lifecycleCtx, provider.HistoryRequest{BeforeMessageID: payload.Before, Limit: limit})
		if err == nil {
			err = page.Validate()
		}
		results <- historyWorkerResult{generation: generation, commandID: commandID, clientID: clientID, page: page, limit: limit, err: err}
	}()
}

func (actor *conversation) handleHistoryResult(attachments map[*attachment]struct{}, turnResults chan<- turnWorkerResult, result historyWorkerResult) {
	settled, resolved := actor.settleProviderWorker()
	if settled != nil {
		defer close(settled)
	}
	if result.generation != actor.generation || resolved {
		return
	}
	if result.err != nil {
		actor.completePendingCommand(attachments, result.commandID, result.clientID, MapError(result.err).Code())
		actor.resumePendingDispatch(attachments, turnResults)
		return
	}
	descriptors := make(map[string][]agentprotocol.ImageDescriptor)
	for _, item := range result.page.Items {
		if item.Role != provider.HistoryUser {
			continue
		}
		images, imageErr := actor.messageImages(item.MessageID)
		if imageErr != nil {
			actor.completePendingCommand(attachments, result.commandID, result.clientID, agentprotocol.ErrorImageStorageFailure)
			actor.resumePendingDispatch(attachments, turnResults)
			return
		}
		descriptors[item.MessageID] = images
	}
	payload, err := historyPayload(result.commandID, result.limit, result.page, descriptors)
	if err != nil {
		actor.completePendingCommand(attachments, result.commandID, result.clientID, agentprotocol.ErrorProviderProtocolFailure)
		actor.resumePendingDispatch(attachments, turnResults)
		return
	}
	event, err := actor.factory.New(payload)
	if err != nil || actor.replay.AppendForClient(result.clientID, event) != nil {
		actor.failPendingCommand(result.commandID)
		actor.resumePendingDispatch(attachments, turnResults)
		return
	}
	for item := range attachments {
		if item.clientID == result.clientID {
			actor.send(attachments, item, event)
		}
	}
	actor.completePendingCommand(attachments, result.commandID, result.clientID, "")
	actor.resumePendingDispatch(attachments, turnResults)
}

func (actor *conversation) resumePendingDispatch(attachments map[*attachment]struct{}, results chan<- turnWorkerResult) {
	if actor.deferredInterrupt != nil {
		deferred := actor.deferredInterrupt
		actor.deferredInterrupt = nil
		if actor.stopping || actor.active == nil || actor.active.phase != turnRunning || actor.active.accepted == nil || actor.active.request.TurnID != deferred.turnID {
			actor.completePendingCommand(attachments, deferred.commandID, deferred.clientID, agentprotocol.ErrorInvalidState)
		} else {
			actor.startInterruptWorker(results, deferred.commandID, deferred.clientID)
			return
		}
	}
	if !actor.dispatchPending {
		return
	}
	actor.dispatchPending = false
	if actor.stopping || actor.dispatchBlocked || actor.active != nil || actor.queue.Empty() {
		return
	}
	actor.dispatchNext(attachments, results)
}

func historyPayload(commandID string, limit int, page provider.HistoryPage, descriptorSets ...map[string][]agentprotocol.ImageDescriptor) (agentprotocol.TimelinePayload, error) {
	if err := page.Validate(); err != nil || limit <= 0 || limit > agentprotocol.MaxPageSize || len(page.Items) > limit {
		return agentprotocol.TimelinePayload{}, errors.New("invalid provider history page")
	}
	items := make([]agentprotocol.TimelineItem, len(page.Items))
	seen := make(map[string]struct{}, len(page.Items))
	total := 0
	for index, item := range page.Items {
		if item.Role == provider.HistoryUser {
			total += item.Content.SemanticBytes()
		} else {
			total += len(item.Text)
		}
		if total > agentprotocol.MaxTimelineBytes {
			return agentprotocol.TimelinePayload{}, errors.New("provider history exceeds browser timeline limit")
		}
		if _, duplicate := seen[item.MessageID]; duplicate {
			return agentprotocol.TimelinePayload{}, errors.New("duplicate provider history message")
		}
		seen[item.MessageID] = struct{}{}
		var kind agentprotocol.TimelineItemKind
		var content *agentprotocol.MessageContent
		var images []agentprotocol.ImageDescriptor
		switch item.Role {
		case provider.HistoryUser:
			kind = agentprotocol.TimelineUser
			if len(descriptorSets) != 0 {
				images = descriptorSets[0][item.MessageID]
			}
			converted, err := messageContentFromProvider(item.Content, images)
			if err != nil {
				return agentprotocol.TimelinePayload{}, errors.New("invalid provider history content")
			}
			content = &converted
			images = ordinaryImageDescriptors(item.Content, images)
		case provider.HistoryAssistant:
			kind = agentprotocol.TimelineAssistant
		default:
			return agentprotocol.TimelinePayload{}, errors.New("invalid provider history role")
		}
		items[index] = agentprotocol.TimelineItem{
			ItemID: item.MessageID, Kind: kind, TurnID: item.TurnID,
			MessageID: item.MessageID, Text: item.Text, Content: content, Images: images, CreatedAt: item.CreatedAt,
		}
	}
	var next *string
	if page.NextCursor != "" {
		cursor := page.NextCursor
		next = &cursor
	}
	return agentprotocol.TimelinePayload{CommandID: commandID, Items: items, NextCursor: next}, nil
}
