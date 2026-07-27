package broker

import (
	"errors"
	"sort"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type archiveRemovalStore interface {
	RemoveSession(agentstate.Identity, string, time.Time) (agentstate.CommitOutcome, error)
}

type archiveWorkerResult struct {
	generation uint64
	commandID  string
	clientID   string
	mapping    agentstate.Mapping
	archiveID  string
	code       agentprotocol.BrowserErrorCode
}

func archivePage(mapping agentstate.Mapping, commandID string, request agentprotocol.PageRequestPayload) (agentprotocol.HistoryPayload, agentprotocol.BrowserErrorCode) {
	archives := append([]agentstate.Session(nil), mapping.Archives...)
	sort.SliceStable(archives, func(left, right int) bool {
		return archives[left].UpdatedAt.After(archives[right].UpdatedAt)
	})
	start := 0
	if request.Before != "" {
		found := false
		for index := range archives {
			if archives[index].ConversationID == request.Before {
				start = index + 1
				found = true
				break
			}
		}
		if !found {
			return agentprotocol.HistoryPayload{}, agentprotocol.ErrorStaleReference
		}
	}
	limit := agentprotocol.NormalizePageSize(request.Limit)
	end := min(start+limit, len(archives))
	items := make([]agentprotocol.ArchiveItem, end-start)
	for index, archived := range archives[start:end] {
		items[index] = agentprotocol.ArchiveItem{
			ArchiveID: archived.ConversationID,
			CreatedAt: archived.CreatedAt,
			UpdatedAt: archived.UpdatedAt,
			Provider:  agentprotocol.ProviderPi,
			Model:     archived.ModelLabel,
			Preview:   "",
		}
	}
	var next *string
	if end < len(archives) && len(items) != 0 {
		cursor := items[len(items)-1].ArchiveID
		next = &cursor
	}
	return agentprotocol.HistoryPayload{CommandID: commandID, Items: items, NextCursor: next}, ""
}

func (actor *conversation) commandArchiveList(attachments map[*attachment]struct{}, command agentprotocol.Command, request agentprotocol.PageRequestPayload) {
	payload, code := archivePage(actor.mapping, command.CommandID, request)
	if code != "" {
		actor.completePendingCommand(attachments, command.CommandID, command.ClientID, code)
		return
	}
	event, err := actor.factory.New(payload)
	if err != nil || actor.replay.AppendForClient(command.ClientID, event) != nil {
		actor.failPendingCommand(command.CommandID)
		return
	}
	for item := range attachments {
		if item.clientID == command.ClientID {
			actor.send(attachments, item, event)
		}
	}
	actor.completePendingCommand(attachments, command.CommandID, command.ClientID, "")
}

func (actor *conversation) commandArchiveDelete(results chan<- archiveWorkerResult, command agentprotocol.Command, request agentprotocol.ArchiveReferencePayload) agentprotocol.BrowserErrorCode {
	if results == nil || actor.active != nil || !actor.queue.Empty() || actor.workerSettled != nil || actor.deferredInterrupt != nil || actor.recoveryActive || actor.dispatchBlocked || actor.stopping || actor.mapping.Current == nil || actor.mapping.Current.PreparedCommit != nil {
		return agentprotocol.ErrorInvalidState
	}
	var archived *agentstate.Session
	for index := range actor.mapping.Archives {
		if actor.mapping.Archives[index].ConversationID == request.ArchiveID {
			copyOfArchive := actor.mapping.Archives[index]
			archived = &copyOfArchive
			break
		}
	}
	if archived == nil {
		return agentprotocol.ErrorStaleReference
	}
	if archived.PreparedCommit != nil {
		return agentprotocol.ErrorStateRepairFailed
	}
	state, ok := actor.state.(archiveRemovalStore)
	if !ok {
		return agentprotocol.ErrorStateRepairFailed
	}
	before := cloneMapping(actor.mapping)
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return agentprotocol.ErrorStateRepairFailed
	}
	actor.beginProviderWorker(providerWorkerArchive, command.CommandID, command.ClientID)
	generation := actor.generation
	go func(target agentstate.Session) {
		result := archiveWorkerResult{generation: generation, commandID: command.CommandID, clientID: command.ClientID, archiveID: target.ConversationID}
		deleteRequest := provider.DeleteRequest{Provider: provider.NamePi, NativeSession: target.NativeSession}
		if deleteRequest.Validate() != nil || actor.driver.Delete(actor.lifecycleCtx, deleteRequest) != nil {
			result.code = agentprotocol.ErrorArchiveDeleteRetained
			results <- result
			return
		}
		if err := actor.state.RemoveWorkspace(target.ConversationID); err != nil {
			result.code = agentprotocol.ErrorStateRepairFailed
			results <- result
			return
		}
		mapping, err := actor.removeArchivedSession(state, before, target.ConversationID, at)
		if err != nil {
			result.code = agentprotocol.ErrorStateRepairFailed
		} else {
			result.mapping = mapping
		}
		results <- result
	}(*archived)
	return ""
}

