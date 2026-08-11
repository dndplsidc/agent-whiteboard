package provider

import (
	"context"
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

type ToolActivity struct {
	ID      string
	TurnID  string
	Kind    ToolKind
	Status  ToolStatus
	Title   string
	Summary string
	Detail  string
}

func (activity ToolActivity) Validate() error {
	if !validID(activity.ID) || (activity.TurnID != "" && !validID(activity.TurnID)) || !validToolKind(activity.Kind) || !validToolStatus(activity.Status) ||
		!validBoundedText(activity.Title, MaxTitleBytes, true) || !validBoundedText(activity.Summary, MaxSummaryBytes, false) || !validBoundedText(activity.Detail, MaxInteractionTextBytes, false) {
		return errors.New("invalid provider tool activity")
	}
	return nil
}

type InteractionKind string

const (
	InteractionCommandApproval    InteractionKind = "command_approval"
	InteractionFileApproval       InteractionKind = "file_change_approval"
	InteractionPermissionApproval InteractionKind = "permission_approval"
	InteractionUserInput          InteractionKind = "user_input"
	InteractionMCPElicitation     InteractionKind = "mcp_elicitation"
)

type InteractionOption struct {
	ID          string
	Label       string
	Description string
}

type InteractionQuestion struct {
	ID         string
	Header     string
	Prompt     string
	Options    []InteractionOption
	AllowOther bool
	Secret     bool
	Multiple   bool
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
	ID          string
	Label       string
	Description string
	Type        InteractionFieldType
	Required    bool
	Secret      bool
	Options     []InteractionOption
}

type InteractionRequest struct {
	ID               string
	TurnID           string
	Kind             InteractionKind
	Title            string
	Summary          string
	Command          string
	WorkingDirectory string
	Options          []InteractionOption
	Questions        []InteractionQuestion
	Fields           []InteractionField
}

func (request InteractionRequest) Validate() error {
	if !validID(request.ID) || (request.TurnID != "" && !validID(request.TurnID)) || !validInteractionKind(request.Kind) ||
		!validBoundedText(request.Title, MaxTitleBytes, true) || !validBoundedText(request.Summary, MaxSummaryBytes, false) ||
		!validBoundedText(request.Command, MaxInteractionTextBytes, false) || !validBoundedText(request.WorkingDirectory, MaxURLBytes, false) ||
		len(request.Options) > MaxInteractionOptions || len(request.Questions) > MaxInteractionQuestions || len(request.Fields) > MaxInteractionAnswers {
		return errors.New("invalid provider interaction request")
	}
	if err := validateOptions(request.Options); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(request.Questions)+len(request.Fields))
	for _, question := range request.Questions {
		if !validInteractionKey(question.ID) || !validBoundedText(question.Header, MaxTitleBytes, true) || !validBoundedText(question.Prompt, MaxSummaryBytes, true) ||
			len(question.Options) > MaxInteractionOptions || (len(question.Options) == 0 && !question.AllowOther) || validateOptions(question.Options) != nil {
			return errors.New("invalid provider interaction question")
		}
		if _, duplicate := seen[question.ID]; duplicate {
			return errors.New("duplicate provider interaction field")
		}
		seen[question.ID] = struct{}{}
	}
	for _, field := range request.Fields {
		if !validInteractionKey(field.ID) || !validBoundedText(field.Label, MaxTitleBytes, true) || !validBoundedText(field.Description, MaxSummaryBytes, false) ||
			!validInteractionFieldType(field.Type) || len(field.Options) > MaxInteractionOptions || interactionFieldOptionsInvalid(field) || validateOptions(field.Options) != nil {
			return errors.New("invalid provider interaction field")
		}
		if _, duplicate := seen[field.ID]; duplicate {
			return errors.New("duplicate provider interaction field")
		}
		seen[field.ID] = struct{}{}
	}
	if !validInteractionSurface(request) {
		return errors.New("invalid provider interaction response surface")
	}
	return nil
}

func validInteractionSurface(request InteractionRequest) bool {
	switch request.Kind {
	case InteractionCommandApproval, InteractionFileApproval:
		return len(request.Options) != 0 && len(request.Questions) == 0 && len(request.Fields) == 0
	case InteractionPermissionApproval:
		return optionIDsEqual(request.Options, "grantTurn", "grantSession", "decline") && len(request.Questions) == 0 && len(request.Fields) == 1 &&
			request.Fields[0].ID == "permissions" && request.Fields[0].Type == InteractionMultiSelect && len(request.Fields[0].Options) != 0
	case InteractionUserInput:
		return len(request.Options) == 0 && len(request.Questions) != 0 && len(request.Fields) == 0
	case InteractionMCPElicitation:
		return optionIDsEqual(request.Options, "accept", "decline", "cancel") && len(request.Questions) == 0
	default:
		return false
	}
}

func optionIDsEqual(options []InteractionOption, expected ...string) bool {
	if len(options) != len(expected) {
		return false
	}
	wanted := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		wanted[id] = struct{}{}
	}
	for _, option := range options {
		if _, ok := wanted[option.ID]; !ok {
			return false
		}
	}
	return true
}

func interactionFieldOptionsInvalid(field InteractionField) bool {
	switch field.Type {
	case InteractionSelect, InteractionMultiSelect:
		return len(field.Options) == 0
	default:
		return len(field.Options) != 0
	}
}

type InteractionResponse struct {
	RequestID string
	Kind      InteractionKind
	OptionID  string
	Answers   map[string][]string
}

// InteractionResolution reports that the native provider resolved or expired
// an outstanding interaction without a browser response.
type InteractionResolution struct {
	RequestID string
	Kind      InteractionKind
	OptionID  string
}

func (resolution InteractionResolution) Validate() error {
	if !validID(resolution.RequestID) || !validInteractionKind(resolution.Kind) || (resolution.OptionID != "" && !validInteractionKey(resolution.OptionID)) {
		return errors.New("invalid provider interaction resolution")
	}
	return nil
}

func (response InteractionResponse) Validate() error {
	if !validID(response.RequestID) || !validInteractionKind(response.Kind) || (response.OptionID != "" && !validInteractionKey(response.OptionID)) || len(response.Answers) > MaxInteractionAnswers {
		return errors.New("invalid provider interaction response")
	}
	if response.OptionID == "" && len(response.Answers) == 0 {
		return errors.New("empty provider interaction response")
	}
	total := 0
	for key, answers := range response.Answers {
		if !validInteractionKey(key) || len(answers) == 0 || len(answers) > MaxInteractionAnswers {
			return errors.New("invalid provider interaction answer")
		}
		for _, answer := range answers {
			if !validBoundedText(answer, MaxInteractionTextBytes, true) {
				return errors.New("invalid provider interaction answer")
			}
			total += len(answer)
			if total > MaxInteractionTextBytes {
				return errors.New("provider interaction answers exceed byte limit")
			}
		}
	}
	return nil
}

// InteractiveSession is implemented by providers that can pause native work
// for a local reader decision. The base Session remains usable by Pi.
type InteractiveSession interface {
	Respond(context.Context, InteractionResponse) error
	CancelInteraction(context.Context, string) error
}

var interactionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func validInteractionKey(value string) bool { return interactionKeyPattern.MatchString(value) }

func validateOptions(options []InteractionOption) error {
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		if !validInteractionKey(option.ID) || !validBoundedText(option.Label, MaxTitleBytes, true) || !validBoundedText(option.Description, MaxSummaryBytes, false) {
			return errors.New("invalid provider interaction option")
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return errors.New("duplicate provider interaction option")
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
