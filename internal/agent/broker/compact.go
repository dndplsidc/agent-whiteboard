package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

func (actor *conversation) commandCompact(attachments map[*clientAttachment]struct{}, results chan<- turnWorkerResult, command protocol.Command, payload protocol.CompactPayload) (bool, protocol.BrowserErrorCode) {
	if actor.identity.Provider != provider.NameCodex || !actor.supportsCompact {
		return false, protocol.ErrorCompactUnsupported
	}
	if actor.active != nil || actor.compact != nil || !actor.queue.Empty() || actor.workerSettled != nil || actor.deferredInterrupt != nil || actor.recoveryActive || actor.dispatchBlocked || actor.stopping || actor.mapping.Current == nil || actor.mapping.Current.PreparedCommit != nil || actor.lifecycle != protocol.LifecycleReady {
		return false, protocol.ErrorInvalidState
	}
	session, ok := actor.session.session.(provider.ManualCompactSession)
	if !ok || !session.SupportsCompact() {
		actor.supportsCompact = false
		return false, protocol.ErrorCompactUnsupported
	}
	request := provider.CompactRequest{WorkID: payload.WorkID}
	if request.Validate() != nil {
		return false, protocol.ErrorInvalidCommand
	}
	actor.compact = &activeCompact{request: request, phase: compactStarting, originCommandID: command.CommandID, originClientID: command.ClientID}
	actor.beginProviderWorker(providerWorkerCompact, command.CommandID, command.ClientID)
	generation := actor.generation
	go func() {
		accepted, err := session.Compact(actor.lifecycleCtx, request)
		if err == nil && (accepted.Validate() != nil || accepted.WorkID != request.WorkID) {
			err = errors.New("invalid provider accepted compact work")
		}
		results <- turnWorkerResult{generation: generation, kind: turnWorkerCompactStart, workID: request.WorkID, acceptedCompact: accepted, commandID: command.CommandID, clientID: command.ClientID, err: err}
	}()
	return true, ""
}

func (actor *conversation) startCompactInterruptWorker(results chan<- turnWorkerResult, commandID, clientID string) {
	compact := actor.compact
	accepted := *compact.accepted
	session := actor.session.session.(provider.ManualCompactSession)
	actor.beginProviderWorker(providerWorkerInterrupt, commandID, clientID)
	generation := actor.generation
	go func() {
		err := session.InterruptCompact(actor.lifecycleCtx, accepted)
		results <- turnWorkerResult{generation: generation, kind: turnWorkerCompactInterrupt, workID: accepted.WorkID, commandID: commandID, clientID: clientID, err: err}
	}()
}

func (actor *conversation) handleCompactWorkerResult(attachments map[*clientAttachment]struct{}, _ chan<- turnWorkerResult, result turnWorkerResult) {
	if actor.compact == nil || actor.compact.request.WorkID != result.workID {
		if result.commandID != "" {
			actor.completePendingCommand(attachments, result.commandID, result.clientID, protocol.ErrorInvalidState)
		}
		return
	}
	if result.kind == turnWorkerCompactInterrupt {
		if result.err != nil {
			actor.compact.phase = compactRunning
			actor.publishShared(attachments, actor.lifecyclePayload())
			actor.publishShared(attachments, protocol.CompactionPayload{WorkID: result.workID, Status: protocol.CompactionRunning})
			actor.completePendingCommand(attachments, result.commandID, result.clientID, MapError(result.err).Code())
			return
		}
		actor.completePendingCommand(attachments, result.commandID, result.clientID, "")
		return
	}
	if result.err != nil {
		code := MapError(result.err).Code()
		if code == protocol.ErrorCompactUnsupported {
			actor.supportsCompact = false
		}
		actor.compact = nil
		actor.lifecycle = protocol.LifecycleReady
		actor.completePendingCommand(attachments, result.commandID, result.clientID, code)
		return
	}
	accepted := result.acceptedCompact
	actor.compact.accepted = &accepted
	actor.compact.phase = compactRunning
	pendingTerminal := actor.compact.pendingTerminal
	actor.compact.pendingTerminal = nil
	originCommandID := actor.compact.originCommandID
	originClientID := actor.compact.originClientID
	actor.compact.originCommandID = ""
	actor.compact.originClientID = ""
	actor.lifecycle = protocol.LifecycleCompacting
	actor.publishShared(attachments, actor.lifecyclePayload())
	actor.publishShared(attachments, protocol.CompactionPayload{WorkID: accepted.WorkID, Status: protocol.CompactionRunning})
	actor.completePendingCommand(attachments, originCommandID, originClientID, "")
	if pendingTerminal != nil {
		actor.publishCompactTerminal(attachments, *pendingTerminal)
	}
}

func (actor *conversation) publishSkillCatalog(attachments map[*clientAttachment]struct{}, source provider.Event) {
	if actor.identity.Provider != provider.NameCodex || source.SkillCatalog == nil || !actor.applySkillCatalog(*source.SkillCatalog) {
		actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
		return
	}
	state := *actor.skillsState
	actor.publishShared(attachments, protocol.SkillCatalogPayload{State: state, Skills: append([]protocol.SkillDescriptor{}, actor.skills...)})
}

func (actor *conversation) publishCompactTerminal(attachments map[*clientAttachment]struct{}, source provider.Event) {
	if actor.identity.Provider != provider.NameCodex || source.Compact == nil || actor.compact == nil || actor.compact.accepted == nil || source.Compact.WorkID != actor.compact.request.WorkID {
		actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
		return
	}
	status := protocol.CompactionStatus(source.Compact.Status)
	if status != protocol.CompactionCompleted && status != protocol.CompactionInterrupted && status != protocol.CompactionFailed {
		actor.publishBrowserError(attachments, protocol.ErrorProviderMalformedStream)
		return
	}
	workID := actor.compact.request.WorkID
	actor.publishShared(attachments, protocol.CompactionPayload{WorkID: workID, Status: status})
	actor.compact = nil
	if status == protocol.CompactionInterrupted {
		actor.lifecycle = protocol.LifecycleInterrupted
	} else {
		actor.lifecycle = protocol.LifecycleReady
	}
	actor.publishShared(attachments, actor.lifecyclePayload())
}
