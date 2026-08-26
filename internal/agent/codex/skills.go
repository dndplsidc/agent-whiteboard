package codex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

type nativeSkill struct {
	id          string
	name        string
	path        string
	displayName string
	description string
	scope       provider.SkillScope
}

type nativeSkillCatalog struct {
	state provider.SkillsState
	byID  map[string]nativeSkill
	order []string
}

func unavailableSkillCatalog() nativeSkillCatalog {
	return nativeSkillCatalog{state: provider.SkillsUnavailable, byID: make(map[string]nativeSkill), order: []string{}}
}

func (catalog nativeSkillCatalog) clone() nativeSkillCatalog {
	result := nativeSkillCatalog{state: catalog.state, byID: make(map[string]nativeSkill, len(catalog.byID)), order: append([]string(nil), catalog.order...)}
	for id, skill := range catalog.byID {
		result.byID[id] = skill
	}
	return result
}

func (catalog nativeSkillCatalog) safeCatalog() provider.SkillCatalog {
	if catalog.state != provider.SkillsReady {
		return provider.SkillCatalog{State: provider.SkillsUnavailable, Skills: []provider.SkillDescriptor{}, MaxSelectedSkills: 0}
	}
	skills := make([]provider.SkillDescriptor, 0, len(catalog.order))
	for _, id := range catalog.order {
		skill, ok := catalog.byID[id]
		if !ok {
			return provider.SkillCatalog{State: provider.SkillsUnavailable, Skills: []provider.SkillDescriptor{}, MaxSelectedSkills: 0}
		}
		skills = append(skills, provider.SkillDescriptor{ID: skill.id, Name: skill.name, DisplayName: skill.displayName, Description: skill.description, Scope: skill.scope})
	}
	result := provider.SkillCatalog{State: provider.SkillsReady, Skills: skills, MaxSelectedSkills: provider.MaxMessageSkills}
	if result.Validate() != nil {
		return provider.SkillCatalog{State: provider.SkillsUnavailable, Skills: []provider.SkillDescriptor{}, MaxSelectedSkills: 0}
	}
	return result
}

