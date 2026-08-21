//go:build unix

package pi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type piModel struct {
	provider      string
	id            string
	name          string
	reasoning     bool
	efforts       []provider.ReasoningEffort
	images        bool
	contextWindow int
	maxTokens     int
}

type availableModelsResponse struct {
	Models []json.RawMessage `json:"models"`
}

func parseAvailableModels(raw json.RawMessage, current startupState) (provider.ModelCatalog, map[string]piModel, error) {
	var wire availableModelsResponse
	if decodeStartupData(raw, &wire) != nil || wire.Models == nil || len(wire.Models) == 0 || len(wire.Models) > provider.MaxCatalogModels {
		return provider.ModelCatalog{}, nil, errors.New("invalid Pi model catalog")
	}
	models := make([]provider.CatalogModel, 0, len(wire.Models))
	private := make(map[string]piModel, len(wire.Models))
	for _, record := range wire.Models {
		var item struct {
			Provider         string             `json:"provider"`
			ID               string             `json:"id"`
			Name             string             `json:"name"`
			Reasoning        *bool              `json:"reasoning"`
			ThinkingLevelMap map[string]*string `json:"thinkingLevelMap"`
			Input            []string           `json:"input"`
			ContextWindow    int                `json:"contextWindow"`
			MaxTokens        int                `json:"maxTokens"`
		}
		if len(record) > provider.MaxCatalogBytes || decodeStartupData(record, &item) != nil || item.Reasoning == nil || !validStartupText(item.Provider, provider.MaxTitleBytes) || !validStartupText(item.ID, provider.MaxTitleBytes) || !validStartupText(item.Name, provider.MaxTitleBytes) || item.ContextWindow <= 0 || item.MaxTokens <= 0 {
			return provider.ModelCatalog{}, nil, errors.New("invalid Pi model record")
		}
		if !validModelModalities(item.Input) {
			return provider.ModelCatalog{}, nil, errors.New("invalid Pi model modalities")
		}
		value := item.Provider + "/" + item.ID
		if len(value) > provider.MaxModelValueBytes {
			return provider.ModelCatalog{}, nil, errors.New("invalid Pi model identity")
		}
		efforts, err := piReasoningEfforts(*item.Reasoning, item.ThinkingLevelMap)
		if err != nil {
			return provider.ModelCatalog{}, nil, err
		}
		if _, duplicate := private[value]; duplicate {
			return provider.ModelCatalog{}, nil, errors.New("duplicate Pi model identity")
		}
		defaultEffort := efforts[0].Value
		if value == current.Model {
			defaultEffort = current.ThinkingLevel
			if defaultEffort == "" {
				defaultEffort = "off"
			}
		}
		found := false
		for _, effort := range efforts {
			found = found || effort.Value == defaultEffort
		}
		if !found {
			return provider.ModelCatalog{}, nil, errors.New("Pi effective effort is unsupported")
		}
		model := provider.CatalogModel{Model: value, DisplayName: item.Name, DefaultEffort: defaultEffort, SupportedReasoningEfforts: efforts, SupportsImages: modelSupportsImages(item.Input), Default: value == current.Model}
		models = append(models, model)
		private[value] = piModel{provider: item.Provider, id: item.ID, name: item.Name, reasoning: *item.Reasoning, efforts: efforts, images: model.SupportsImages, contextWindow: item.ContextWindow, maxTokens: item.MaxTokens}
	}
	catalog := provider.ModelCatalog{Models: models}
	if catalog.Validate() != nil {
		return provider.ModelCatalog{}, nil, errors.New("invalid Pi model catalog")
	}
	return catalog, private, nil
}

func piReasoningEfforts(reasoning bool, mapping map[string]*string) ([]provider.ReasoningEffort, error) {
	if !reasoning {
		return []provider.ReasoningEffort{{Value: "off"}}, nil
	}
	allowed := map[string]bool{"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	for key, mapped := range mapping {
		if !allowed[key] || mapped != nil && !validStartupText(*mapped, provider.MaxEffortValueBytes) {
			return nil, errors.New("unsupported Pi thinking level")
		}
	}
	result := make([]provider.ReasoningEffort, 0, 7)
	for _, level := range []string{"off", "minimal", "low", "medium", "high"} {
		if mapped, exists := mapping[level]; exists && mapped == nil {
			continue
		}
		result = append(result, provider.ReasoningEffort{Value: level})
	}
	for _, level := range []string{"xhigh", "max"} {
		if mapped, exists := mapping[level]; exists && mapped != nil {
			result = append(result, provider.ReasoningEffort{Value: level})
		}
	}
	return result, nil
}

func validModelModalities(modalities []string) bool {
	if modalities == nil {
		return false
	}
	seen := make(map[string]struct{}, len(modalities))
	for _, modality := range modalities {
		if modality != "text" && modality != "image" {
			return false
		}
		if _, duplicate := seen[modality]; duplicate {
			return false
		}
		seen[modality] = struct{}{}
	}
	return true
}

