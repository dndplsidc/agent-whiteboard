package broker

import (
	"context"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

type compactPhase uint8

const (
	compactStarting compactPhase = iota
	compactRunning
	compactStopping
	compactAcceptanceUnknown
)

type activeCompact struct {
	request         provider.CompactRequest
	accepted        *provider.AcceptedCompact
	phase           compactPhase
	originCommandID string
	originClientID  string
	pendingTerminal *provider.Event
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func (actor *conversation) loadSessionFeatures(ctx context.Context) {
	actor.makeSkillsUnavailable()
	actor.supportsCompact = false
	actor.busyPolicy = protocol.BusyTurnPreserveDraft
	if session, ok := actor.session.session.(provider.SkillCatalogSession); ok {
		actor.applySkillCatalog(session.Skills(ctx))
	}
	if session, ok := actor.session.session.(provider.ManualCompactSession); ok {
		actor.supportsCompact = session.SupportsCompact()
	}
	if session, ok := actor.session.session.(provider.BusyTurnSession); ok {
		policy := session.BusyTurnPolicy()
		if policy.Valid() {
			actor.busyPolicy = protocol.BusyTurnPolicy(policy)
		}
	}
}

func (actor *conversation) makeSkillsUnavailable() {
	state := protocol.SkillsUnavailable
	actor.skillsState = &state
	actor.skills = []protocol.SkillDescriptor{}
	actor.maxSelectedSkills = nil
}

func (actor *conversation) applySkillCatalog(catalog provider.SkillCatalog) bool {
	if catalog.Validate() != nil {
		return false
	}
	state := protocol.SkillsState(catalog.State)
	skills := make([]protocol.SkillDescriptor, len(catalog.Skills))
	for index, skill := range catalog.Skills {
		skills[index] = protocol.SkillDescriptor{ID: skill.ID, Name: skill.Name, DisplayName: skill.DisplayName, Description: skill.Description, Scope: protocol.SkillScope(skill.Scope)}
	}
	var limit *int
	if catalog.State == provider.SkillsReady {
		value := catalog.MaxSelectedSkills
		limit = &value
	}
	if err := protocol.ValidateSkillCatalogWithLimit(state, skills, limit); err != nil {
		return false
	}
	actor.skillsState = &state
	actor.skills = skills
	if catalog.State == provider.SkillsReady {
		limit := catalog.MaxSelectedSkills
		actor.maxSelectedSkills = &limit
	} else {
		actor.maxSelectedSkills = nil
	}
	return true
}

func (actor *conversation) validateSelectedSkills(content protocol.MessageContent) protocol.BrowserErrorCode {
	selected := 0
	for _, part := range content.Parts {
		if part.Type == protocol.MessagePartSkill {
			selected++
		}
	}
	if selected == 0 {
		return ""
	}
	if actor.skillsState == nil || *actor.skillsState != protocol.SkillsReady || actor.maxSelectedSkills == nil || selected > *actor.maxSelectedSkills {
		return protocol.ErrorSkillUnavailable
	}
	byID := make(map[string]protocol.SkillDescriptor, len(actor.skills))
	for _, skill := range actor.skills {
		byID[skill.ID] = skill
	}
	for _, part := range content.Parts {
		if part.Type != protocol.MessagePartSkill || part.Skill == nil {
			continue
		}
		skill, exists := byID[part.Skill.ID]
		if !exists || skill.Name != part.Skill.Name {
			return protocol.ErrorSkillUnavailable
		}
	}
	return ""
}

func (actor *conversation) composerAdmission() protocol.ComposerAdmission {
	idleLifecycle := actor.lifecycle == protocol.LifecycleReady || actor.lifecycle == protocol.LifecycleInterrupted
	unsafe := !idleLifecycle || actor.compact != nil || actor.workerSettled != nil && actor.active == nil || actor.dispatchBlocked || actor.stopping || actor.recoveryActive || actor.handoffActive || actor.mapping.Current == nil || actor.mapping.Current.PreparedCommit != nil
	if unsafe {
		return protocol.ComposerBlocked
	}
	if actor.active == nil && actor.queue.Empty() {
		return protocol.ComposerSubmit
	}
	if actor.busyPolicy == protocol.BusyTurnQueue && actor.queue.Len() < MaxQueueItems && actor.queue.Bytes() < MaxQueueBytes {
		return protocol.ComposerQueue
	}
	if actor.busyPolicy == protocol.BusyTurnPreserveDraft {
		return protocol.ComposerPreserveDraft
	}
	return protocol.ComposerBlocked
}

func (actor *conversation) activeWork() *protocol.ActiveWork {
	if actor.lifecycle == protocol.LifecycleResponding && actor.active != nil {
		state := protocol.ActiveWorkRunning
		if actor.active.phase == turnInterrupting || actor.active.phase == turnInterruptRequested {
			state = protocol.ActiveWorkStopping
		}
		return &protocol.ActiveWork{WorkID: actor.active.request.TurnID, Kind: protocol.ActiveWorkTurn, State: state}
	}
	if actor.lifecycle == protocol.LifecycleCompacting && actor.compact != nil {
		state := protocol.ActiveWorkRunning
		if actor.compact.phase == compactStopping {
			state = protocol.ActiveWorkStopping
		}
		return &protocol.ActiveWork{WorkID: actor.compact.request.WorkID, Kind: protocol.ActiveWorkCompact, State: state}
	}
	return nil
}

func (actor *conversation) lifecyclePayload() protocol.LifecyclePayload {
	return protocol.LifecyclePayload{State: actor.lifecycle, ActiveWork: actor.activeWork()}
}