func (catalog nativeSkillCatalog) resolve(content provider.MessageContent) ([]nativeSkill, error) {
	if content.Validate() != nil {
		return nil, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	result := make([]nativeSkill, 0)
	for _, part := range content.Parts {
		if part.Kind != provider.MessagePartSkill {
			continue
		}
		if catalog.state != provider.SkillsReady || part.Skill == nil {
			return nil, provider.NewProviderError(provider.ErrorSkillUnavailable)
		}
		skill, exists := catalog.byID[part.Skill.ID]
		if !exists || skill.name != part.Skill.Name {
			return nil, provider.NewProviderError(provider.ErrorSkillUnavailable)
		}
		result = append(result, skill)
	}
	return result, nil
}

func parseSkillCatalog(raw json.RawMessage, workspace string, previous nativeSkillCatalog, newID func() (string, error)) (nativeSkillCatalog, error) {
	if !validAbsolutePath(workspace) || newID == nil || validateJSONStructure(raw) != nil {
		return nativeSkillCatalog{}, errors.New("invalid skill catalog")
	}
	var response struct {
		Data []struct {
			CWD    string            `json:"cwd"`
			Skills []json.RawMessage `json:"skills"`
			Errors []json.RawMessage `json:"errors"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Data == nil || len(response.Data) > provider.MaxSkills {
		return nativeSkillCatalog{}, errors.New("invalid skill catalog")
	}
	var selected *struct {
		CWD    string
		Skills []json.RawMessage
		Errors []json.RawMessage
	}
	for index := range response.Data {
		entry := response.Data[index]
		if !validAbsolutePath(entry.CWD) || entry.Skills == nil || entry.Errors == nil || len(entry.Errors) > provider.MaxSkills {
			return nativeSkillCatalog{}, errors.New("invalid skill catalog")
		}
		for _, loadError := range entry.Errors {
			if len(loadError) > provider.MaxSkillDescriptionBytes || !json.Valid(loadError) {
				return nativeSkillCatalog{}, errors.New("invalid skill catalog")
			}
		}
		if entry.CWD == workspace {
			if selected != nil {
				return nativeSkillCatalog{}, errors.New("invalid skill catalog")
			}
			selected = &struct {
				CWD    string
				Skills []json.RawMessage
				Errors []json.RawMessage
			}{entry.CWD, entry.Skills, entry.Errors}
		}
	}
	if selected == nil || len(selected.Skills) > provider.MaxSkills {
		return nativeSkillCatalog{}, errors.New("invalid skill catalog")
	}
	previousByIdentity := make(map[string]string, len(previous.byID))
	for id, skill := range previous.byID {
		previousByIdentity[skillIdentity(skill.scope, skill.name, skill.path)] = id
	}
	result := nativeSkillCatalog{state: provider.SkillsReady, byID: make(map[string]nativeSkill), order: make([]string, 0, len(selected.Skills))}
	seenIdentities := make(map[string]struct{}, len(selected.Skills))
	seenNames := make(map[string]struct{}, len(selected.Skills))
	for _, rawSkill := range selected.Skills {
		var dto struct {
			Name             string `json:"name"`
			Description      string `json:"description"`
			ShortDescription string `json:"shortDescription"`
			Path             string `json:"path"`
			Scope            string `json:"scope"`
			Enabled          *bool  `json:"enabled"`
			Interface        *struct {
				DisplayName      string `json:"displayName"`
				ShortDescription string `json:"shortDescription"`
			} `json:"interface"`
		}
		if len(rawSkill) > provider.MaxSkillsCatalogBytes || validateJSONStructure(rawSkill) != nil || json.Unmarshal(rawSkill, &dto) != nil || dto.Enabled == nil || !validAbsolutePath(dto.Path) {
			return nativeSkillCatalog{}, errors.New("invalid skill catalog")
		}
		if !*dto.Enabled {
			continue
		}
		description := dto.Description
		displayName := dto.Name
		if dto.ShortDescription != "" {
			description = dto.ShortDescription
		}
		if dto.Interface != nil {
			if dto.Interface.DisplayName != "" {
				displayName = dto.Interface.DisplayName
			}
			if dto.Interface.ShortDescription != "" {
				description = dto.Interface.ShortDescription
			}
		}
		scope := provider.SkillScope(dto.Scope)
		candidate := provider.SkillDescriptor{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Name: dto.Name, DisplayName: displayName, Description: description, Scope: scope}
		if dto.Description == "" || candidate.Validate() != nil {
			return nativeSkillCatalog{}, errors.New("invalid skill catalog")
		}
		identity := skillIdentity(scope, dto.Name, dto.Path)
		if _, duplicate := seenIdentities[identity]; duplicate {
			return nativeSkillCatalog{}, errors.New("invalid skill catalog")
		}
		if _, duplicate := seenNames[dto.Name]; duplicate {
			return nativeSkillCatalog{}, errors.New("invalid skill catalog")
		}
		seenIdentities[identity] = struct{}{}
		seenNames[dto.Name] = struct{}{}
		id := previousByIdentity[identity]
		if id == "" {
			var err error
			id, err = newID()
			if err != nil {
				return nativeSkillCatalog{}, errors.New("invalid skill catalog")
			}
		}
		skill := nativeSkill{id: id, name: dto.Name, path: filepath.Clean(dto.Path), displayName: displayName, description: description, scope: scope}
		if (provider.SkillDescriptor{ID: id, Name: skill.name, DisplayName: skill.displayName, Description: skill.description, Scope: skill.scope}).Validate() != nil {
			return nativeSkillCatalog{}, errors.New("invalid skill catalog")
		}
		result.byID[id] = skill
		result.order = append(result.order, id)
	}
	if result.safeCatalog().Validate() != nil {
		return nativeSkillCatalog{}, errors.New("invalid skill catalog")
	}
	return result, nil
}

func skillIdentity(scope provider.SkillScope, name, path string) string {
	return string(scope) + "\x00" + name + "\x00" + path
}

func (session *Session) Skills(ctx context.Context) provider.SkillCatalog {
	session.skillsMu.Lock()
	defer session.skillsMu.Unlock()
	session.mu.Lock()
	closed := session.closed
	previous := session.skills.clone()
	loaded := session.skillsLoaded
	session.mu.Unlock()
	if closed {
		return provider.SkillCatalog{State: provider.SkillsUnavailable, Skills: []provider.SkillDescriptor{}, MaxSelectedSkills: 0}
	}
	if loaded {
		return previous.safeCatalog()
	}
	return session.loadSkillsLocked(ctx, previous, 0, false)
}

func (session *Session) loadSkillsLocked(ctx context.Context, previous nativeSkillCatalog, generation uint64, emit bool) provider.SkillCatalog {
	result, err := session.runtime.call(ctx, "skills/list", map[string]any{"cwds": []string{session.workspace}, "forceReload": false})
	catalog := unavailableSkillCatalog()
	if err == nil {
		if parsed, parseErr := parseSkillCatalog(result, session.workspace, previous, session.driver.newID); parseErr == nil {
			catalog = parsed
		}
	}
	if generation != 0 {
		session.runtime.skillsMu.Lock()
		currentGeneration := session.runtime.skillsGeneration
		session.runtime.skillsMu.Unlock()
		if generation < currentGeneration {
			session.mu.Lock()
			safe := session.skills.safeCatalog()
			session.mu.Unlock()
			return safe
		}
	}
	session.mu.Lock()
	if session.closed || generation != 0 && generation < session.skillsGeneration {
		safe := session.skills.safeCatalog()
		session.mu.Unlock()
		return safe
	}
	session.skills = catalog
	session.skillsLoaded = true
	if generation > session.skillsGeneration {
		session.skillsGeneration = generation
	}
	safe := catalog.safeCatalog()
	session.mu.Unlock()
	if emit {
		session.emit(provider.NewSkillCatalogEvent(safe))
	}
	return safe
}

func (session *Session) refreshSkills(ctx context.Context, generation uint64) {
	session.skillsMu.Lock()
	defer session.skillsMu.Unlock()
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return
	}
	previous := session.skills.clone()
	session.mu.Unlock()
	session.loadSkillsLocked(ctx, previous, generation, true)
}

func (runtime *runtime) scheduleSkillRefresh() {
	runtime.skillsMu.Lock()
	runtime.skillsGeneration++
	if runtime.skillsRefreshing {
		runtime.skillsMu.Unlock()
		return
	}
	runtime.skillsRefreshing = true
	runtime.skillsMu.Unlock()
	go runtime.runSkillRefresh()
}

func (runtime *runtime) runSkillRefresh() {
	for {
		runtime.skillsMu.Lock()
		generation := runtime.skillsGeneration
		runtime.skillsMu.Unlock()
		runtime.mu.Lock()
		sessions := make([]*Session, 0, len(runtime.sessions))
		for _, session := range runtime.sessions {
			sessions = append(sessions, session)
		}
		runtime.mu.Unlock()
		var group sync.WaitGroup
		for _, session := range sessions {
			group.Add(1)
			go func(session *Session) {
				defer group.Done()
				session.refreshSkills(context.Background(), generation)
			}(session)
		}
		group.Wait()
		runtime.skillsMu.Lock()
		if generation == runtime.skillsGeneration {
			runtime.skillsRefreshing = false
			runtime.skillsMu.Unlock()
			return
		}
		runtime.skillsMu.Unlock()
	}
}
