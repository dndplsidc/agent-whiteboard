package broker

import (
	"context"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type compactPhase uint8

const (
	compactStarting compactPhase = iota
	compactRunning
	compactStopping
)

type activeCompact struct {
	request         provider.CompactRequest
	accepted        *provider.AcceptedCompact
	phase           compactPhase
	originCommandID string
	originClientID  string
	pendingTerminal *provider.Event
}

func (actor *conversation) loadSessionFeatures(ctx context.Context) {
	unavailable := protocol.SkillsUnavailable
	actor.skillsState = &unavailable
	actor.skills = []protocol.SkillDescriptor{}
	actor.supportsCompact = false
	if session, ok := actor.session.session.(provider.SkillCatalogSession); ok {
		actor.applySkillCatalog(session.Skills(ctx))
	}
	if session, ok := actor.session.session.(provider.ManualCompactSession); ok {
		actor.supportsCompact = session.SupportsCompact()
	}
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
	if err := protocol.ValidateSkillCatalog(state, skills); err != nil {
		return false
	}
	actor.skillsState = &state
	actor.skills = skills
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
	if actor.identity.Provider != provider.NameCodex || actor.skillsState == nil || *actor.skillsState != protocol.SkillsReady {
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
