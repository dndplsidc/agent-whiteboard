package agentprotocol

import (
	"errors"
	"regexp"
)

const (
	MaxInteractionTextBytes = 64 << 10
	MaxInteractionOptions   = 16
	MaxInteractionQuestions = 3
	MaxInteractionAnswers   = 32
)

type ToolKind string

const (
	ToolCommand       ToolKind = "command"
	ToolFileChange    ToolKind = "file_change"
	ToolMCP           ToolKind = "mcp"
	ToolWeb           ToolKind = "web"
	ToolImage         ToolKind = "image"
	ToolCollaboration ToolKind = "collaboration"
	ToolPlan          ToolKind = "plan"
	ToolOther         ToolKind = "other"
)

type ToolStatus string

const (
	ToolRunning     ToolStatus = "running"
	ToolCompleted   ToolStatus = "completed"
	ToolFailed      ToolStatus = "failed"
	ToolInterrupted ToolStatus = "interrupted"
)

type InteractionKind string

const (
	InteractionCommandApproval    InteractionKind = "command_approval"
	InteractionFileApproval       InteractionKind = "file_change_approval"
	InteractionPermissionApproval InteractionKind = "permission_approval"
	InteractionUserInput          InteractionKind = "user_input"
	InteractionMCPElicitation     InteractionKind = "mcp_elicitation"
)

type InteractionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type InteractionQuestion struct {
	ID         string              `json:"id"`
	Header     string              `json:"header"`
	Prompt     string              `json:"prompt"`
	Options    []InteractionOption `json:"options"`
	AllowOther bool                `json:"allow_other"`
	Secret     bool                `json:"secret"`
	Multiple   bool                `json:"multiple"`
}

type InteractionFieldType string

const (
	InteractionText        InteractionFieldType = "text"
	InteractionNumber      InteractionFieldType = "number"
	InteractionBoolean     InteractionFieldType = "boolean"
	InteractionSelect      InteractionFieldType = "select"
	InteractionMultiSelect InteractionFieldType = "multi_select"
)

type InteractionField struct {
	ID          string               `json:"id"`
	Label       string               `json:"label"`
	Description string               `json:"description"`
	Type        InteractionFieldType `json:"type"`
	Required    bool                 `json:"required"`
	Secret      bool                 `json:"secret"`
	Options     []InteractionOption  `json:"options"`
}

type InteractionResponsePayload struct {
	RequestID string              `json:"request_id"`
	Kind      InteractionKind     `json:"kind"`
	OptionID  string              `json:"option_id"`
	Answers   map[string][]string `json:"answers"`
}

func (InteractionResponsePayload) commandPayload() {}

func (payload InteractionResponsePayload) validate() error {
	if !validID(payload.RequestID) || !validInteractionKind(payload.Kind) || (payload.OptionID != "" && !validInteractionKey(payload.OptionID)) || len(payload.Answers) > MaxInteractionAnswers {
		return invalid(nil)
	}
	if payload.OptionID == "" && len(payload.Answers) == 0 {
		return invalid(nil)
	}
	total := 0
	for key, answers := range payload.Answers {
		if !validInteractionKey(key) || len(answers) == 0 || len(answers) > MaxInteractionAnswers {
			return invalid(nil)
		}
		for _, answer := range answers {
			if !validBoundedText(answer, MaxInteractionTextBytes, true) {
				return invalid(nil)
			}
			total += len(answer)
			if total > MaxInteractionTextBytes {
				return ErrMessageTooLarge
			}
		}
	}
	return nil
}

type ToolActivityPayload struct {
	ActivityID string     `json:"activity_id"`
	TurnID     string     `json:"turn_id,omitempty"`
	Kind       ToolKind   `json:"kind"`
	Status     ToolStatus `json:"status"`
	Title      string     `json:"title"`
	Summary    string     `json:"summary"`
	Detail     string     `json:"detail"`
}

func (ToolActivityPayload) EventType() EventType { return EventToolActivity }
func (payload ToolActivityPayload) validate() error {
	if !validID(payload.ActivityID) || (payload.TurnID != "" && !validID(payload.TurnID)) || !validToolKind(payload.Kind) || !validToolStatus(payload.Status) ||
		!validBoundedText(payload.Title, MaxTitleBytes, true) || !validBoundedText(payload.Summary, MaxSummaryBytes, false) || !validBoundedText(payload.Detail, MaxInteractionTextBytes, false) {
		return invalid(nil)
	}
	return nil
}

