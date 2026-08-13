package broker

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
)

type currentSettingsStore interface {
	UpdateCurrentSettings(statepkg.Identity, string, provider.NativeSessionRef, provider.ExecutionSettings, provider.ModelPresentation, time.Time) (statepkg.CommitOutcome, error)
}

func protocolSettings(settings *provider.ExecutionSettings) *protocol.ExecutionSettings {
	if settings == nil {
		return nil
	}
	return &protocol.ExecutionSettings{Model: settings.Model, Effort: settings.Effort, Speed: protocol.ExecutionSpeed(settings.Speed)}
}

func providerSettings(settings *protocol.ExecutionSettings) *provider.ExecutionSettings {
	if settings == nil {
		return nil
	}
	return &provider.ExecutionSettings{Model: settings.Model, Effort: settings.Effort, Speed: provider.ExecutionSpeed(settings.Speed)}
}

func presentedProtocolSettings(settings *provider.ExecutionSettings, presentation *provider.ModelPresentation) *protocol.PresentedExecutionSettings {
	if settings == nil || presentation == nil {
		return nil
	}
	return &protocol.PresentedExecutionSettings{
		ExecutionSettings: protocol.ExecutionSettings{Model: settings.Model, Effort: settings.Effort, Speed: protocol.ExecutionSpeed(settings.Speed)},
		ModelDisplayName:  presentation.ModelDisplayName,
		Selectable:        presentation.Selectable,
	}
}

func protocolCatalog(catalog provider.ModelCatalog) ([]protocol.CatalogModel, error) {
	if len(catalog.Models) == 0 {
		return []protocol.CatalogModel{}, nil
	}
	if catalog.Validate() != nil {
		return nil, errors.New("invalid provider model catalog")
	}
	result := make([]protocol.CatalogModel, len(catalog.Models))
	for index, model := range catalog.Models {
		efforts := make([]protocol.ReasoningEffortOption, len(model.SupportedReasoningEfforts))
		for effortIndex, effort := range model.SupportedReasoningEfforts {
			efforts[effortIndex] = protocol.ReasoningEffortOption{Effort: effort.Value, Description: effort.Description}
		}
		result[index] = protocol.CatalogModel{
			Model: model.Model, ModelDisplayName: model.DisplayName, Description: model.Description,
			DefaultEffort: model.DefaultEffort, SupportedReasoningEfforts: efforts,
			SupportsImages: model.SupportsImages, Default: model.Default, SupportsFast: model.SupportsFast,
		}
	}
	return result, nil
}

func loadModelCatalog(ctx context.Context, name provider.Name, driver provider.Driver) (provider.ModelCatalog, error) {
	if name == provider.NamePi {
		return provider.ModelCatalog{}, nil
	}
	selectable, ok := driver.(provider.SelectableDriver)
	if !ok {
		return provider.ModelCatalog{}, errors.New("selectable provider lacks model catalog")
	}
	catalog, err := selectable.ModelCatalog(ctx)
	if err != nil || catalog.Validate() != nil {
		return provider.ModelCatalog{}, errors.New("model catalog unavailable")
	}
	return catalog.Clone(), nil
}

func compatibleInitialSettings(catalog provider.ModelCatalog, settings *protocol.ExecutionSettings) *provider.ExecutionSettings {
	converted := providerSettings(settings)
	if converted == nil || !catalog.Compatibility(*converted).Compatible {
		return nil
	}
	return converted
}

func validateCommandSettings(name provider.Name, catalog provider.ModelCatalog, settings *protocol.ExecutionSettings) (*provider.ExecutionSettings, protocol.BrowserErrorCode) {
	if name == provider.NamePi {
		if settings != nil {
			return nil, protocol.ErrorInvalidCommand
		}
		return nil, ""
	}
	converted := providerSettings(settings)
	if converted == nil || !catalog.Compatibility(*converted).Compatible {
		return nil, protocol.ErrorInvalidModelConfiguration
	}
	return converted, ""
}

func stateSessionFromNative(conversationID string, native provider.NativeSession, at time.Time) (statepkg.Session, error) {
	ref, err := statepkg.NativeSessionRef(native.Ref.Value())
	if err != nil || ref != native.Ref || native.Validate() != nil {
		return statepkg.Session{}, errors.New("invalid provider native session")
	}
	result := statepkg.Session{
		ConversationID: conversationID, NativeSession: ref, CreatedAt: at, UpdatedAt: at,
		ProviderLabel: string(native.Provider), ModelLabel: native.Model,
	}
	if native.Settings != nil {
		settings := *native.Settings
		presentation := *native.Presentation
		result.Settings = &settings
		result.Presentation = &presentation
		result.ModelLabel = presentation.ModelDisplayName
	}
	if result.Validate() != nil {
		return statepkg.Session{}, errors.New("invalid durable provider session")
	}
	return result, nil
}

func (actor *conversation) browserPresentation(settings *provider.ExecutionSettings, presentation *provider.ModelPresentation) *provider.ModelPresentation {
	if settings == nil || presentation == nil {
		return nil
	}
	result := *presentation
	result.Selectable = false
	if model, err := actor.domainCatalog.Resolve(*settings); err == nil && model.DisplayName == presentation.ModelDisplayName {
		result.Selectable = true
	}
	return &result
}

func (actor *conversation) presentedEffectiveSettings(settings *provider.ExecutionSettings, presentation *provider.ModelPresentation) *protocol.PresentedExecutionSettings {
	return presentedProtocolSettings(settings, actor.browserPresentation(settings, presentation))
}

