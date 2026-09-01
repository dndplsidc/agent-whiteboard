package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"unicode"
	"unicode/utf8"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

const (
	maxConfigOptions     = 64
	maxConfigIDBytes     = 256
	maxConfigNameBytes   = provider.MaxTitleBytes
	maxConfigDescBytes   = provider.MaxSummaryBytes
	maxConfigTypeBytes   = 64
	maxConfigValueBytes  = provider.MaxModelValueBytes
	maxConfigBytes       = provider.MaxCatalogBytes
	maxListSessionsPage  = 1000
	maxListPages         = 64
	maxListSessions      = 4096
	maxListCursorBytes   = 1024
	maxListSemanticBytes = 16 << 20
)

type runtime struct {
	child   provider.ManagedChild
	client  *acp.Client
	caps    capabilities
	session *Session
}

func (r *runtime) close(ctx context.Context) error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Shutdown(ctx)
}
func (r *runtime) defaultModel() string { return "Cursor default" }

type capabilities struct{ Load, List, Image bool }
type initializeResult struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession        bool `json:"loadSession"`
		PromptCapabilities struct {
			Image           bool `json:"image"`
			EmbeddedContext bool `json:"embeddedContext"`
		} `json:"promptCapabilities"`
		SessionCapabilities struct {
			List json.RawMessage `json:"list"`
		} `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
}

func parseCapabilities(v initializeResult) (capabilities, error) {
	list := v.AgentCapabilities.SessionCapabilities.List
	validList := len(list) >= 2 && list[0] == '{' && list[len(list)-1] == '}'
	c := capabilities{Load: v.AgentCapabilities.LoadSession, Image: v.AgentCapabilities.PromptCapabilities.Image, List: validList}
	if v.ProtocolVersion != 1 || !c.Load || !c.List {
		return capabilities{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	return c, nil
}

type configOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Category     string        `json:"category"`
	Type         string        `json:"type"`
	CurrentValue string        `json:"currentValue"`
	Options      []configValue `json:"options"`
}
type configValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
type openResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []configOption `json:"configOptions"`
}
type listItem struct {
	SessionID            string         `json:"sessionId"`
	ConfigOptions        []configOption `json:"configOptions"`
	configOptionsPresent bool
}

func (s *listItem) UnmarshalJSON(data []byte) error {
	var wire struct {
		SessionID     string          `json:"sessionId"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	s.SessionID = wire.SessionID
	s.configOptionsPresent = wire.ConfigOptions != nil
	if s.configOptionsPresent {
		if err := json.Unmarshal(wire.ConfigOptions, &s.ConfigOptions); err != nil {
			return err
		}
	}
	return nil
}

type listResult struct {
	Sessions          []listItem `json:"sessions"`
	NextCursor        string     `json:"nextCursor,omitempty"`
	nextCursorPresent bool
}

