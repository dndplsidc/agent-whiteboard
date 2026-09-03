package broker

import (
	"errors"
	"sort"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

type archiveRemovalStore interface {
	RemoveSession(statepkg.Identity, string, time.Time) (statepkg.CommitOutcome, error)
}

type archiveWorkerResult struct {
	generation uint64
	commandID  string
	clientID   string
	mapping    statepkg.Mapping
	archiveID  string
	code       protocol.BrowserErrorCode
}

func archivePage(mapping statepkg.Mapping, commandID string, request protocol.PageRequestPayload) (protocol.HistoryPayload, protocol.BrowserErrorCode) {
	wireProvider, err := providerNameFromDomain(mapping.Identity.Provider)
	if err != nil {
		return protocol.HistoryPayload{}, protocol.ErrorStateRepairFailed
	}
	archives := append([]statepkg.Session(nil), mapping.Archives...)
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
			return protocol.HistoryPayload{}, protocol.ErrorStaleReference
		}
	}
	limit := protocol.NormalizePageSize(request.Limit)
	end := min(start+limit, len(archives))
	items := make([]protocol.ArchiveItem, end-start)
	for index, archived := range archives[start:end] {
		items[index] = protocol.ArchiveItem{
			ArchiveID: archived.ConversationID,
			CreatedAt: archived.CreatedAt,
			UpdatedAt: archived.UpdatedAt,
			Provider:  wireProvider,
			Model:     archived.ModelLabel,
			Preview:   "",
		}
	}
	var next *string
	if end < len(archives) && len(items) != 0 {
		cursor := items[len(items)-1].ArchiveID
		next = &cursor
	}
	return protocol.HistoryPayload{CommandID: commandID, Items: items, NextCursor: next}, ""
}

func (actor *conversation) commandArchiveList(attachments map[*clientAttachment]struct{}, command protocol.Command, request protocol.PageRequestPayload) {
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

func (actor *conversation) commandArchiveDelete(results chan<- archiveWorkerResult, command protocol.Command, request protocol.ArchiveReferencePayload) protocol.BrowserErrorCode {
	if results == nil || actor.active != nil || actor.compact != nil || !actor.queue.Empty() || actor.workerSettled != nil || actor.deferredInterrupt != nil || actor.recoveryActive || actor.dispatchBlocked || actor.stopping || actor.mapping.Current == nil || actor.mapping.Current.PreparedCommit != nil {
		return protocol.ErrorInvalidState
	}
	deleter, supported := actor.driver.(provider.NativeSessionDeleter)
	if !supported || common.IsNil(deleter) {
		return protocol.ErrorArchiveDeleteUnsupported
	}
	var archived *statepkg.Session
	for index := range actor.mapping.Archives {
		if actor.mapping.Archives[index].ConversationID == request.ArchiveID {
			copyOfArchive := actor.mapping.Archives[index]
			archived = &copyOfArchive
			break
		}
	}
	if archived == nil {
		return protocol.ErrorStaleReference
	}
	if archived.PreparedCommit != nil {
		return protocol.ErrorStateRepairFailed
	}
	archiveStore, ok := actor.state.(archiveRemovalStore)
	if !ok {
		return protocol.ErrorStateRepairFailed
	}
	before := cloneMapping(actor.mapping)
	at := actor.clock.Now().UTC()
	if at.IsZero() {
		return protocol.ErrorStateRepairFailed
	}
	actor.beginProviderWorker(providerWorkerArchive, command.CommandID, command.ClientID)
	generation := actor.generation
	go func(target statepkg.Session) {
		result := archiveWorkerResult{generation: generation, commandID: command.CommandID, clientID: command.ClientID, archiveID: target.ConversationID}
		deleteRequest := provider.DeleteRequest{Provider: actor.identity.Provider, NativeSession: target.NativeSession}
		if deleteRequest.Validate() != nil || deleter.Delete(actor.lifecycleCtx, deleteRequest) != nil {
			result.code = protocol.ErrorArchiveDeleteRetained
			results <- result
			return
		}
		if err := removeImageWorkspace(actor.lifecycleCtx, actor.attachments, actor.state, target.ConversationID); err != nil {
			result.code = protocol.ErrorStateRepairFailed
			results <- result
			return
		}
		mapping, err := actor.removeArchivedSession(archiveStore, before, target.ConversationID, at)
		if err != nil {
			result.code = protocol.ErrorStateRepairFailed
		} else {
			result.mapping = mapping
		}
		results <- result
	}(*archived)
	return ""
}

func (actor *conversation) removeArchivedSession(store archiveRemovalStore, before statepkg.Mapping, archiveID string, at time.Time) (statepkg.Mapping, error) {
	target, ok := removedArchiveMapping(before, archiveID, at)
	if !ok {
		return statepkg.Mapping{}, errors.New("archive is absent")
	}
	outcome, mutationErr := store.RemoveSession(actor.identity, archiveID, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class := classifyLoadedState(actor.state, actor.identity, before, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	if class != mappingPrecondition || !knownCommitOutcome(outcome) {
		return statepkg.Mapping{}, errors.New("archive removal outcome is ambiguous")
	}
	at = actor.clock.Now().UTC()
	if at.IsZero() {
		return statepkg.Mapping{}, errors.New("invalid archive removal time")
	}
	target, ok = removedArchiveMapping(loaded, archiveID, at)
	if !ok {
		return statepkg.Mapping{}, errors.New("archive disappeared before retry")
	}
	outcome, mutationErr = store.RemoveSession(actor.identity, archiveID, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		return target, nil
	}
	loaded, class = classifyLoadedState(actor.state, actor.identity, loaded, target)
	if class == mappingTarget && knownCommitOutcome(outcome) {
		return loaded, nil
	}
	return statepkg.Mapping{}, errors.New("archive removal failed")
}

func removedArchiveMapping(before statepkg.Mapping, archiveID string, at time.Time) (statepkg.Mapping, bool) {
	result := cloneMapping(before)
	index := -1
	for candidate := range result.Archives {
		if result.Archives[candidate].ConversationID == archiveID {
			index = candidate
			break
		}
	}
	if index < 0 {
		return statepkg.Mapping{}, false
	}
	result.Archives = append(result.Archives[:index], result.Archives[index+1:]...)
	result.UpdatedAt = maxTime(result.UpdatedAt, at)
	return result, true
}

func (actor *conversation) handleArchiveResult(attachments map[*clientAttachment]struct{}, recoveryResults chan<- recoveryWorkerResult, result archiveWorkerResult) {
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
		actor.contextState = protocol.ContextUnavailable
		actor.dispatchBlocked = true
		actor.recoveryUnavailable = true
		actor.lifecycle = protocol.LifecycleUnavailable
		actor.publishBrowserError(attachments, protocol.ErrorStateRepairFailed)
		actor.publishShared(attachments, actor.lifecyclePayload())
	}
	if result.code != "" {
		actor.completePendingCommand(attachments, result.commandID, result.clientID, result.code)
	} else {
		actor.publishShared(attachments, protocol.ArchivePayload{Action: protocol.ArchiveDeleted, ArchiveID: result.archiveID})
		if observationErr != nil {
			actor.completePendingCommand(attachments, result.commandID, result.clientID, protocol.ErrorStateRepairFailed)
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
				actor.publishBrowserError(attachments, protocol.ErrorProviderCrashed)
			}
			return
		}
		actor.startRecovery(attachments, recoveryResults, trigger)
	}
}