func (actor *conversation) settingsSnapshot() (*protocol.SettingsState, *protocol.PresentedExecutionSettings, []protocol.CatalogModel) {
	catalog := append([]protocol.CatalogModel{}, actor.catalog...)
	if actor.identity.Provider == provider.NamePi {
		return nil, nil, catalog
	}
	state := actor.settingsState
	if state == "" {
		state = protocol.SettingsUnverified
	}
	if state != protocol.SettingsVerified {
		return &state, nil, catalog
	}
	return &state, actor.presentedEffectiveSettings(actor.effectiveSettings, actor.effectivePresentation), catalog
}

func (actor *conversation) persistEffectiveSettings(settings provider.ExecutionSettings, presentation provider.ModelPresentation) bool {
	store, ok := actor.state.(currentSettingsStore)
	if !ok || actor.mapping.Current == nil {
		return false
	}
	before := cloneMapping(actor.mapping)
	at := actor.clock.Now().UTC()
	target := cloneMapping(before)
	copySettings := settings
	copyPresentation := presentation
	target.Current.Settings = &copySettings
	target.Current.Presentation = &copyPresentation
	target.Current.ModelLabel = presentation.ModelDisplayName
	target.Current.UpdatedAt = maxTime(target.Current.UpdatedAt, at)
	target.UpdatedAt = maxTime(target.UpdatedAt, at)
	outcome, mutationErr := store.UpdateCurrentSettings(actor.identity, target.Current.ConversationID, target.Current.NativeSession, settings, presentation, at)
	if outcome == statepkg.CommitApplied && mutationErr == nil {
		actor.mapping = target
		return true
	}
	loaded, err := actor.state.Load(actor.identity)
	if err != nil || loaded.Validate(actor.identity) != nil || !reflect.DeepEqual(loaded, target) || !knownCommitOutcome(outcome) {
		return false
	}
	actor.mapping = loaded
	return true
}

func (actor *conversation) refreshCatalog(attachments map[*clientAttachment]struct{}) bool {
	catalog, err := loadModelCatalog(actor.lifecycleCtx, actor.identity.Provider, actor.driver)
	if err != nil {
		actor.settingsState = protocol.SettingsUnverified
		actor.dispatchBlocked = true
		actor.publishShared(attachments, protocol.SettingsPayload{SettingsState: protocol.SettingsUnverified, Catalog: append([]protocol.CatalogModel{}, actor.catalog...)})
		return false
	}
	wire, err := protocolCatalog(catalog)
	if err != nil {
		return false
	}
	actor.domainCatalog = catalog
	actor.catalog = wire
	if actor.settingsState == protocol.SettingsVerified && actor.effectiveSettings != nil && actor.effectivePresentation != nil {
		actor.publishShared(attachments, protocol.SettingsPayload{
			SettingsState: protocol.SettingsVerified, EffectiveSettings: actor.presentedEffectiveSettings(actor.effectiveSettings, actor.effectivePresentation),
			Catalog: append([]protocol.CatalogModel{}, actor.catalog...),
		})
		return true
	}
	actor.publishShared(attachments, protocol.SettingsPayload{SettingsState: protocol.SettingsUnverified, Catalog: append([]protocol.CatalogModel{}, actor.catalog...)})
	return true
}

func (actor *conversation) publishSettingsEvent(attachments map[*clientAttachment]struct{}, source provider.Event) {
	if source.SettingsState == provider.SettingsUnverified {
		actor.settingsState = protocol.SettingsUnverified
		actor.dispatchBlocked = true
		actor.publishShared(attachments, protocol.SettingsPayload{SettingsState: protocol.SettingsUnverified, Catalog: append([]protocol.CatalogModel{}, actor.catalog...)})
		return
	}
	if source.Settings == nil || source.Presentation == nil || !actor.persistEffectiveSettings(*source.Settings, *source.Presentation) {
		actor.dispatchBlocked = true
		actor.lifecycle = protocol.LifecycleUnavailable
		actor.publishBrowserError(attachments, protocol.ErrorStateRepairFailed)
		actor.publishShared(attachments, actor.lifecyclePayload())
		return
	}
	actor.applyEffectiveSettings(*source.Settings, *source.Presentation)
	actor.dispatchBlocked = false
	actor.publishShared(attachments, protocol.SettingsPayload{
		SettingsState: protocol.SettingsVerified, EffectiveSettings: actor.presentedEffectiveSettings(source.Settings, source.Presentation),
		Catalog: append([]protocol.CatalogModel{}, actor.catalog...),
	})
}

func (actor *conversation) modelPresentation(settings provider.ExecutionSettings) (provider.ModelPresentation, bool) {
	model, err := actor.domainCatalog.Resolve(settings)
	if err != nil {
		return provider.ModelPresentation{}, false
	}
	return provider.ModelPresentation{ModelDisplayName: model.DisplayName, Selectable: true}, true
}

func (actor *conversation) draftSupportsImages(settings *provider.ExecutionSettings) bool {
	if settings == nil {
		return actor.session.capabilities.Images
	}
	model, err := actor.domainCatalog.Resolve(*settings)
	return err == nil && model.SupportsImages
}

func (actor *conversation) applyEffectiveSettings(settings provider.ExecutionSettings, presentation provider.ModelPresentation) {
	copySettings := settings
	copyPresentation := presentation
	actor.effectiveSettings = &copySettings
	actor.effectivePresentation = &copyPresentation
	actor.settingsState = protocol.SettingsVerified
	actor.session.capabilities.Images = false
	for _, model := range actor.catalog {
		if model.Model == settings.Model {
			actor.session.capabilities.Images = model.SupportsImages
			break
		}
	}
}
