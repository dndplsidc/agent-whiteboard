package pi

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

const (
	testTurnID    = "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u"
	testMessageID = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"
)

func TestBuildEnvelopeExactInitialBytes(t *testing.T) {
	request := envelopeRequest(provider.ContextInitial)
	encoded, err := BuildEnvelope(request)
	require.NoError(t, err)
	expected := envelopeGolden([][]byte{
		[]byte("initial"), []byte(testTurnID), []byte(testMessageID), []byte(initialInstructions),
		[]byte("A title"), []byte("https://example.test/board"), []byte("markdown"), []byte(testTurnID),
		[]byte("2026-07-27T12:00:00.123456789Z"), []byte("2026-07-27T13:00:00.987654321Z"), []byte("2026-07-28T12:00:00Z"),
		[]byte("creator\x00context\nwith delimiter: end-agent-whiteboard-turn-v1"),
		[]byte("# Markdown\n\x01exact — 世界\nrevision 999\n"), []byte(`{"parts":[{"type":"text","text":"reader\nmessage"}]}`),
	})
	require.Equal(t, expected, encoded)
	parsed, err := ParseEnvelope(encoded)
	require.NoError(t, err)
	require.Equal(t, request.Context.Source, parsed.Source)
	require.Equal(t, request.Context.CreatorContext, parsed.CreatorContext)
	require.Equal(t, request.Content, parsed.ReaderContent)
	require.Equal(t, request.Context.Digest, agent.CalculateContextDigest(parsed.Source, parsed.CreatorContext))
}

func TestBuildEnvelopeExactReplacementBytes(t *testing.T) {
	request := envelopeRequest(provider.ContextReplacement)
	encoded, err := BuildEnvelope(request)
	require.NoError(t, err)
	parsed, err := ParseEnvelope(encoded)
	require.NoError(t, err)
	require.Equal(t, "replacement", parsed.Revision)
	require.Equal(t, replacementInstructions, parsed.ApplicationInstructions)
	require.Contains(t, parsed.ApplicationInstructions, "completely replaces all prior document context")
	require.Equal(t, request.Context.Source, parsed.Source)
}

func TestBuildEnvelopeExactContinuationBytes(t *testing.T) {
	request := provider.TurnRequest{TurnID: testTurnID, MessageID: testMessageID, Content: provider.TextMessage("continue exactly")}
	encoded, err := BuildEnvelope(request)
	require.NoError(t, err)
	expected := envelopeGolden([][]byte{
		[]byte("continuation"), []byte(testTurnID), []byte(testMessageID), []byte(continuationInstructions),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, []byte(`{"parts":[{"type":"text","text":"continue exactly"}]}`),
	})
	require.Equal(t, expected, encoded)
	parsed, err := ParseEnvelope(encoded)
	require.NoError(t, err)
	require.Equal(t, "continuation", parsed.Revision)
	require.False(t, parsed.HasContextFields())
}

func TestParseEnvelopeClonesContentBytes(t *testing.T) {
	encoded, err := BuildEnvelope(envelopeRequest(provider.ContextInitial))
	require.NoError(t, err)
	parsed, err := ParseEnvelope(encoded)
	require.NoError(t, err)
	beforeMarkdown := bytes.Clone(parsed.Source)
	beforeContext := bytes.Clone(parsed.CreatorContext)
	for index := range encoded {
		encoded[index] = 0
	}
	require.Equal(t, beforeMarkdown, parsed.Source)
	require.Equal(t, beforeContext, parsed.CreatorContext)
}

