package protocol

import (
	"bytes"
	"encoding/json"
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

type ExecutionSettings struct {
	Model  string         `json:"model"`
	Effort string         `json:"effort"`
	Speed  ExecutionSpeed `json:"speed"`
}

func (settings ExecutionSettings) Validate() error {
	if !validBoundedText(settings.Model, MaxModelValueBytes, true) ||
		!validBoundedText(settings.Effort, MaxEffortValueBytes, true) ||
		!settings.Speed.Valid() {
		return invalid(nil)
	}
	return nil
}

func (settings *ExecutionSettings) UnmarshalJSON(data []byte) error {
	if err := inspectJSON(data, map[string]bool{}); err != nil {
		return err
	}
	if err := requireFields(data, "model", "effort", "speed"); err != nil {
		return err
	}
	type wire ExecutionSettings
	var decoded wire
	if err := strictDecode(data, &decoded); err != nil {
		return err
	}
	*settings = ExecutionSettings(decoded)
	return nil
}

type PresentedExecutionSettings struct {
	ExecutionSettings
	ModelDisplayName string `json:"model_display_name"`
	Selectable       bool   `json:"selectable"`
}

func (settings PresentedExecutionSettings) Validate() error {
	if settings.ExecutionSettings.Validate() != nil || !validBoundedText(settings.ModelDisplayName, MaxTitleBytes, true) {
		return invalid(nil)
	}
	return nil
}

func (settings *PresentedExecutionSettings) UnmarshalJSON(data []byte) error {
	if err := inspectJSON(data, map[string]bool{}); err != nil {
		return err
	}
	if err := requireFields(data, "model", "effort", "speed", "model_display_name", "selectable"); err != nil {
		return err
	}
	var decoded struct {
		Model            string         `json:"model"`
		Effort           string         `json:"effort"`
		Speed            ExecutionSpeed `json:"speed"`
		ModelDisplayName string         `json:"model_display_name"`
		Selectable       bool           `json:"selectable"`
	}
	if err := strictDecode(data, &decoded); err != nil {
		return err
	}
	*settings = PresentedExecutionSettings{
		ExecutionSettings: ExecutionSettings{Model: decoded.Model, Effort: decoded.Effort, Speed: decoded.Speed},
		ModelDisplayName:  decoded.ModelDisplayName,
		Selectable:        decoded.Selectable,
	}
	return nil
}

type ReasoningEffortOption struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

func (option ReasoningEffortOption) validate() error {
	if !validBoundedText(option.Effort, MaxEffortValueBytes, true) || !validBoundedText(option.Description, MaxEffortDescriptionBytes, false) {
		return invalid(nil)
	}
	return nil
}

func (option *ReasoningEffortOption) UnmarshalJSON(data []byte) error {
	if err := inspectJSON(data, map[string]bool{}); err != nil {
		return err
	}
	if err := requireFields(data, "effort", "description"); err != nil {
		return err
	}
	type wire ReasoningEffortOption
	var decoded wire
	if err := strictDecode(data, &decoded); err != nil {
		return err
	}
	*option = ReasoningEffortOption(decoded)
	return nil
}

type CatalogModel struct {
	Model                     string                  `json:"model"`
	ModelDisplayName          string                  `json:"model_display_name"`
	Description               string                  `json:"description"`
	DefaultEffort             string                  `json:"default_effort"`
	SupportedReasoningEfforts []ReasoningEffortOption `json:"supported_reasoning_efforts"`
	SupportsImages            bool                    `json:"supports_images"`
	Default                   bool                    `json:"default"`
	SupportsFast              bool                    `json:"supports_fast"`
}

func (model CatalogModel) validate() error {
	if !validBoundedText(model.Model, MaxModelValueBytes, true) ||
		!validBoundedText(model.ModelDisplayName, MaxTitleBytes, true) ||
		!validBoundedText(model.Description, MaxModelDescriptionBytes, false) ||
		!validBoundedText(model.DefaultEffort, MaxEffortValueBytes, true) ||
		len(model.SupportedReasoningEfforts) == 0 || len(model.SupportedReasoningEfforts) > MaxReasoningEfforts {
		return invalid(nil)
	}
	seen := make(map[string]struct{}, len(model.SupportedReasoningEfforts))
	defaultFound := false
	for _, effort := range model.SupportedReasoningEfforts {
		if effort.validate() != nil {
			return invalid(nil)
		}
		if _, duplicate := seen[effort.Effort]; duplicate {
			return invalid(nil)
		}
		seen[effort.Effort] = struct{}{}
		defaultFound = defaultFound || effort.Effort == model.DefaultEffort
	}
	if !defaultFound {
		return invalid(nil)
	}
	return nil
}

func (model *CatalogModel) UnmarshalJSON(data []byte) error {
	if err := inspectJSON(data, map[string]bool{}); err != nil {
		return err
	}
	if err := requireFields(data, "model", "model_display_name", "description", "default_effort", "supported_reasoning_efforts", "supports_images", "default", "supports_fast"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || bytes.Equal(fields["supported_reasoning_efforts"], []byte("null")) {
		return errors.New("invalid catalog efforts")
	}
	type wire CatalogModel
	var decoded wire
	if err := strictDecode(data, &decoded); err != nil {
		return err
	}
	*model = CatalogModel(decoded)
	return nil
}

func validateCatalog(catalog []CatalogModel) error {
	if catalog == nil || len(catalog) > MaxCatalogModels {
		return invalid(nil)
	}
	seen := make(map[string]struct{}, len(catalog))
	defaults := 0
	total := 0
	for _, model := range catalog {
		if model.validate() != nil {
			return invalid(nil)
		}
		if _, duplicate := seen[model.Model]; duplicate {
			return invalid(nil)
		}
		seen[model.Model] = struct{}{}
		if model.Default {
			defaults++
		}
		total += len(model.Model) + len(model.ModelDisplayName) + len(model.Description) + len(model.DefaultEffort)
		for _, effort := range model.SupportedReasoningEfforts {
			total += len(effort.Effort) + len(effort.Description)
		}
		if total > MaxCatalogBytes {
			return ErrMessageTooLarge
		}
	}
	if len(catalog) > 0 && defaults != 1 {
		return invalid(nil)
	}
	return nil
}

type SettingsState string

const (
	SettingsVerified   SettingsState = "verified"
	SettingsUnverified SettingsState = "unverified"
)

func (state SettingsState) Valid() bool {
	return state == SettingsVerified || state == SettingsUnverified
}

func validateSettingsSnapshot(state *SettingsState, settings *PresentedExecutionSettings, catalog []CatalogModel) error {
	if validateCatalog(catalog) != nil {
		return invalid(nil)
	}
	if state == nil {
		if settings != nil || len(catalog) != 0 {
			return invalid(nil)
		}
		return nil
	}
	if !state.Valid() {
		return invalid(nil)
	}
	if *state == SettingsVerified {
		if settings == nil || settings.Validate() != nil || validateSelectableSettings(*settings, catalog) != nil {
			return invalid(nil)
		}
		return nil
	}
	if settings != nil {
		return invalid(nil)
	}
	return nil
}

func validateSelectableSettings(settings PresentedExecutionSettings, catalog []CatalogModel) error {
	if !settings.Selectable {
		return nil
	}
	for _, model := range catalog {
		if model.Model != settings.Model {
			continue
		}
		effortSupported := false
		for _, effort := range model.SupportedReasoningEfforts {
			effortSupported = effortSupported || effort.Effort == settings.Effort
		}
		if !effortSupported || settings.Speed == SpeedFast && !model.SupportsFast || settings.ModelDisplayName != model.ModelDisplayName {
			return invalid(nil)
		}
		return nil
	}
	return invalid(nil)
}

func selectableModelCapabilities(settings PresentedExecutionSettings, catalog []CatalogModel) (bool, bool) {
	if !settings.Selectable {
		return false, false
	}
	for _, model := range catalog {
		if model.Model == settings.Model {
			return model.SupportsImages, true
		}
	}
	return false, false
}

func presentedSettingsBytes(settings *PresentedExecutionSettings) int {
	if settings == nil {
		return 0
	}
	return len(settings.Model) + len(settings.Effort) + len(settings.Speed) + len(settings.ModelDisplayName)
}
