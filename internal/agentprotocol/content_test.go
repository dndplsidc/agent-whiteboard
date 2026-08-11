package agentprotocol_test

import (
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/stretchr/testify/require"
)

func TestV3SubmitRoundTripsOrderedContentStrictly(t *testing.T) {
	conversationID := protocolID("C")
	reference := protocolTextReference()
	command := agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion, CommandID: protocolID("D"), ClientID: protocolID("E"), ConversationID: &conversationID,
		Type: agentprotocol.CommandSubmit,
		Payload: agentprotocol.SubmitPayload{TurnID: protocolID("T"), MessageID: protocolID("M"), Content: agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{
			{Type: agentprotocol.MessagePartText, Text: "Explain "},
			{Type: agentprotocol.MessagePartReference, Reference: &reference},
			{Type: agentprotocol.MessagePartText, Text: " in one paragraph."},
		}}},
	}
	encoded, err := command.MarshalJSON()
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"api_version":"3"`)
	require.NotContains(t, string(encoded), `"message"`)

	decoded, err := agentprotocol.DecodeCommand(encoded)
	require.NoError(t, err)
	require.Equal(t, command, decoded)
}

func TestV3ContentRejectsNoncanonicalDuplicateAndPhaseInvalidVisuals(t *testing.T) {
	text := protocolTextReference()
	content := agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{
		{Type: agentprotocol.MessagePartText, Text: "a"},
		{Type: agentprotocol.MessagePartText, Text: "b"},
	}}
	require.Error(t, content.ValidateCommand())

	image := protocolTextReference()
	image.Kind = agentprotocol.ReferenceImage
	image.Quote = ""
	image.Visual = &agentprotocol.ReferenceVisual{ImageID: protocolID("I"), Name: "chart.png"}
	require.NoError(t, (agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &image}}}).ValidateCommand())
	require.Error(t, (agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &image}}}).ValidateEvent())
	image.Visual.MediaType = "image/png"
	require.NoError(t, (agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &image}}}).ValidateEvent())

	duplicate := `{"api_version":"3","command_id":"` + protocolID("D") + `","client_id":"` + protocolID("E") + `","conversation_id":"` + protocolID("C") + `","type":"submit","payload":{"turn_id":"` + protocolID("T") + `","message_id":"` + protocolID("M") + `","content":{"parts":[{"type":"text","text":"a","text":"b"}]}}}`
	_, err := agentprotocol.DecodeCommand([]byte(duplicate))
	require.Error(t, err)

	text.ID = "bad"
	require.Error(t, (agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &text}}}).ValidateCommand())
}

func TestV3ContentBoundsQuotesSectionsAndReferences(t *testing.T) {
	text := protocolTextReference()
	text.Quote = strings.Repeat("x", agentprotocol.MaxReferenceQuoteBytes+1)
	require.Error(t, (agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &text}}}).ValidateCommand())

	section := protocolTextReference()
	section.Kind = agentprotocol.ReferenceSection
	section.Quote = ""
	section.Markdown = "## Finding\nBody\n"
	section.SectionLines = &agentprotocol.SourceLineRange{Start: 2, End: 5}
	require.NoError(t, (agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{{Type: agentprotocol.MessagePartReference, Reference: &section}}}).ValidateCommand())

	parts := make([]agentprotocol.MessagePart, 0, agentprotocol.MaxMessageReferences+1)
	for index := 0; index <= agentprotocol.MaxMessageReferences; index++ {
		item := protocolTextReference()
		item.ID = protocolID(string(rune('A' + index)))
		parts = append(parts, agentprotocol.MessagePart{Type: agentprotocol.MessagePartReference, Reference: &item})
	}
	require.Error(t, (agentprotocol.MessageContent{Parts: parts}).ValidateCommand())
}

func protocolTextReference() agentprotocol.ContextReference {
	return agentprotocol.ContextReference{
		ID: protocolID("R"), Kind: agentprotocol.ReferenceText, Label: "Finding", Quote: "Only 23% invited a collaborator.",
		Source: agentprotocol.ReferenceSource{
			ResourceKind: agentprotocol.ResourceMarkdown, ResourceID: protocolID("B"),
			ResourceUpdatedAt: time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC), ContextDigest: strings.Repeat("a", 64),
			HeadingPath: []agentprotocol.HeadingReference{{Level: 2, Title: "Analysis", Ordinal: 1}},
			Start:       agentprotocol.SourceAnchor{Block: 3, Line: 5, Offset: 7}, End: agentprotocol.SourceAnchor{Block: 3, Line: 5, Offset: 42},
		},
	}
}

func protocolID(value string) string { return strings.Repeat(value, 32) }
