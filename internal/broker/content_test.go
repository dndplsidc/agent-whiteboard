package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/stretchr/testify/require"
)

func TestMessageContentConversionPreservesInterleavedOrderAndSourceContext(t *testing.T) {
	resource := agentprotocol.Resource{Kind: agentprotocol.ResourceMarkdown, ID: testID('R'), CreatedAt: testTime(), UpdatedAt: testTime()}
	first := protocolTextReference(testID('A'), "first", "first quote", resource, strings.Repeat("a", 64))
	second := protocolTextReference(testID('B'), "second", "second quote", resource, strings.Repeat("a", 64))
	wire := agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{
		{Type: agentprotocol.MessagePartText, Text: "compare "},
		{Type: agentprotocol.MessagePartReference, Reference: &first},
		{Type: agentprotocol.MessagePartText, Text: " with "},
		{Type: agentprotocol.MessagePartReference, Reference: &second},
		{Type: agentprotocol.MessagePartText, Text: " please"},
	}}

	domain, err := messageContentToProvider(wire)
	require.NoError(t, err)
	require.Equal(t, "compare [first] with [second] please", domain.PlainText())
	require.Equal(t, first.Source.Start.Line, domain.Parts[1].Reference.Source.Start.Line)

	roundTrip, err := messageContentFromProvider(domain, nil)
	require.NoError(t, err)
	require.Equal(t, wire, roundTrip)
}

func TestQueueEditCanReorderOrDeleteReferencesButCannotForgeThem(t *testing.T) {
	resource := agentprotocol.Resource{Kind: agentprotocol.ResourceMarkdown, ID: testID('R'), CreatedAt: testTime(), UpdatedAt: testTime()}
	firstWire := protocolTextReference(testID('A'), "first", "first quote", resource, strings.Repeat("a", 64))
	secondWire := protocolTextReference(testID('B'), "second", "second quote", resource, strings.Repeat("a", 64))
	initialWire := agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{
		{Type: agentprotocol.MessagePartText, Text: "before "},
		{Type: agentprotocol.MessagePartReference, Reference: &firstWire},
		{Type: agentprotocol.MessagePartText, Text: " between "},
		{Type: agentprotocol.MessagePartReference, Reference: &secondWire},
	}}
	initial, err := messageContentToProvider(initialWire)
	require.NoError(t, err)
	queue := NewQueue()
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: testID('T'), MessageID: testID('M'), Content: initial}))

	reorderedWire := agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{
		{Type: agentprotocol.MessagePartReference, Reference: &secondWire},
		{Type: agentprotocol.MessagePartText, Text: " then "},
		{Type: agentprotocol.MessagePartReference, Reference: &firstWire},
	}}
	reordered, err := messageContentToProvider(reorderedWire)
	require.NoError(t, err)
	require.NoError(t, queue.Edit(testID('M'), reordered))
	require.Equal(t, reorderedWire, queue.Items()[0].Content)

	deleted, err := messageContentToProvider(agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &firstWire}}})
	require.NoError(t, err)
	require.NoError(t, queue.Edit(testID('M'), deleted))

	forgedWire := firstWire
	forgedWire.Quote = "different quote"
	forged, err := messageContentToProvider(agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &forgedWire}}})
	require.NoError(t, err)
	require.ErrorIs(t, queue.Edit(testID('M'), forged), ErrQueueInvalid)

	newWire := protocolTextReference(testID('C'), "new", "new quote", resource, strings.Repeat("a", 64))
	added, err := messageContentToProvider(agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &newWire}}})
	require.NoError(t, err)
	require.ErrorIs(t, queue.Edit(testID('M'), added), ErrQueueInvalid)
}

func TestSubmitRejectsReferenceFromStalePageRevision(t *testing.T) {
	broker, _, _, connection, clientID, _, resource, page := turnFixture(t, 9900)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(9901), clientID, conversationID, sequenceID(9902), sequenceID(9903), "", &page)
	payload := command.Payload.(agentprotocol.SubmitPayload)
	reference := protocolTextReference(sequenceID(9904), "selected text", "the selected text", resource, strings.Repeat("f", 64))
	payload.Content = agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &reference}}}
	command.Payload = payload

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorBoardRevisionUnavailable)
}

func protocolTextReference(id, label, quote string, resource agentprotocol.Resource, digest string) agentprotocol.ContextReference {
	return agentprotocol.ContextReference{
		ID: id, Kind: agentprotocol.ReferenceText, Label: label, Quote: quote,
		Source: agentprotocol.ReferenceSource{
			ResourceKind: resource.Kind, ResourceID: resource.ID, ResourceUpdatedAt: resource.UpdatedAt, ContextDigest: digest,
			HeadingPath: []agentprotocol.HeadingReference{{Level: 2, Title: "Details", Ordinal: 1}},
			Start:       agentprotocol.SourceAnchor{Block: 1, Line: 2, Offset: 0},
			End:         agentprotocol.SourceAnchor{Block: 1, Line: 2, Offset: len(quote)},
		},
	}
}
