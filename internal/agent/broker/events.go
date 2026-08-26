package broker

import (
	"errors"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

var (
	ErrNilEventIDGenerator = errors.New("nil event ID generator")
	ErrNilEventClock       = errors.New("nil event clock")
)

// EventFactory creates complete, browser-valid events without consulting wall
// time or randomness directly. The injected dependencies make IDs and times
// deterministic in unit tests and keep event construction pure.
type EventFactory struct {
	conversationID string
	ids            common.IDGenerator
	clock          common.Clock
}

func NewEventFactory(conversationID string, ids common.IDGenerator, clock common.Clock) (*EventFactory, error) {
	if common.ValidateID(conversationID) != nil {
		return nil, errors.New("invalid event conversation ID")
	}
	if common.IsNil(ids) {
		return nil, ErrNilEventIDGenerator
	}
	if common.IsNil(clock) {
		return nil, ErrNilEventClock
	}
	return &EventFactory{conversationID: conversationID, ids: ids, clock: clock}, nil
}

func (factory *EventFactory) New(payload protocol.EventPayload) (protocol.Event, error) {
	if factory == nil || common.IsNil(factory.ids) || common.IsNil(factory.clock) {
		return protocol.Event{}, errors.New("invalid event factory")
	}
	if common.IsNil(payload) {
		return protocol.Event{}, errors.New("nil event payload")
	}
	eventID, err := factory.ids.NewID()
	if err != nil {
		return protocol.Event{}, err
	}
	event := protocol.Event{
		APIVersion:     protocol.APIVersion,
		EventID:        eventID,
		ConversationID: factory.conversationID,
		Type:           payloadType(payload),
		Timestamp:      factory.clock.Now().UTC(),
		Payload:        payload,
	}
	if _, err := protocol.EncodeEvent(event); err != nil {
		return protocol.Event{}, err
	}
	return event, nil
}

func payloadType(payload protocol.EventPayload) protocol.EventType {
	if payload == nil {
		return ""
	}
	return payload.EventType()
}

// FromProvider normalizes a provider event into a browser payload. Provider
// event text is copied only into the protocol's bounded, validated fields;
// native references and untrusted provider payloads have no representation.
func (factory *EventFactory) FromProvider(event provider.Event) (protocol.Event, error) {
	if err := event.Validate(); err != nil {
		return protocol.Event{}, NewBrokerError(protocol.ErrorProviderMalformedStream)
	}
	var payload protocol.EventPayload
	switch event.Kind {
	case provider.EventUserMessage:
		content, err := messageContentFromProvider(event.Content, nil)
		if err != nil {
			return protocol.Event{}, NewBrokerError(protocol.ErrorProviderMalformedStream)
		}
		payload = protocol.UserMessagePayload{TurnID: event.TurnID, MessageID: event.MessageID, Content: content, CreatedAt: event.Timestamp}
	case provider.EventAssistantDelta:
		payload = protocol.AssistantDeltaPayload{TurnID: event.TurnID, MessageID: event.MessageID, Text: event.Text}
	case provider.EventAssistantMessage:
		payload = protocol.AssistantMessagePayload{TurnID: event.TurnID, MessageID: event.MessageID, Text: event.Text, CreatedAt: event.Timestamp}
	case provider.EventActivity:
		payload = protocol.ActivityPayload{Kind: protocol.ActivityKind(event.Activity), Summary: event.Text}
	case provider.EventToolActivity:
		tool := event.Tool
		payload = protocol.ToolActivityPayload{
			ActivityID: tool.ID, TurnID: tool.TurnID, Kind: protocol.ToolKind(tool.Kind), Status: protocol.ToolStatus(tool.Status),
			Title: tool.Title, Summary: tool.Summary, Detail: tool.Detail,
		}
	case provider.EventInteractionRequest:
		converted, err := interactionRequestFromProvider(*event.Interaction)
		if err != nil {
			return protocol.Event{}, NewBrokerError(protocol.ErrorProviderMalformedStream)
		}
		payload = converted
	case provider.EventInteractionResolved:
		payload = protocol.InteractionResolvedPayload{
			RequestID: event.Resolution.RequestID,
			Kind:      protocol.InteractionKind(event.Resolution.Kind),
			OptionID:  event.Resolution.OptionID,
		}
	case provider.EventSkillCatalog:
		catalog := event.SkillCatalog
		skills := make([]protocol.SkillDescriptor, len(catalog.Skills))
		for index, skill := range catalog.Skills {
			skills[index] = protocol.SkillDescriptor{ID: skill.ID, Name: skill.Name, DisplayName: skill.DisplayName, Description: skill.Description, Scope: protocol.SkillScope(skill.Scope)}
		}
		var maxSelectedSkills *int
		if catalog.State == provider.SkillsReady {
			limit := catalog.MaxSelectedSkills
			maxSelectedSkills = &limit
		}
		payload = protocol.SkillCatalogPayload{State: protocol.SkillsState(catalog.State), Skills: skills, MaxSelectedSkills: maxSelectedSkills}
	case provider.EventCompact:
		payload = protocol.CompactionPayload{WorkID: event.Compact.WorkID, Status: protocol.CompactionStatus(event.Compact.Status)}
	case provider.EventBlocked:
		payload = protocol.NewBlockedPayload(protocol.BlockedKind(event.Blocked))
	case provider.EventCompletion:
		payload = protocol.CompletionPayload{TurnID: event.TurnID}
	case provider.EventInterruption:
		payload = protocol.InterruptionPayload{TurnID: event.TurnID, Reason: protocol.InterruptionReason(event.Interruption)}
	case provider.EventTerminalFailure:
		payload = protocol.ErrorPayload{Error: MapProviderError(event.Failure).BrowserError()}
	default:
		return protocol.Event{}, errors.New("unsupported provider event")
	}
	return factory.New(payload)
}

func interactionRequestFromProvider(request provider.InteractionRequest) (protocol.InteractionRequestPayload, error) {
	if request.Validate() != nil {
		return protocol.InteractionRequestPayload{}, errors.New("invalid provider interaction")
	}
	converted := protocol.InteractionRequestPayload{
		RequestID: request.ID, TurnID: request.TurnID, Kind: protocol.InteractionKind(request.Kind), Title: request.Title,
		Summary: request.Summary, Command: request.Command, WorkingDirectory: request.WorkingDirectory,
		Options: make([]protocol.InteractionOption, len(request.Options)), Questions: make([]protocol.InteractionQuestion, len(request.Questions)), Fields: make([]protocol.InteractionField, len(request.Fields)),
	}
	for index, option := range request.Options {
		converted.Options[index] = protocol.InteractionOption{ID: option.ID, Label: option.Label, Description: option.Description}
	}
	for index, question := range request.Questions {
		converted.Questions[index] = protocol.InteractionQuestion{
			ID: question.ID, Header: question.Header, Prompt: question.Prompt, AllowOther: question.AllowOther, Secret: question.Secret, Multiple: question.Multiple,
			Options: make([]protocol.InteractionOption, len(question.Options)),
		}
		for optionIndex, option := range question.Options {
			converted.Questions[index].Options[optionIndex] = protocol.InteractionOption{ID: option.ID, Label: option.Label, Description: option.Description}
		}
	}
	for index, field := range request.Fields {
		converted.Fields[index] = protocol.InteractionField{
			ID: field.ID, Label: field.Label, Description: field.Description, Type: protocol.InteractionFieldType(field.Type), Required: field.Required, Secret: field.Secret, Multiline: field.Multiline,
			Options: make([]protocol.InteractionOption, len(field.Options)),
		}
		for optionIndex, option := range field.Options {
			converted.Fields[index].Options[optionIndex] = protocol.InteractionOption{ID: option.ID, Label: option.Label, Description: option.Description}
		}
	}
	return converted, nil
}
