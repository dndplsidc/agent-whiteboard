package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
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

func (factory *EventFactory) New(payload agentprotocol.EventPayload) (agentprotocol.Event, error) {
	if factory == nil || common.IsNil(factory.ids) || common.IsNil(factory.clock) {
		return agentprotocol.Event{}, errors.New("invalid event factory")
	}
	if common.IsNil(payload) {
		return agentprotocol.Event{}, errors.New("nil event payload")
	}
	eventID, err := factory.ids.NewID()
	if err != nil {
		return agentprotocol.Event{}, err
	}
	event := agentprotocol.Event{
		APIVersion:     agentprotocol.APIVersion,
		EventID:        eventID,
		ConversationID: factory.conversationID,
		Type:           payloadType(payload),
		Timestamp:      factory.clock.Now().UTC(),
		Payload:        payload,
	}
	if _, err := agentprotocol.EncodeEvent(event); err != nil {
		return agentprotocol.Event{}, err
	}
	return event, nil
}

func payloadType(payload agentprotocol.EventPayload) agentprotocol.EventType {
	if payload == nil {
		return ""
	}
	return payload.EventType()
}

// FromProvider normalizes a provider event into a browser payload. Provider
// event text is copied only into the protocol's bounded, validated fields;
// native references and untrusted provider payloads have no representation.
func (factory *EventFactory) FromProvider(event provider.Event) (agentprotocol.Event, error) {
	if err := event.Validate(); err != nil {
		return agentprotocol.Event{}, NewBrokerError(agentprotocol.ErrorProviderMalformedStream)
	}
	var payload agentprotocol.EventPayload
	switch event.Kind {
	case provider.EventUserMessage:
		payload = agentprotocol.UserMessagePayload{TurnID: event.TurnID, MessageID: event.MessageID, Text: event.Text, CreatedAt: event.Timestamp}
	case provider.EventAssistantDelta:
		payload = agentprotocol.AssistantDeltaPayload{TurnID: event.TurnID, MessageID: event.MessageID, Text: event.Text}
	case provider.EventAssistantMessage:
		payload = agentprotocol.AssistantMessagePayload{TurnID: event.TurnID, MessageID: event.MessageID, Text: event.Text, CreatedAt: event.Timestamp}
	case provider.EventActivity:
		payload = agentprotocol.ActivityPayload{Kind: agentprotocol.ActivityKind(event.Activity), Summary: event.Text}
	case provider.EventBlocked:
		payload = agentprotocol.NewBlockedPayload(agentprotocol.BlockedKind(event.Blocked))
	case provider.EventCompletion:
		payload = agentprotocol.CompletionPayload{TurnID: event.TurnID}
	case provider.EventInterruption:
		payload = agentprotocol.InterruptionPayload{TurnID: event.TurnID, Reason: agentprotocol.InterruptionReason(event.Interruption)}
	case provider.EventTerminalFailure:
		payload = agentprotocol.ErrorPayload{Error: MapProviderError(event.Failure).BrowserError()}
	default:
		return agentprotocol.Event{}, errors.New("unsupported provider event")
	}
	return factory.New(payload)
}