type InteractionRequestPayload struct {
	RequestID        string                `json:"request_id"`
	TurnID           string                `json:"turn_id,omitempty"`
	Kind             InteractionKind       `json:"kind"`
	Title            string                `json:"title"`
	Summary          string                `json:"summary"`
	Command          string                `json:"command"`
	WorkingDirectory string                `json:"working_directory"`
	Options          []InteractionOption   `json:"options"`
	Questions        []InteractionQuestion `json:"questions"`
	Fields           []InteractionField    `json:"fields"`
}

func (InteractionRequestPayload) EventType() EventType { return EventInteractionRequest }
func (payload InteractionRequestPayload) validate() error {
	if !validID(payload.RequestID) || (payload.TurnID != "" && !validID(payload.TurnID)) || !validInteractionKind(payload.Kind) ||
		!validBoundedText(payload.Title, MaxTitleBytes, true) || !validBoundedText(payload.Summary, MaxSummaryBytes, false) ||
		!validBoundedText(payload.Command, MaxInteractionTextBytes, false) || !validBoundedText(payload.WorkingDirectory, MaxURLBytes, false) ||
		len(payload.Options) > MaxInteractionOptions || len(payload.Questions) > MaxInteractionQuestions || len(payload.Fields) > MaxInteractionAnswers || validateInteractionOptions(payload.Options) != nil {
		return invalid(nil)
	}
	seen := make(map[string]struct{}, len(payload.Questions)+len(payload.Fields))
	for _, question := range payload.Questions {
		if !validInteractionKey(question.ID) || !validBoundedText(question.Header, MaxTitleBytes, true) || !validBoundedText(question.Prompt, MaxSummaryBytes, true) || len(question.Options) > MaxInteractionOptions || validateInteractionOptions(question.Options) != nil {
			return invalid(nil)
		}
		if _, duplicate := seen[question.ID]; duplicate {
			return invalid(nil)
		}
		seen[question.ID] = struct{}{}
	}
	for _, field := range payload.Fields {
		if !validInteractionKey(field.ID) || !validBoundedText(field.Label, MaxTitleBytes, true) || !validBoundedText(field.Description, MaxSummaryBytes, false) || !validInteractionFieldType(field.Type) || len(field.Options) > MaxInteractionOptions || validateInteractionOptions(field.Options) != nil {
			return invalid(nil)
		}
		if _, duplicate := seen[field.ID]; duplicate {
			return invalid(nil)
		}
		seen[field.ID] = struct{}{}
	}
	if len(payload.Options) == 0 && len(payload.Questions) == 0 && len(payload.Fields) == 0 {
		return invalid(nil)
	}
	return nil
}

type InteractionResolvedPayload struct {
	RequestID string          `json:"request_id"`
	Kind      InteractionKind `json:"kind"`
	OptionID  string          `json:"option_id"`
}

func (InteractionResolvedPayload) EventType() EventType { return EventInteractionResolved }
func (payload InteractionResolvedPayload) validate() error {
	if !validID(payload.RequestID) || !validInteractionKind(payload.Kind) || (payload.OptionID != "" && !validInteractionKey(payload.OptionID)) {
		return invalid(nil)
	}
	return nil
}

var interactionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func validInteractionKey(value string) bool { return interactionKeyPattern.MatchString(value) }

func validateInteractionOptions(options []InteractionOption) error {
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		if !validInteractionKey(option.ID) || !validBoundedText(option.Label, MaxTitleBytes, true) || !validBoundedText(option.Description, MaxSummaryBytes, false) {
			return errors.New("invalid interaction option")
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return errors.New("duplicate interaction option")
		}
		seen[option.ID] = struct{}{}
	}
	return nil
}

func validToolKind(kind ToolKind) bool {
	switch kind {
	case ToolCommand, ToolFileChange, ToolMCP, ToolWeb, ToolImage, ToolCollaboration, ToolPlan, ToolOther:
		return true
	default:
		return false
	}
}

func validToolStatus(status ToolStatus) bool {
	return status == ToolRunning || status == ToolCompleted || status == ToolFailed || status == ToolInterrupted
}

func validInteractionKind(kind InteractionKind) bool {
	switch kind {
	case InteractionCommandApproval, InteractionFileApproval, InteractionPermissionApproval, InteractionUserInput, InteractionMCPElicitation:
		return true
	default:
		return false
	}
}

func validInteractionFieldType(kind InteractionFieldType) bool {
	switch kind {
	case InteractionText, InteractionNumber, InteractionBoolean, InteractionSelect, InteractionMultiSelect:
		return true
	default:
		return false
	}
}
