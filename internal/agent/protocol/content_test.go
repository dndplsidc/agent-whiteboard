package protocol_test

import (
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestV4SubmitRoundTripsOrderedContentStrictly(t *testing.T) {
	conversationID := protocolID("C")
	reference := protocolTextReference()
	command := protocol.Command{
		APIVersion: protocol.APIVersion, CommandID: protocolID("D"), ClientID: protocolID("E"), ConversationID: &conversationID,
		Type: protocol.CommandSubmit,
		Payload: protocol.SubmitPayload{TurnID: protocolID("T"), MessageID: protocolID("M"), Content: protocol.MessageContent{Parts: []protocol.MessagePart{
			{Type: protocol.MessagePartText, Text: "Explain "},
			{Type: protocol.MessagePartReference, Reference: &reference},
			{Type: protocol.MessagePartText, Text: " in one paragraph."},
		}}},
	}
	encoded, err := command.MarshalJSON()
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"api_version":"4"`)
	require.NotContains(t, string(encoded), `"message"`)

	decoded, err := protocol.DecodeCommand(encoded)
	require.NoError(t, err)
	require.Equal(t, command, decoded)
}

func TestV4ContentRejectsNoncanonicalDuplicateAndPhaseInvalidVisuals(t *testing.T) {
	text := protocolTextReference()
	content := protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartText, Text: "a"},
		{Type: protocol.MessagePartText, Text: "b"},
	}}
	require.Error(t, content.ValidateCommand())

	image := protocolTextReference()
	image.Kind = protocol.ReferenceImage
	image.Quote = ""
	image.Visual = &protocol.ReferenceVisual{ImageID: protocolID("I"), Name: "chart.png"}
	require.NoError(t, (protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &image}}}).ValidateCommand())
	require.Error(t, (protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &image}}}).ValidateEvent())
	image.Visual.MediaType = "image/png"
	require.NoError(t, (protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &image}}}).ValidateEvent())

	duplicate := `{"api_version":"4","command_id":"` + protocolID("D") + `","client_id":"` + protocolID("E") + `","conversation_id":"` + protocolID("C") + `","type":"submit","payload":{"turn_id":"` + protocolID("T") + `","message_id":"` + protocolID("M") + `","content":{"parts":[{"type":"text","text":"a","text":"b"}]},"settings":null}}`
	_, err := protocol.DecodeCommand([]byte(duplicate))
	require.Error(t, err)

	text.ID = "bad"
	require.Error(t, (protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &text}}}).ValidateCommand())
}

func TestV4ContentBoundsQuotesSectionsAndReferences(t *testing.T) {
	text := protocolTextReference()
	text.Quote = strings.Repeat("x", protocol.MaxReferenceQuoteBytes+1)
	require.Error(t, (protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &text}}}).ValidateCommand())

	section := protocolTextReference()
	section.Kind = protocol.ReferenceSection
	section.Quote = ""
	section.Markdown = "## Finding\nBody\n"
	section.SectionLines = &protocol.SourceLineRange{Start: 2, End: 5}
	require.NoError(t, (protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &section}}}).ValidateCommand())

	parts := make([]protocol.MessagePart, 0, protocol.MaxMessageReferences+1)
	for index := 0; index <= protocol.MaxMessageReferences; index++ {
		item := protocolTextReference()
		item.ID = protocolID(string(rune('A' + index)))
		parts = append(parts, protocol.MessagePart{Type: protocol.MessagePartReference, Reference: &item})
	}
	require.Error(t, (protocol.MessageContent{Parts: parts}).ValidateCommand())
}

func protocolTextReference() protocol.ContextReference {
	return protocol.ContextReference{
		ID: protocolID("R"), Kind: protocol.ReferenceText, Label: "Finding", Quote: "Only 23% invited a collaborator.",
		Source: protocol.ReferenceSource{
			ResourceKind: protocol.ResourceMarkdown, ResourceID: protocolID("B"),
			ResourceUpdatedAt: time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC), ContextDigest: strings.Repeat("a", 64),
			HeadingPath: []protocol.HeadingReference{{Level: 2, Title: "Analysis", Ordinal: 1}},
			Start:       protocol.SourceAnchor{Block: 3, Line: 5, Offset: 7}, End: protocol.SourceAnchor{Block: 3, Line: 5, Offset: 42},
		},
	}
}

func protocolID(value string) string { return strings.Repeat(value, 32) }