func (s *Session) loadSettingsCatalog(ctx context.Context) (provider.ModelCatalog, map[string]piModel, error) {
	response, _, err := s.rpc.call(ctx, "get_available_models", nil)
	if err != nil || requireSuccessfulResponse(response, "get_available_models") != nil {
		return provider.ModelCatalog{}, nil, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	catalog, private, err := parseAvailableModels(response.Data, state)
	if err != nil {
		return provider.ModelCatalog{}, nil, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	return catalog, private, nil
}

func (s *Session) SettingsCatalog(ctx context.Context) (provider.ModelCatalog, error) {
	catalog, private, err := s.loadSettingsCatalog(ctx)
	if err != nil {
		return provider.ModelCatalog{}, err
	}
	s.mu.Lock()
	s.models = private
	s.catalog = catalog.Clone()
	s.mu.Unlock()
	return catalog.Clone(), nil
}

func (s *Session) EffectiveSettings(ctx context.Context) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	state, err := s.readEffectiveState(ctx)
	if err != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, err
	}
	settings, presentation := state.settings()
	return settings, presentation, nil
}

func (s *Session) readEffectiveState(ctx context.Context) (startupState, error) {
	response, _, err := s.rpc.call(ctx, "get_state", nil)
	if err != nil || requireSuccessfulResponse(response, "get_state") != nil {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	var native startupRPCState
	if decodeStartupData(response.Data, &native) != nil || native.ThinkingLevel == nil || native.IsStreaming == nil || native.IsCompacting == nil || native.PendingMessageCount == nil || native.SessionFile == nil || native.SessionID == nil || bytes.Equal(bytes.TrimSpace(native.Model), []byte("null")) {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	if *native.IsStreaming || *native.IsCompacting || *native.PendingMessageCount != 0 {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	var model startupModel
	if decodeStartupData(native.Model, &model) != nil || !validStartupText(model.Provider, provider.MaxTitleBytes) || !validStartupText(model.ID, provider.MaxTitleBytes) {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	name := model.Name
	if name == "" {
		name = model.ID
	}
	state := startupState{Model: model.Provider + "/" + model.ID, ModelProvider: model.Provider, ModelID: model.ID, ModelName: name, ThinkingLevel: *native.ThinkingLevel, ContextWindow: model.ContextWindow, MaxTokens: model.MaxTokens, SupportsImages: modelSupportsImages(model.Input)}
	s.mu.Lock()
	expectedFile, expectedID := s.state.SessionFile, s.state.SessionID
	s.mu.Unlock()
	if *native.SessionFile != expectedFile || *native.SessionID != expectedID || !validModelModalities(model.Input) || model.ContextWindow <= 0 || model.MaxTokens <= 0 {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	settings, presentation := state.settings()
	if settings.Validate() != nil || presentation.Validate() != nil {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	return state, nil
}

const settingsRecoveryTimeout = time.Second

type preparedPiSettings struct {
	requested provider.ExecutionSettings
	catalog   provider.ModelCatalog
	private   map[string]piModel
	model     piModel
}

func (s *Session) prepareSettings(ctx context.Context, requested provider.ExecutionSettings) (preparedPiSettings, error) {
	catalog, private, err := s.loadSettingsCatalog(ctx)
	if err != nil {
		return preparedPiSettings{}, err
	}
	if _, err := catalog.Resolve(requested); err != nil || requested.Speed != provider.SpeedStandard {
		return preparedPiSettings{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	model, ok := private[requested.Model]
	if !ok {
		return preparedPiSettings{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	return preparedPiSettings{requested: requested, catalog: catalog, private: private, model: model}, nil
}

func (s *Session) ApplySettings(ctx context.Context, requested provider.ExecutionSettings) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	s.admission.Lock()
	defer s.admission.Unlock()
	s.mu.Lock()
	busy := s.active != nil || s.compact != nil || s.shutdownStarted
	s.mu.Unlock()
	if busy {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	prepared, err := s.prepareSettings(ctx, requested)
	if err != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, err
	}
	s.mu.Lock()
	busy = s.active != nil || s.compact != nil || s.shutdownStarted
	s.mu.Unlock()
	if busy {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return s.applySettingsAdmitted(ctx, prepared)
}

func (s *Session) applySettingsAdmitted(ctx context.Context, prepared preparedPiSettings) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	requested, catalog, private := prepared.requested, prepared.catalog, prepared.private
	s.mu.Lock()
	current, _ := s.state.settings()
	s.mu.Unlock()
	applyErr := error(nil)
	mutationPossible := false
	if current.Model != requested.Model {
		response, wrote, callErr := s.rpc.call(ctx, "set_model", map[string]any{"provider": prepared.model.provider, "modelId": prepared.model.id})
		mutationPossible = mutationPossible || wrote
		if callErr != nil || requireSuccessfulResponse(response, "set_model") != nil {
			applyErr = provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	if applyErr == nil && current.Effort != requested.Effort {
		response, wrote, callErr := s.rpc.call(ctx, "set_thinking_level", map[string]any{"level": requested.Effort})
		mutationPossible = mutationPossible || wrote
		if callErr != nil || requireSuccessfulResponse(response, "set_thinking_level") != nil {
			applyErr = provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	reconcileContext := ctx
	cancelRecovery := func() {}
	if mutationPossible {
		reconcileContext, cancelRecovery = context.WithTimeout(context.Background(), settingsRecoveryTimeout)
	}
	defer cancelRecovery()
	effectiveState, stateErr := s.readEffectiveState(reconcileContext)
	if stateErr != nil {
		if mutationPossible {
			s.rpc.finish(provider.NewProviderError(provider.ErrorProtocolFailure))
			return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, stateErr
	}
	effective, presentation := effectiveState.settings()
	updatedAt := s.driver.config.Clock.Now().UTC()
	s.mu.Lock()
	candidate := s.native
	if updatedAt.Before(candidate.UpdatedAt) {
		updatedAt = candidate.UpdatedAt
	}
	s.mu.Unlock()
	copySettings, copyPresentation := effective, presentation
	candidate.Model, candidate.Settings, candidate.Presentation, candidate.UpdatedAt = effective.Model, &copySettings, &copyPresentation, updatedAt
	persistErr := error(nil)
	if s.driver == nil || s.driver.native == nil {
		persistErr = errors.New("Pi native metadata unavailable")
	} else {
		persistErr = s.driver.native.updateSettings(candidate)
	}
	s.mu.Lock()
	s.state.Model, s.state.ModelProvider, s.state.ModelID, s.state.ModelName, s.state.ThinkingLevel, s.state.ContextWindow, s.state.MaxTokens, s.state.SupportsImages = effectiveState.Model, effectiveState.ModelProvider, effectiveState.ModelID, effectiveState.ModelName, effectiveState.ThinkingLevel, effectiveState.ContextWindow, effectiveState.MaxTokens, effectiveState.SupportsImages
	s.models, s.catalog = private, catalog.Clone()
	s.native = candidate
	s.mu.Unlock()
	if persistErr != nil {
		s.emit(provider.NewVerifiedSettingsEvent("", effective, presentation))
		s.rpc.finish(provider.NewProviderError(provider.ErrorProtocolFailure))
		return effective, presentation, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if callerErr := ctx.Err(); callerErr != nil {
		s.emit(provider.NewVerifiedSettingsEvent("", effective, presentation))
		return effective, presentation, callerErr
	}
	if applyErr != nil || effective != requested {
		s.emit(provider.NewVerifiedSettingsEvent("", effective, presentation))
		return effective, presentation, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	return effective, presentation, nil
}

type piSkill struct {
	id, name string
	scope    provider.SkillScope
}
type piSkillCatalog struct {
	byID  map[string]piSkill
	order []string
}

func unavailablePiSkills() provider.SkillCatalog {
	return provider.SkillCatalog{State: provider.SkillsUnavailable, Skills: []provider.SkillDescriptor{}, MaxSelectedSkills: 0}
}

func parsePiSkills(raw json.RawMessage) (piSkillCatalog, provider.SkillCatalog, error) {
	var response struct {
		Commands []json.RawMessage `json:"commands"`
	}
	if decodeStartupData(raw, &response) != nil || response.Commands == nil || len(response.Commands) > provider.MaxSkills {
		return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("invalid Pi command catalog")
	}
	private := piSkillCatalog{byID: make(map[string]piSkill), order: []string{}}
	safe := provider.SkillCatalog{State: provider.SkillsReady, Skills: []provider.SkillDescriptor{}, MaxSelectedSkills: 1}
	seenNames := make(map[string]struct{})
	for _, rawCommand := range response.Commands {
		var command struct {
			Name, Description, Source string
			SourceInfo                json.RawMessage `json:"sourceInfo"`
		}
		if len(rawCommand) > provider.MaxSkillDescriptionBytes*2 || decodeStrict(rawCommand, &command) != nil || command.Source == "" {
			return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("invalid Pi command")
		}
		if command.Source != "skill" {
			continue
		}
		if !strings.HasPrefix(command.Name, "skill:") || len(command.Description) > provider.MaxSkillDescriptionBytes {
			return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("invalid Pi skill")
		}
		name := strings.TrimPrefix(command.Name, "skill:")
		if !validPiSkillName(name) {
			return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("invalid Pi skill")
		}
		var source struct {
			Path    string  `json:"path"`
			Source  string  `json:"source"`
			Scope   string  `json:"scope"`
			Origin  string  `json:"origin"`
			BaseDir *string `json:"baseDir"`
		}
		if len(command.SourceInfo) == 0 || decodeStrict(command.SourceInfo, &source) != nil || source.Origin != "top-level" || !filepath.IsAbs(source.Path) || filepath.Clean(source.Path) != source.Path {
			return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("invalid Pi skill provenance")
		}
		scope := provider.SkillScope("")
		switch {
		case source.Source == "local" && source.Scope == "user" && validOptionalSkillBase(source.BaseDir, false):
			scope = provider.SkillScopeUser
		case source.Source == "local" && source.Scope == "project" && validOptionalSkillBase(source.BaseDir, false):
			scope = provider.SkillScopeRepo
		case source.Source == "local" && source.Scope == "temporary" && validOptionalSkillBase(source.BaseDir, false):
			scope = provider.SkillScopeSystem
		case source.Source == "cli" && source.Scope == "temporary" && validOptionalSkillBase(source.BaseDir, true):
			scope = provider.SkillScopeSystem
		default:
			return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("invalid Pi skill provenance")
		}
		if _, duplicate := seenNames[name]; duplicate {
			return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("duplicate Pi skill")
		}
		seenNames[name] = struct{}{}
		digest := sha256.Sum256([]byte(string(scope) + "\x00" + name + "\x00" + source.Path))
		id := hex.EncodeToString(digest[:16])
		skill := piSkill{id: id, name: name, scope: scope}
		private.byID[id] = skill
		private.order = append(private.order, id)
		safe.Skills = append(safe.Skills, provider.SkillDescriptor{ID: id, Name: name, DisplayName: name, Description: command.Description, Scope: scope})
	}
	if safe.Validate() != nil {
		return piSkillCatalog{}, provider.SkillCatalog{}, errors.New("invalid Pi skill catalog")
	}
	return private, safe, nil
}

func validOptionalSkillBase(baseDir *string, optional bool) bool {
	if baseDir == nil {
		return optional
	}
	return filepath.IsAbs(*baseDir) && filepath.Clean(*baseDir) == *baseDir
}

func validPiSkillName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	previousHyphen := false
	for _, character := range []byte(name) {
		if character == '-' {
			if previousHyphen {
				return false
			}
			previousHyphen = true
			continue
		}
		previousHyphen = false
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil {
		return errors.New("invalid strict Pi record")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid strict Pi record")
	}
	return nil
}

func (s *Session) refreshSkills(ctx context.Context) (provider.SkillCatalog, error) {
	response, _, err := s.rpc.call(ctx, "get_commands", nil)
	if err != nil || requireSuccessfulResponse(response, "get_commands") != nil {
		return unavailablePiSkills(), provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	private, safe, err := parsePiSkills(response.Data)
	if err != nil {
		return unavailablePiSkills(), provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	s.mu.Lock()
	s.skills = private
	s.skillsCatalog = safe.Clone()
	s.mu.Unlock()
	return safe, nil
}

func (s *Session) Skills(ctx context.Context) provider.SkillCatalog {
	catalog, err := s.refreshSkills(ctx)
	if err != nil {
		return unavailablePiSkills()
	}
	return catalog.Clone()
}

func (s *Session) selectedSkill(content provider.MessageContent) (piSkill, bool, error) {
	var invocation *provider.SkillInvocation
	for _, part := range content.Parts {
		if part.Kind == provider.MessagePartSkill {
			if invocation != nil || part.Skill == nil {
				return piSkill{}, false, provider.NewProviderError(provider.ErrorSkillUnavailable)
			}
			invocation = part.Skill
		}
	}
	if invocation == nil {
		return piSkill{}, false, nil
	}
	s.mu.Lock()
	skill, ok := s.skills.byID[invocation.ID]
	s.mu.Unlock()
	if !ok || skill.name != invocation.Name {
		return piSkill{}, false, provider.NewProviderError(provider.ErrorSkillUnavailable)
	}
	return skill, true, nil
}

func piSkillPrompt(skill piSkill, envelope []byte) string {
	return "/skill:" + skill.name + " " + string(envelope)
}
func validatePiSkillExpansion(text string, skill piSkill, envelope []byte) bool {
	if strings.HasPrefix(text, "/skill:") || !strings.HasSuffix(text, string(bytes.TrimSuffix(envelope, []byte{'\n'}))) {
		return false
	}
	normalized := strings.TrimSuffix(text, string(bytes.TrimSuffix(envelope, []byte{'\n'})))
	return strings.HasPrefix(normalized, "<skill name=\""+skill.name+"\" location=\"") && strings.HasSuffix(normalized, "</skill>\n\n")
}

func (*Session) BusyTurnPolicy() provider.BusyTurnPolicy { return provider.BusyTurnQueue }