func (actor *conversation) removeArchivedSession(state archiveRemovalStore, before agentstate.Mapping, archiveID string, at time.Time) (agentstate.Mapping, error) {
	target, ok := removedArchiveMapping(before, archiveID, at)
	if !ok {
		return agentstate.Mapping{}, errors.New("archive is absent")
	}
	outcome, mutationErr := state.RemoveSession(actor.identity, archiveID, at)
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class := classifyLoadedState(actor.state, actor.identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	if class != mappingPrecondition || !knownCommitOutcome(outcome) {
		return agentstate.Mapping{}, errors.New("archive removal outcome is ambiguous")
	}
	at = actor.clock.Now().UTC()
	if at.IsZero() {
		return agentstate.Mapping{}, errors.New("invalid archive removal time")
	}
	target, ok = removedArchiveMapping(loaded, archiveID, at)
	if !ok {
		return agentstate.Mapping{}, errors.New("archive disappeared before retry")
	}
	outcome, mutationErr = state.RemoveSession(actor.identity, archiveID, at)
	if outcome == agentstate.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class = classifyLoadedState(actor.state, actor.identity, loaded, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	return agentstate.Mapping{}, errors.New("archive removal failed")
}

func removedArchiveMapping(before agentstate.Mapping, archiveID string, at time.Time) (agentstate.Mapping, bool) {
	result := cloneMapping(before)
	index := -1
	for candidate := range result.Archives {
		if result.Archives[candidate].ConversationID == archiveID {
			index = candidate
			break
		}
	}
	if index < 0 {
		return agentstate.Mapping{}, false
	}
	result.Archives = append(result.Archives[:index], result.Archives[index+1:]...)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result, true
}

func (actor *conversation) handleArchiveResult(attachments map[*attachment]struct{}, recoveryResults chan<- recoveryWorkerResult, result archiveWorkerResult) {
	settled, resolved := actor.settleProviderWorker()
	if settled != nil {
		defer close(settled)
	}
	if result.generation != actor.generation || resolved {
		return
	}
	if result.code == "" {
		actor.mapping = result.mapping
	}
	observationErr := actor.applyDeferredObservation(attachments)
	if observationErr != nil {
		actor.contextState = agentprotocol.ContextUnavailable
		actor.dispatchBlocked = true
		actor.recoveryUnavailable = true
		actor.lifecycle = agentprotocol.LifecycleUnavailable
		actor.publishBrowserError(attachments, agentprotocol.ErrorStateRepairFailed)
		actor.publishShared(attachments, agentprotocol.LifecyclePayload{State: actor.lifecycle})
	}
	if result.code != "" {
		actor.completePendingCommand(attachments, result.commandID, result.clientID, result.code)
	} else {
		actor.publishShared(attachments, agentprotocol.ArchivePayload{Action: agentprotocol.ArchiveDeleted, ArchiveID: result.archiveID})
		if observationErr != nil {
			actor.completePendingCommand(attachments, result.commandID, result.clientID, agentprotocol.ErrorStateRepairFailed)
		} else {
			actor.completePendingCommand(attachments, result.commandID, result.clientID, "")
		}
	}
	if actor.recoveryPending {
		trigger := actor.recoveryPendingTrigger
		actor.recoveryPending = false
		if observationErr != nil {
			actor.recoveryAttempted = actor.generation
			if trigger == recoveryTriggerClosure {
				actor.publishBrowserError(attachments, agentprotocol.ErrorProviderCrashed)
			}
			return
		}
		actor.startRecovery(attachments, recoveryResults, trigger)
	}
}
