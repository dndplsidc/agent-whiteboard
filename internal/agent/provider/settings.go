package provider

import (
	"context"
	"errors"
)

const (
	MaxCatalogModels          = 64
	MaxReasoningEfforts       = 16
	MaxModelValueBytes        = 256
	MaxEffortValueBytes       = 64
	MaxModelDescriptionBytes  = 8 << 10
	MaxEffortDescriptionBytes = 2 << 10
	MaxCatalogBytes           = 128 << 10
)

type ExecutionSpeed string

const (
	SpeedStandard ExecutionSpeed = "standard"
	SpeedFast     ExecutionSpeed = "fast"
)

func (speed ExecutionSpeed) Valid() bool {
	return speed == SpeedStandard || speed == SpeedFast
}

// ExecutionSettings is the complete provider-neutral execution tuple. Native
// service-tier names remain adapter-owned and never appear here.
type ExecutionSettings struct {
	Model  string
	Effort string
	Speed  ExecutionSpeed
}

func (settings ExecutionSettings) Validate() error {
	if !validBoundedText(settings.Model, MaxModelValueBytes, true) ||
		!validBoundedText(settings.Effort, MaxEffortValueBytes, true) ||
		!settings.Speed.Valid() {
		return errors.New("invalid execution settings")
	}
	return nil
}

// ModelPresentation is bounded display metadata resolved by the provider.
type ModelPresentation struct {
	ModelDisplayName string
	Selectable       bool
}

func (presentation ModelPresentation) Validate() error {
	if !validBoundedText(presentation.ModelDisplayName, MaxTitleBytes, true) {
		return errors.New("invalid model presentation")
	}
	return nil
}

type ReasoningEffort struct {
	Value       string
	Description string
}

func (effort ReasoningEffort) validate() error {
	if !validBoundedText(effort.Value, MaxEffortValueBytes, true) ||
		!validBoundedText(effort.Description, MaxEffortDescriptionBytes, false) {
		return errors.New("invalid reasoning effort")
	}
	return nil
}

type CatalogModel struct {
	Model                     string
	DisplayName               string
	Description               string
	DefaultEffort             string
	SupportedReasoningEfforts []ReasoningEffort
	SupportsImages            bool
	Default                   bool
	SupportsFast              bool
}

func (model CatalogModel) validate() error {
	if !validBoundedText(model.Model, MaxModelValueBytes, true) ||
		!validBoundedText(model.DisplayName, MaxTitleBytes, true) ||
		!validBoundedText(model.Description, MaxModelDescriptionBytes, false) ||
		!validBoundedText(model.DefaultEffort, MaxEffortValueBytes, true) ||
		len(model.SupportedReasoningEfforts) == 0 || len(model.SupportedReasoningEfforts) > MaxReasoningEfforts {
		return errors.New("invalid catalog model")
	}
	seen := make(map[string]struct{}, len(model.SupportedReasoningEfforts))
	defaultFound := false
	for _, effort := range model.SupportedReasoningEfforts {
		if effort.validate() != nil {
			return errors.New("invalid catalog model effort")
		}
		if _, duplicate := seen[effort.Value]; duplicate {
			return errors.New("duplicate catalog effort")
		}
		seen[effort.Value] = struct{}{}
		defaultFound = defaultFound || effort.Value == model.DefaultEffort
	}
	if !defaultFound {
		return errors.New("catalog default effort is unsupported")
	}
	return nil
}

func (model CatalogModel) clone() CatalogModel {
	result := model
	result.SupportedReasoningEfforts = append([]ReasoningEffort{}, model.SupportedReasoningEfforts...)
	return result
}

type ModelCatalog struct {
	Models []CatalogModel
}

func (catalog ModelCatalog) Validate() error {
	if len(catalog.Models) == 0 || len(catalog.Models) > MaxCatalogModels {
		return errors.New("invalid model catalog size")
	}
	seen := make(map[string]struct{}, len(catalog.Models))
	defaults := 0
	total := 0
	for _, model := range catalog.Models {
		if model.validate() != nil {
			return errors.New("invalid model catalog entry")
		}
		if _, duplicate := seen[model.Model]; duplicate {
			return errors.New("duplicate catalog model")
		}
		seen[model.Model] = struct{}{}
		if model.Default {
			defaults++
		}
		total += len(model.Model) + len(model.DisplayName) + len(model.Description) + len(model.DefaultEffort)
		for _, effort := range model.SupportedReasoningEfforts {
			total += len(effort.Value) + len(effort.Description)
		}
		if total > MaxCatalogBytes {
			return errors.New("model catalog exceeds byte limit")
		}
	}
	if defaults != 1 {
		return errors.New("model catalog requires one default")
	}
	return nil
}

func (catalog ModelCatalog) Clone() ModelCatalog {
	result := ModelCatalog{Models: make([]CatalogModel, len(catalog.Models))}
	for index, model := range catalog.Models {
		result.Models[index] = model.clone()
	}
	return result
}

type SettingsIncompatibility string

const (
	IncompatibleCatalogInvalid    SettingsIncompatibility = "catalog_invalid"
	IncompatibleModelUnavailable  SettingsIncompatibility = "model_unavailable"
	IncompatibleEffortUnsupported SettingsIncompatibility = "effort_unsupported"
	IncompatibleFastUnsupported   SettingsIncompatibility = "fast_unsupported"
)

type SettingsCompatibility struct {
	Compatible bool
	Reason     SettingsIncompatibility
}

func (catalog ModelCatalog) Compatibility(settings ExecutionSettings) SettingsCompatibility {
	if catalog.Validate() != nil || settings.Validate() != nil {
		return SettingsCompatibility{Reason: IncompatibleCatalogInvalid}
	}
	for _, model := range catalog.Models {
		if model.Model != settings.Model {
			continue
		}
		effortSupported := false
		for _, effort := range model.SupportedReasoningEfforts {
			effortSupported = effortSupported || effort.Value == settings.Effort
		}
		if !effortSupported {
			return SettingsCompatibility{Reason: IncompatibleEffortUnsupported}
		}
		if settings.Speed == SpeedFast && !model.SupportsFast {
			return SettingsCompatibility{Reason: IncompatibleFastUnsupported}
		}
		return SettingsCompatibility{Compatible: true}
	}
	return SettingsCompatibility{Reason: IncompatibleModelUnavailable}
}

func (catalog ModelCatalog) Resolve(settings ExecutionSettings) (CatalogModel, error) {
	if !catalog.Compatibility(settings).Compatible {
		return CatalogModel{}, errors.New("incompatible execution settings")
	}
	for _, model := range catalog.Models {
		if model.Model == settings.Model {
			return model.clone(), nil
		}
	}
	return CatalogModel{}, errors.New("model unavailable")
}

// Canonicalize verifies that every tuple value is an exact advertised value.
func (catalog ModelCatalog) Canonicalize(settings ExecutionSettings) (ExecutionSettings, error) {
	if _, err := catalog.Resolve(settings); err != nil {
		return ExecutionSettings{}, err
	}
	return settings, nil
}

// SettingsSession is implemented by active sessions that expose a safe,
// runtime-selectable execution catalog. ApplySettings returns the authoritative
// effective tuple and its bounded presentation after native application.
type SettingsSession interface {
	SettingsCatalog(context.Context) (ModelCatalog, error)
	EffectiveSettings(context.Context) (ExecutionSettings, ModelPresentation, error)
	ApplySettings(context.Context, ExecutionSettings) (ExecutionSettings, ModelPresentation, error)
}