func (l *listResult) UnmarshalJSON(data []byte) error {
	var wire struct {
		Sessions   []listItem      `json:"sessions"`
		NextCursor json.RawMessage `json:"nextCursor"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	l.Sessions = wire.Sessions
	l.nextCursorPresent = wire.NextCursor != nil
	if l.nextCursorPresent {
		if err := json.Unmarshal(wire.NextCursor, &l.NextCursor); err != nil {
			return err
		}
	}
	return nil
}

func (l listResult) validate() error {
	if l.Sessions == nil || len(l.Sessions) > maxListSessionsPage ||
		(l.nextCursorPresent && (l.NextCursor == "" || !validListCursor(l.NextCursor))) ||
		(!l.nextCursorPresent && l.NextCursor != "") {
		return errors.New("invalid session list")
	}
	for _, s := range l.Sessions {
		if !bounded(s.SessionID, provider.MaxNativeReferenceBytes, true) {
			return errors.New("invalid session list")
		}
		if s.configOptionsPresent {
			if _, err := validateConfigOptions(s.ConfigOptions); err != nil {
				return errors.New("invalid session configuration")
			}
		}
	}
	return nil
}

func validListCursor(cursor string) bool {
	if cursor == "" || len(cursor) > maxListCursorBytes || !utf8.ValidString(cursor) {
		return false
	}
	for _, r := range cursor {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (l listResult) semanticBytes() int {
	total := len(l.NextCursor)
	for _, session := range l.Sessions {
		total += len(session.SessionID)
		for _, option := range session.ConfigOptions {
			total += len(option.ID) + len(option.Name) + len(option.Description) + len(option.Category) + len(option.Type) + len(option.CurrentValue)
			for _, value := range option.Options {
				total += len(value.Value) + len(value.Name) + len(value.Description)
			}
		}
	}
	return total
}

func validateConfigOptions(options []configOption) (int, error) {
	if len(options) == 0 || len(options) > maxConfigOptions {
		return -1, errors.New("missing configuration")
	}
	seenIDs := make(map[string]struct{}, len(options))
	model := -1
	total := 0
	for i, option := range options {
		if !bounded(option.ID, maxConfigIDBytes, true) || !bounded(option.Name, maxConfigNameBytes, true) ||
			!bounded(option.Description, maxConfigDescBytes, false) || !bounded(option.Category, maxConfigTypeBytes, true) ||
			!bounded(option.Type, maxConfigTypeBytes, true) || !bounded(option.CurrentValue, maxConfigValueBytes, true) ||
			len(option.Options) == 0 || len(option.Options) > provider.MaxCatalogModels {
			return -1, errors.New("invalid configuration")
		}
		if _, duplicate := seenIDs[option.ID]; duplicate {
			return -1, errors.New("duplicate configuration")
		}
		seenIDs[option.ID] = struct{}{}
		values := make(map[string]struct{}, len(option.Options))
		current := false
		for _, value := range option.Options {
			if !bounded(value.Value, maxConfigValueBytes, true) || !bounded(value.Name, maxConfigNameBytes, true) || !bounded(value.Description, maxConfigDescBytes, false) {
				return -1, errors.New("invalid configuration value")
			}
			if _, duplicate := values[value.Value]; duplicate {
				return -1, errors.New("duplicate configuration value")
			}
			values[value.Value] = struct{}{}
			current = current || value.Value == option.CurrentValue
			total += len(value.Value) + len(value.Name) + len(value.Description)
		}
		if !current {
			return -1, errors.New("unknown current configuration")
		}
		total += len(option.ID) + len(option.Name) + len(option.Description) + len(option.Category) + len(option.Type) + len(option.CurrentValue)
		if total > maxConfigBytes {
			return -1, errors.New("oversized configuration")
		}
		isModel := option.Category == "model" || option.ID == "model"
		if isModel {
			if model >= 0 || option.Type != "select" {
				return -1, errors.New("invalid model configuration")
			}
			model = i
		}
	}
	if model < 0 {
		return -1, errors.New("missing model configuration")
	}
	return model, nil
}

func modelOption(options []configOption) (configOption, error) {
	index, err := validateConfigOptions(options)
	if err != nil {
		return configOption{}, err
	}
	return options[index], nil
}

func catalogFromOptions(options []configOption, images bool) (provider.ModelCatalog, provider.ExecutionSettings, provider.ModelPresentation, error) {
	option, err := modelOption(options)
	if err != nil {
		return provider.ModelCatalog{}, provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	catalog := provider.ModelCatalog{Models: make([]provider.CatalogModel, 0, len(option.Options))}
	for _, v := range option.Options {
		catalog.Models = append(catalog.Models, provider.CatalogModel{Model: v.Value, DisplayName: v.Name, Description: v.Description, DefaultEffort: "default", SupportedReasoningEfforts: []provider.ReasoningEffort{{Value: "default"}}, SupportsImages: images, Default: v.Value == option.CurrentValue})
	}
	if catalog.Validate() != nil {
		return provider.ModelCatalog{}, provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	settings := provider.ExecutionSettings{Model: option.CurrentValue, Effort: "default", Speed: provider.SpeedStandard}
	model, err := catalog.Resolve(settings)
	if err != nil {
		return provider.ModelCatalog{}, provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	presentation := provider.ModelPresentation{ModelDisplayName: model.DisplayName, Selectable: true}
	return catalog, settings, presentation, nil
}

func bounded(v string, max int, nonempty bool) bool {
	if len(v) > max || (nonempty && v == "") || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}