func TestParseEnvelopeRejectsMalformedAndNoncanonicalFrames(t *testing.T) {
	valid, err := BuildEnvelope(envelopeRequest(provider.ContextInitial))
	require.NoError(t, err)
	cases := map[string]func([]byte) []byte{
		"wrong header": func(value []byte) []byte { return append([]byte("wrong\n"), value[len(envelopeHeader):]...) },
		"wrong order": func(value []byte) []byte {
			return bytes.Replace(value, []byte("turn-id 32\n"), []byte("message-id 32\n"), 1)
		},
		"leading zero": func(value []byte) []byte {
			return bytes.Replace(value, []byte("revision 7\n"), []byte("revision 07\n"), 1)
		},
		"signed length": func(value []byte) []byte {
			return bytes.Replace(value, []byte("revision 7\n"), []byte("revision +7\n"), 1)
		},
		"short value": func(value []byte) []byte {
			return bytes.Replace(value, []byte("revision 7\ninitial\n"), []byte("revision 6\ninitial\n"), 1)
		},
		"long value": func(value []byte) []byte {
			return bytes.Replace(value, []byte("revision 7\ninitial\n"), []byte("revision 8\ninitial\n"), 1)
		},
		"truncated":   func(value []byte) []byte { return value[:len(value)-1] },
		"extra bytes": func(value []byte) []byte { return append(value, 'x') },
		"invalid utf8": func(value []byte) []byte {
			copyValue := append([]byte(nil), value...)
			index := bytes.Index(copyValue, []byte("# Markdown"))
			copyValue[index] = 0xff
			return copyValue
		},
		"wrong instructions": func(value []byte) []byte {
			copyValue := append([]byte(nil), value...)
			index := bytes.Index(copyValue, []byte(initialInstructions))
			copyValue[index] = 'X'
			return copyValue
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, parseErr := ParseEnvelope(mutate(append([]byte(nil), valid...)))
			require.Error(t, parseErr)
		})
	}
}

func TestBuildEnvelopeRoundTripsOrderedComponentContent(t *testing.T) {
	component := provider.ContextReference{
		ID: testTurnID, Kind: provider.ReferenceComponent, Label: "Revenue chart",
		Source: provider.ReferenceSource{
			ResourceKind: provider.ResourceHTML, ResourceID: testMessageID,
			ResourceUpdatedAt: time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC), ContextDigest: strings.Repeat("b", 64),
			Anchor: provider.ReferenceAnchor{HTML: &provider.HTMLReferenceAnchor{ElementID: "revenue-chart", Tag: "figure", Ordinal: 2}},
		},
		Component: &provider.ComponentReference{Type: provider.ComponentChart, SourceExcerpt: `<figure id="revenue-chart">Revenue</figure>`},
	}
	request := provider.TurnRequest{TurnID: testTurnID, MessageID: testMessageID, Content: provider.MessageContent{Parts: []provider.MessagePart{
		{Kind: provider.MessagePartText, Text: "Explain "},
		{Kind: provider.MessagePartReference, Reference: &component},
	}}}

	encoded, err := BuildEnvelope(request)
	require.NoError(t, err)
	parsed, err := ParseEnvelope(encoded)
	require.NoError(t, err)
	require.Equal(t, request.Content, parsed.ReaderContent)
	require.Contains(t, parsed.ApplicationInstructions, "untrusted")
}

func TestBuildEnvelopeRejectsInvalidRequest(t *testing.T) {
	request := envelopeRequest(provider.ContextInitial)
	request.Context.Source = []byte{0xff}
	encoded, err := BuildEnvelope(request)
	require.Error(t, err)
	require.Nil(t, encoded)
}

func envelopeRequest(revision provider.ContextRevision) provider.TurnRequest {
	created := time.Date(2026, 7, 27, 12, 0, 0, 123456789, time.UTC)
	updated := time.Date(2026, 7, 27, 13, 0, 0, 987654321, time.UTC)
	expires := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	markdown := []byte("# Markdown\n\x01exact — 世界\nrevision 999\n")
	creator := []byte("creator\x00context\nwith delimiter: end-agent-whiteboard-turn-v1")
	return provider.TurnRequest{
		TurnID: testTurnID, MessageID: testMessageID, Content: provider.TextMessage("reader\nmessage"),
		Context: &provider.PageContext{
			Revision: revision, Source: markdown, CreatorContext: creator, Title: "A title", URL: "https://example.test/board",
			Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: testTurnID, CreatedAt: created, UpdatedAt: updated, ExpiresAt: &expires},
			Digest:   agent.CalculateContextDigest(markdown, creator),
		},
	}
}

func envelopeGolden(values [][]byte) []byte {
	var result strings.Builder
	result.WriteString(envelopeHeader)
	for index, label := range envelopeLabels {
		fmt.Fprintf(&result, "%s %d\n", label, len(values[index]))
		result.Write(values[index])
		result.WriteByte('\n')
	}
	result.WriteString(envelopeFooter)
	return []byte(result.String())
}
