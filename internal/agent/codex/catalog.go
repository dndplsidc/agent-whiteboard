package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

const maxModelPages = 32

type nativeServiceTier struct {
	ID          string
	Name        string
	Description string
}

type nativeServiceTierWire struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

type nativeEffortOption struct {
	Effort      string  `json:"reasoningEffort"`
	Description *string `json:"description"`
}

type nativeModelRecord struct {
	ID            string
	Model         string
	DisplayName   string
	Description   string
	Hidden        bool
	Default       bool
	DefaultEffort string
	Efforts       []provider.ReasoningEffort
	Capabilities  provider.Capabilities
	SupportsFast  bool
	ServiceTiers  []nativeServiceTier
}

type nativeCatalog struct {
	models  map[string]nativeModelRecord
	aliases map[string]string
	visible provider.ModelCatalog
}

func parseModelCatalogPage(raw []byte) ([]nativeModelRecord, string, error) {
	if validateJSONStructure(raw) != nil {
		return nil, "", errors.New("invalid model/list response")
	}
	var page struct {
		Data []struct {
			ID                       string               `json:"id"`
			Model                    string               `json:"model"`
			DisplayName              string               `json:"displayName"`
			Description              *string              `json:"description"`
			Hidden                   *bool                `json:"hidden"`
			Default                  *bool                `json:"isDefault"`
			DefaultEffort            string               `json:"defaultReasoningEffort"`
			SupportedReasoningEffort []nativeEffortOption `json:"supportedReasoningEfforts"`
			InputModalities          json.RawMessage      `json:"inputModalities"`
			ServiceTiers             json.RawMessage      `json:"serviceTiers"`
		} `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&page) != nil || decoder.Decode(&struct{}{}) != io.EOF || page.Data == nil || len(page.Data) > provider.MaxCatalogModels {
		return nil, "", errors.New("invalid model/list response")
	}
	result := make([]nativeModelRecord, 0, len(page.Data))
	for _, wire := range page.Data {
		if wire.Hidden == nil || wire.Default == nil || wire.SupportedReasoningEffort == nil ||
			!validCatalogText(wire.ID, provider.MaxModelValueBytes, true) || !validCatalogText(wire.Model, provider.MaxModelValueBytes, true) ||
			wire.Description == nil || !validCatalogText(wire.DisplayName, provider.MaxTitleBytes, true) || !validCatalogText(*wire.Description, provider.MaxModelDescriptionBytes, false) ||
			!validCatalogText(wire.DefaultEffort, provider.MaxEffortValueBytes, true) || len(wire.SupportedReasoningEffort) == 0 || len(wire.SupportedReasoningEffort) > provider.MaxReasoningEfforts {
			return nil, "", errors.New("invalid native model record")
		}
		efforts := make([]provider.ReasoningEffort, len(wire.SupportedReasoningEffort))
		seenEfforts := make(map[string]struct{}, len(efforts))
		defaultFound := false
		for index, effort := range wire.SupportedReasoningEffort {
			if effort.Description == nil || !validCatalogText(effort.Effort, provider.MaxEffortValueBytes, true) || !validCatalogText(*effort.Description, provider.MaxEffortDescriptionBytes, false) {
				return nil, "", errors.New("invalid native reasoning effort")
			}
			if _, duplicate := seenEfforts[effort.Effort]; duplicate {
				return nil, "", errors.New("duplicate native reasoning effort")
			}
			seenEfforts[effort.Effort] = struct{}{}
			defaultFound = defaultFound || effort.Effort == wire.DefaultEffort
			efforts[index] = provider.ReasoningEffort{Value: effort.Effort, Description: *effort.Description}
		}
		if !defaultFound {
			return nil, "", errors.New("native default effort is unsupported")
		}
		capabilities, err := parseInputModalities(wire.InputModalities)
		if err != nil {
			return nil, "", err
		}
		tiers, supportsFast, err := parseServiceTiers(wire.ServiceTiers)
		if err != nil {
			return nil, "", err
		}
		model := provider.CatalogModel{
			Model: wire.Model, DisplayName: wire.DisplayName, Description: *wire.Description, DefaultEffort: wire.DefaultEffort,
			SupportedReasoningEfforts: efforts, SupportsImages: capabilities.Images, Default: true, SupportsFast: supportsFast,
		}
		if (provider.ModelCatalog{Models: []provider.CatalogModel{model}}).Validate() != nil {
			return nil, "", errors.New("invalid bounded native model")
		}
		result = append(result, nativeModelRecord{
			ID: wire.ID, Model: wire.Model, DisplayName: wire.DisplayName, Description: *wire.Description,
			Hidden: *wire.Hidden, Default: *wire.Default, DefaultEffort: wire.DefaultEffort, Efforts: efforts,
			Capabilities: capabilities, SupportsFast: supportsFast, ServiceTiers: tiers,
		})
	}
	cursor := ""
	if page.NextCursor != nil {
		cursor = *page.NextCursor
		if cursor == "" || !validCatalogText(cursor, provider.MaxNativeReferenceBytes, true) {
			return nil, "", errors.New("invalid model cursor")
		}
	}
	return result, cursor, nil
}

func parseInputModalities(raw json.RawMessage) (provider.Capabilities, error) {
	if len(raw) == 0 {
		return provider.Capabilities{Images: true}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return provider.Capabilities{}, errors.New("invalid model modalities")
	}
	var modalities []string
	if json.Unmarshal(raw, &modalities) != nil || modalities == nil {
		return provider.Capabilities{}, errors.New("invalid model modalities")
	}
	capabilities := provider.Capabilities{}
	seen := make(map[string]struct{}, len(modalities))
	for _, modality := range modalities {
		if _, duplicate := seen[modality]; duplicate {
			return provider.Capabilities{}, errors.New("duplicate model modality")
		}
		seen[modality] = struct{}{}
		switch modality {
		case "text", "audio":
		case "image":
			capabilities.Images = true
		default:
			return provider.Capabilities{}, errors.New("unknown model modality")
		}
	}
	return capabilities, nil
}

func parseServiceTiers(raw json.RawMessage) ([]nativeServiceTier, bool, error) {
	if len(raw) == 0 {
		return []nativeServiceTier{}, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false, errors.New("invalid model service tiers")
	}
	var wire []nativeServiceTierWire
	if json.Unmarshal(raw, &wire) != nil || wire == nil || len(wire) > provider.MaxReasoningEfforts {
		return nil, false, errors.New("invalid model service tiers")
	}
	tiers := make([]nativeServiceTier, 0, len(wire))
	seen := make(map[string]struct{}, len(wire))
	supportsFast := false
	for _, tier := range wire {
		if tier.Description == nil || !validCatalogText(tier.ID, provider.MaxEffortValueBytes, true) || !validCatalogText(tier.Name, provider.MaxTitleBytes, true) || !validCatalogText(*tier.Description, provider.MaxEffortDescriptionBytes, false) {
			return nil, false, errors.New("invalid model service tier")
		}
		if _, duplicate := seen[tier.ID]; duplicate {
			return nil, false, errors.New("duplicate model service tier")
		}
		seen[tier.ID] = struct{}{}
		supportsFast = supportsFast || tier.ID == "priority"
		tiers = append(tiers, nativeServiceTier{ID: tier.ID, Name: tier.Name, Description: *tier.Description})
	}
	return tiers, supportsFast, nil
}

func newNativeCatalog(records []nativeModelRecord) (nativeCatalog, error) {
	if len(records) == 0 || len(records) > provider.MaxCatalogModels {
		return nativeCatalog{}, errors.New("invalid native catalog size")
	}
	catalog := nativeCatalog{models: make(map[string]nativeModelRecord, len(records)), aliases: make(map[string]string, len(records)*2)}
	visible := make([]provider.CatalogModel, 0, len(records))
	total := 0
	for _, record := range records {
		if _, duplicate := catalog.models[record.Model]; duplicate {
			return nativeCatalog{}, errors.New("duplicate native model")
		}
		catalog.models[record.Model] = cloneNativeModel(record)
		for _, alias := range []string{record.ID, record.Model} {
			if existing, duplicate := catalog.aliases[alias]; duplicate && existing != record.Model {
				return nativeCatalog{}, errors.New("conflicting native model alias")
			}
			catalog.aliases[alias] = record.Model
		}
		total += len(record.ID) + len(record.Model) + len(record.DisplayName) + len(record.Description) + len(record.DefaultEffort)
		for _, effort := range record.Efforts {
			total += len(effort.Value) + len(effort.Description)
		}
		for _, tier := range record.ServiceTiers {
			total += len(tier.ID) + len(tier.Name) + len(tier.Description)
		}
		if total > provider.MaxCatalogBytes {
			return nativeCatalog{}, errors.New("native model catalog exceeds byte limit")
		}
		if !record.Hidden {
			visible = append(visible, provider.CatalogModel{
				Model: record.Model, DisplayName: record.DisplayName, Description: record.Description, DefaultEffort: record.DefaultEffort,
				SupportedReasoningEfforts: append([]provider.ReasoningEffort{}, record.Efforts...), SupportsImages: record.Capabilities.Images,
				Default: record.Default, SupportsFast: record.SupportsFast,
			})
		}
	}
	catalog.visible = provider.ModelCatalog{Models: visible}
	if catalog.visible.Validate() != nil {
		return nativeCatalog{}, errors.New("invalid visible native model catalog")
	}
	return catalog, nil
}

func cloneNativeModel(model nativeModelRecord) nativeModelRecord {
	model.Efforts = append([]provider.ReasoningEffort{}, model.Efforts...)
	model.ServiceTiers = append([]nativeServiceTier{}, model.ServiceTiers...)
	return model
}

func (catalog nativeCatalog) clone() nativeCatalog {
	result := nativeCatalog{models: make(map[string]nativeModelRecord, len(catalog.models)), aliases: make(map[string]string, len(catalog.aliases)), visible: catalog.visible.Clone()}
	for key, model := range catalog.models {
		result.models[key] = cloneNativeModel(model)
	}
	for key, model := range catalog.aliases {
		result.aliases[key] = model
	}
	return result
}

func (catalog nativeCatalog) visibleCatalog() provider.ModelCatalog { return catalog.visible.Clone() }

func (catalog nativeCatalog) resolveSubmitted(settings provider.ExecutionSettings) (provider.ExecutionSettings, string, provider.ModelPresentation, provider.Capabilities, error) {
	canonical, err := catalog.visible.Canonicalize(settings)
	if err != nil {
		return provider.ExecutionSettings{}, "", provider.ModelPresentation{}, provider.Capabilities{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	model := catalog.models[canonical.Model]
	presentation := provider.ModelPresentation{ModelDisplayName: model.DisplayName, Selectable: true}
	return canonical, model.Model, presentation, model.Capabilities, nil
}

func (catalog nativeCatalog) resolveEffective(nativeModel, effort, tier string) (provider.ExecutionSettings, provider.ModelPresentation, provider.Capabilities, error) {
	if canonical := catalog.aliases[nativeModel]; canonical != "" {
		nativeModel = canonical
	}
	speed, err := semanticSpeed(tier)
	if err != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.Capabilities{}, err
	}
	model, known := catalog.models[nativeModel]
	if known && effort == "" {
		effort = model.DefaultEffort
	}
	settings := provider.ExecutionSettings{Model: nativeModel, Effort: effort, Speed: speed}
	if settings.Validate() != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.Capabilities{}, errors.New("incomplete native effective settings")
	}
	if !known {
		presentation := provider.ModelPresentation{ModelDisplayName: nativeModel, Selectable: false}
		if presentation.Validate() != nil {
			return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.Capabilities{}, errors.New("invalid removed model presentation")
		}
		return settings, presentation, provider.Capabilities{}, nil
	}
	effortSupported := false
	for _, option := range model.Efforts {
		effortSupported = effortSupported || option.Value == settings.Effort
	}
	if !effortSupported || settings.Speed == provider.SpeedFast && !model.SupportsFast {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.Capabilities{}, errors.New("native effective settings conflict with catalog")
	}
	presentation := provider.ModelPresentation{ModelDisplayName: model.DisplayName, Selectable: !model.Hidden}
	return settings, presentation, model.Capabilities, nil
}

func semanticSpeed(tier string) (provider.ExecutionSpeed, error) {
	switch tier {
	case "", "default":
		return provider.SpeedStandard, nil
	case "priority":
		return provider.SpeedFast, nil
	default:
		return "", errors.New("unknown native service tier")
	}
}

func nativeServiceTierValue(speed provider.ExecutionSpeed) any {
	if speed == provider.SpeedFast {
		return "priority"
	}
	return nil
}

func loadModelCatalog(ctx context.Context, runtime *runtime) (nativeCatalog, error) {
	records := make([]nativeModelRecord, 0)
	cursor := ""
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxModelPages; page++ {
		params := map[string]any{"includeHidden": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := runtime.call(ctx, "model/list", params)
		if err != nil {
			return nativeCatalog{}, err
		}
		pageRecords, next, err := parseModelCatalogPage(raw)
		if err != nil || len(records)+len(pageRecords) > provider.MaxCatalogModels {
			return nativeCatalog{}, errors.New("invalid paged model catalog")
		}
		records = append(records, pageRecords...)
		if next == "" {
			return newNativeCatalog(records)
		}
		if _, duplicate := seenCursors[next]; duplicate {
			return nativeCatalog{}, errors.New("repeated model cursor")
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
	return nativeCatalog{}, errors.New("model catalog pagination limit exceeded")
}

func validCatalogText(value string, maxBytes int, nonempty bool) bool {
	if !utf8.ValidString(value) || len(value) > maxBytes || nonempty && value == "" {
		return false
	}
	for _, char := range value {
		if char < 0x20 && char != '\t' && char != '\n' && char != '\r' {
			return false
		}
	}
	return !strings.ContainsRune(value, '\x00')
}
