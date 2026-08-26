package provider_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestMessageContentNormalizesOrderedTextAndReferences(t *testing.T) {
	reference := validTextReference()
	content, err := provider.NormalizeMessageContent(provider.MessageContent{Parts: []provider.MessagePart{
		{Kind: provider.MessagePartText, Text: "Compare "},
		{Kind: provider.MessagePartText, Text: "this "},
		{Kind: provider.MessagePartReference, Reference: &reference},
		{Kind: provider.MessagePartText, Text: " with the baseline."},
	}})
	require.NoError(t, err)
	require.Len(t, content.Parts, 3)
	require.Equal(t, "Compare this ", content.Parts[0].Text)
	require.Equal(t, reference, *content.Parts[1].Reference)
	require.Equal(t, " with the baseline.", content.Parts[2].Text)
	require.NoError(t, content.Validate())
	require.False(t, content.Empty())
	require.False(t, content.TextOnly())

	clone := content.Clone()
	clone.Parts[1].Reference.Label = "changed"
	clone.Parts[1].Reference.Source.Anchor.Markdown.HeadingPath[0].Title = "changed"
	require.Equal(t, "Finding", content.Parts[1].Reference.Label)
	require.Equal(t, "Analysis", content.Parts[1].Reference.Source.Anchor.Markdown.HeadingPath[0].Title)
}

func TestMessageContentValidatesReferenceKindsAndBounds(t *testing.T) {
	text := validTextReference()
	section := validSectionReference()
	image := validImageReference()
	content := provider.MessageContent{Parts: []provider.MessagePart{
		{Kind: provider.MessagePartReference, Reference: &text},
		{Kind: provider.MessagePartReference, Reference: &section},
		{Kind: provider.MessagePartReference, Reference: &image},
	}}
	require.NoError(t, content.Validate())
	require.Equal(t, []string{idC}, content.InlineImageIDs())

	tests := []struct {
		name   string
		mutate func(*provider.ContextReference)
	}{
		{"duplicate id", func(reference *provider.ContextReference) { reference.ID = text.ID }},
		{"invalid digest", func(reference *provider.ContextReference) { reference.Source.ContextDigest = "bad" }},
		{"reversed anchor", func(reference *provider.ContextReference) {
			reference.Source.Anchor.Markdown.End = provider.SourceAnchor{Block: 0, Line: 1, Offset: 0}
		}},
		{"oversized quote", func(reference *provider.ContextReference) {
			reference.Kind = provider.ReferenceText
			reference.Quote = strings.Repeat("x", provider.MaxReferenceQuoteBytes+1)
			reference.Markdown = ""
			reference.SectionLines = nil
		}},
		{"oversized section", func(reference *provider.ContextReference) {
			reference.Kind = provider.ReferenceSection
			reference.Quote = ""
			reference.Markdown = strings.Repeat("x", provider.MaxReferenceMarkdownBytes+1)
			reference.SectionLines = &provider.SourceLineRange{Start: 2, End: 3}
		}},
		{"missing visual", func(reference *provider.ContextReference) {
			reference.Kind = provider.ReferenceImage
			reference.Quote = ""
			reference.Visual = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := content.Clone()
			test.mutate(bad.Parts[2].Reference)
			require.Error(t, bad.Validate())
		})
	}
}

func TestComponentReferencesValidateCloneSizeAndImages(t *testing.T) {
	component := validComponentReference()
	content := provider.MessageContent{Parts: []provider.MessagePart{{Kind: provider.MessagePartReference, Reference: &component}}}
	require.NoError(t, content.Validate())
	require.Empty(t, content.InlineImageIDs())
	expectedBytes := len(component.ID) + len(component.Label) + len(component.Source.ResourceID) + len(component.Source.ContextDigest) +
		len(component.Source.Anchor.HTML.ElementID) + len(component.Source.Anchor.HTML.Tag) + len(component.Component.Type) + len(component.Component.SourceExcerpt)
	require.Equal(t, expectedBytes, content.SemanticBytes())

	clone := content.Clone()
	clone.Parts[0].Reference.Source.Anchor.HTML.ElementID = "changed"
	clone.Parts[0].Reference.Component.SourceExcerpt = "changed"
	require.Equal(t, "revenue", content.Parts[0].Reference.Source.Anchor.HTML.ElementID)
	require.NotEqual(t, "changed", content.Parts[0].Reference.Component.SourceExcerpt)

	component.Component.Type = provider.ComponentImage
	component.Visual = &provider.ReferenceVisual{ImageID: idC, Name: "revenue.png", Alt: "Revenue", Ordinal: 1}
	withVisual := provider.MessageContent{Parts: []provider.MessagePart{{Kind: provider.MessagePartReference, Reference: &component}}}
	require.NoError(t, withVisual.Validate())
	require.Equal(t, []string{idC}, withVisual.InlineImageIDs())
	require.NoError(t, withVisual.ValidateImages([]provider.ImageInput{{ID: idC}}))

	component.Component.Type = provider.ComponentChart
	require.Error(t, (provider.MessageContent{Parts: []provider.MessagePart{{Kind: provider.MessagePartReference, Reference: &component}}}).Validate())
}

func TestMessageContentRejectsNonCanonicalPartsAndAggregateOverflow(t *testing.T) {
	tests := []provider.MessageContent{
		{Parts: []provider.MessagePart{{Kind: provider.MessagePartText, Text: ""}}},
		{Parts: []provider.MessagePart{{Kind: provider.MessagePartText, Text: "a"}, {Kind: provider.MessagePartText, Text: "b"}}},
		{Parts: []provider.MessagePart{{Kind: provider.MessagePartReference}}},
		{Parts: []provider.MessagePart{{Kind: provider.MessagePartText, Text: strings.Repeat("x", provider.MaxTurnMessageBytes+1)}}},
	}
	for _, content := range tests {
		require.Error(t, content.Validate())
	}

	parts := make([]provider.MessagePart, 0, provider.MaxMessageReferences+1)
	for index := 0; index <= provider.MaxMessageReferences; index++ {
		reference := validTextReference()
		reference.ID = referenceID(index)
		parts = append(parts, provider.MessagePart{Kind: provider.MessagePartReference, Reference: &reference})
	}
	require.Error(t, (provider.MessageContent{Parts: parts}).Validate())
}

func TestTurnRequestAcceptsContentOrImagesAndRejectsVisualMismatch(t *testing.T) {
	request := provider.TurnRequest{TurnID: idA, MessageID: idB, Content: provider.TextMessage("question")}
	require.NoError(t, request.Validate())

	request.Content = provider.MessageContent{}
	require.Error(t, request.Validate())

	image := provider.ImageInput{ID: idC, Name: "chart.png", MediaType: "image/png", Bytes: 4, Path: "/tmp/chart.png"}
	request.Images = []provider.ImageInput{image}
	require.NoError(t, request.Validate())

	reference := validImageReference()
	request.Content = provider.MessageContent{Parts: []provider.MessagePart{{Kind: provider.MessagePartReference, Reference: &reference}}}
	require.NoError(t, request.Validate())

	request.Images = nil
	require.Error(t, request.Validate())
	require.Equal(t, "question", provider.TextMessage("question").PlainText())
}

func validTextReference() provider.ContextReference {
	return provider.ContextReference{
		ID: idA, Kind: provider.ReferenceText, Label: "Finding", Quote: "Only 23% invited a collaborator.",
		Source: validReferenceSource(),
	}
}

func validSectionReference() provider.ContextReference {
	reference := validTextReference()
	reference.ID = idB
	reference.Kind = provider.ReferenceSection
	reference.Label = "Recommendation"
	reference.Quote = ""
	reference.Markdown = "## Recommendation\nShip the focused flow.\n"
	reference.SectionLines = &provider.SourceLineRange{Start: 8, End: 11}
	return reference
}

func validImageReference() provider.ContextReference {
	reference := validTextReference()
	reference.ID = idC
	reference.Kind = provider.ReferenceImage
	reference.Label = "Activation chart"
	reference.Quote = ""
	reference.Visual = &provider.ReferenceVisual{ImageID: idC, Name: "activation.png", Alt: "Activation by cohort", Ordinal: 1}
	return reference
}

func validReferenceSource() provider.ReferenceSource {
	return provider.ReferenceSource{
		ResourceKind: provider.ResourceMarkdown, ResourceID: idA,
		ResourceUpdatedAt: time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC), ContextDigest: strings.Repeat("a", 64),
		Anchor: provider.ReferenceAnchor{Markdown: &provider.MarkdownReferenceAnchor{
			HeadingPath: []provider.HeadingReference{{Level: 2, Title: "Analysis", Ordinal: 1}},
			Start:       provider.SourceAnchor{Block: 3, Line: 5, Offset: 7}, End: provider.SourceAnchor{Block: 3, Line: 5, Offset: 42},
		}},
	}
}

func validComponentReference() provider.ContextReference {
	return provider.ContextReference{
		ID: idB, Kind: provider.ReferenceComponent, Label: "Revenue chart",
		Source: provider.ReferenceSource{
			ResourceKind: provider.ResourceHTML, ResourceID: idA,
			ResourceUpdatedAt: time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC), ContextDigest: strings.Repeat("b", 64),
			Anchor: provider.ReferenceAnchor{HTML: &provider.HTMLReferenceAnchor{ElementID: "revenue", Tag: "figure", Ordinal: 4}},
		},
		Component: &provider.ComponentReference{Type: provider.ComponentChart, SourceExcerpt: `<figure id="revenue">Revenue</figure>`},
	}
}

func referenceID(index int) string {
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	return strings.Repeat(string(alphabet[index%len(alphabet)]), 32)
}
