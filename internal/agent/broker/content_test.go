package broker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestMessageContentConversionPreservesInterleavedOrderAndSourceContext(t *testing.T) {
	resource := protocol.Resource{Kind: protocol.ResourceMarkdown, ID: testID('R'), CreatedAt: testTime(), UpdatedAt: testTime()}
	first := protocolTextReference(testID('A'), "first", "first quote", resource, strings.Repeat("a", 64))
	second := protocolTextReference(testID('B'), "second", "second quote", resource, strings.Repeat("a", 64))
	wire := protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartText, Text: "compare "},
		{Type: protocol.MessagePartReference, Reference: &first},
		{Type: protocol.MessagePartText, Text: " with "},
		{Type: protocol.MessagePartReference, Reference: &second},
		{Type: protocol.MessagePartText, Text: " please"},
	}}

	domain, err := messageContentToProvider(wire)
	require.NoError(t, err)
	require.Equal(t, "compare [first] with [second] please", domain.PlainText())
	require.Equal(t, first.Source.Anchor.Markdown.Start.Line, domain.Parts[1].Reference.Source.Anchor.Markdown.Start.Line)

	roundTrip, err := messageContentFromProvider(domain, nil)
	require.NoError(t, err)
	require.Equal(t, wire, roundTrip)
}

func TestQueueEditCanReorderOrDeleteReferencesButCannotForgeThem(t *testing.T) {
	resource := protocol.Resource{Kind: protocol.ResourceMarkdown, ID: testID('R'), CreatedAt: testTime(), UpdatedAt: testTime()}
	firstWire := protocolTextReference(testID('A'), "first", "first quote", resource, strings.Repeat("a", 64))
	secondWire := protocolTextReference(testID('B'), "second", "second quote", resource, strings.Repeat("a", 64))
	initialWire := protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartText, Text: "before "},
		{Type: protocol.MessagePartReference, Reference: &firstWire},
		{Type: protocol.MessagePartText, Text: " between "},
		{Type: protocol.MessagePartReference, Reference: &secondWire},
	}}
	initial, err := messageContentToProvider(initialWire)
	require.NoError(t, err)
	queue := NewQueue()
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: testID('T'), MessageID: testID('M'), Content: initial}))

	reorderedWire := protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartReference, Reference: &secondWire},
		{Type: protocol.MessagePartText, Text: " then "},
		{Type: protocol.MessagePartReference, Reference: &firstWire},
	}}
	reordered, err := messageContentToProvider(reorderedWire)
	require.NoError(t, err)
	require.NoError(t, queue.Edit(testID('M'), reordered))
	require.Equal(t, reorderedWire, queue.Items()[0].Content)

	deleted, err := messageContentToProvider(protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &firstWire}}})
	require.NoError(t, err)
	require.NoError(t, queue.Edit(testID('M'), deleted))

	forgedWire := firstWire
	forgedWire.Quote = "different quote"
	forged, err := messageContentToProvider(protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &forgedWire}}})
	require.NoError(t, err)
	require.ErrorIs(t, queue.Edit(testID('M'), forged), ErrQueueInvalid)

	newWire := protocolTextReference(testID('C'), "new", "new quote", resource, strings.Repeat("a", 64))
	added, err := messageContentToProvider(protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &newWire}}})
	require.NoError(t, err)
	require.ErrorIs(t, queue.Edit(testID('M'), added), ErrQueueInvalid)
}

func TestSubmitRejectsReferenceFromStalePageRevision(t *testing.T) {
	broker, _, _, connection, clientID, _, resource, page := turnFixture(t, 9900)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	command := submitCommand(sequenceID(9901), clientID, conversationID, sequenceID(9902), sequenceID(9903), "", &page)
	payload := command.Payload.(protocol.SubmitPayload)
	reference := protocolTextReference(sequenceID(9904), "selected text", "the selected text", resource, strings.Repeat("f", 64))
	payload.Content = protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &reference}}}
	command.Payload = payload

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorBoardRevisionUnavailable)
}

func TestComponentContentConversionPreservesNestedAnchorsDescriptorsAndClones(t *testing.T) {
	markdownResource := protocol.Resource{Kind: protocol.ResourceMarkdown, ID: testID('R'), CreatedAt: testTime(), UpdatedAt: testTime()}
	htmlResource := protocol.Resource{Kind: protocol.ResourceHTML, ID: testID('H'), CreatedAt: testTime(), UpdatedAt: testTime()}
	markdown := protocolTextReference(testID('A'), "finding", "quoted finding", markdownResource, strings.Repeat("a", 64))
	component := protocolComponentReference(testID('B'), "Revenue chart", protocol.ComponentChart, htmlResource, strings.Repeat("b", 64), nil)
	visual := &protocol.ReferenceVisual{ImageID: testID('I'), Name: "photo.png", Alt: "Product photo"}
	imageComponent := protocolComponentReference(testID('C'), "Product photo", protocol.ComponentImage, htmlResource, strings.Repeat("b", 64), visual)
	wire := protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartText, Text: "Compare "},
		{Type: protocol.MessagePartReference, Reference: &markdown},
		{Type: protocol.MessagePartReference, Reference: &component},
		{Type: protocol.MessagePartReference, Reference: &imageComponent},
	}}

	domain, err := messageContentToProvider(wire)
	require.NoError(t, err)
	require.Equal(t, 1, domain.Parts[3].Reference.Visual.Ordinal)
	require.Equal(t, provider.ComponentChart, domain.Parts[2].Reference.Component.Type)
	require.Equal(t, "revenue-chart", domain.Parts[2].Reference.Source.Anchor.HTML.ElementID)

	// Conversion must not alias caller-owned nested values.
	wire.Parts[2].Reference.Source.Anchor.HTML.ElementID = "mutated"
	wire.Parts[2].Reference.Component.SourceExcerpt = "mutated"
	require.Equal(t, "revenue-chart", domain.Parts[2].Reference.Source.Anchor.HTML.ElementID)
	require.NotEqual(t, "mutated", domain.Parts[2].Reference.Component.SourceExcerpt)

	descriptor := protocol.ImageDescriptor{ImageID: testID('I'), Name: "photo.png", MediaType: "image/png"}
	roundTrip, err := messageContentFromProvider(domain, []protocol.ImageDescriptor{descriptor})
	require.NoError(t, err)
	require.Equal(t, protocol.ComponentChart, roundTrip.Parts[2].Reference.Component.Type)
	require.Equal(t, "revenue-chart", roundTrip.Parts[2].Reference.Source.Anchor.HTML.ElementID)
	require.Equal(t, "image/png", roundTrip.Parts[3].Reference.Visual.MediaType)
}

func TestCurrentPageMatchingRequiresExactReferenceResourceKindRevisionAndDigest(t *testing.T) {
	resource := protocol.Resource{Kind: protocol.ResourceHTML, ID: testID('H'), CreatedAt: testTime(), UpdatedAt: testTime()}
	reference := protocolComponentReference(testID('C'), "Revenue chart", protocol.ComponentChart, resource, strings.Repeat("a", 64), nil)
	content, err := messageContentToProvider(protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &reference}}})
	require.NoError(t, err)
	require.True(t, referencesMatchCurrentPage(content, resource, strings.Repeat("a", 64)))

	kindMismatch := resource
	kindMismatch.Kind = protocol.ResourceMarkdown
	require.False(t, referencesMatchCurrentPage(content, kindMismatch, strings.Repeat("a", 64)))
	stale := resource
	stale.UpdatedAt = stale.UpdatedAt.Add(1)
	require.False(t, referencesMatchCurrentPage(content, stale, strings.Repeat("a", 64)))
	require.False(t, referencesMatchCurrentPage(content, resource, strings.Repeat("b", 64)))
}

func TestQueueEditCannotForgeComponentReference(t *testing.T) {
	resource := protocol.Resource{Kind: protocol.ResourceHTML, ID: testID('H'), CreatedAt: testTime(), UpdatedAt: testTime()}
	visual := &protocol.ReferenceVisual{ImageID: testID('I'), Name: "photo.png", Alt: "Product photo"}
	reference := protocolComponentReference(testID('C'), "Product photo", protocol.ComponentImage, resource, strings.Repeat("a", 64), visual)
	content, err := messageContentToProvider(protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &reference}}})
	require.NoError(t, err)
	input := provider.ImageInput{ID: testID('I'), Name: "photo.png", MediaType: "image/png", Bytes: 4, Path: filepath.Join("/private/tmp", testID('I')+".png")}
	descriptor := protocol.ImageDescriptor{ImageID: input.ID, Name: input.Name, MediaType: input.MediaType}
	queue := NewQueue()
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: testID('T'), MessageID: testID('M'), Content: content, Images: []provider.ImageInput{input}, Descriptors: []protocol.ImageDescriptor{descriptor}}))

	for name, mutate := range map[string]func(*protocol.ContextReference){
		"anchor":  func(value *protocol.ContextReference) { value.Source.Anchor.HTML.Ordinal++ },
		"excerpt": func(value *protocol.ContextReference) { value.Component.SourceExcerpt = "forged" },
		"visual":  func(value *protocol.ContextReference) { value.Visual.Alt = "forged" },
	} {
		t.Run(name, func(t *testing.T) {
			forged := reference
			forged.Source.Anchor.HTML = &protocol.HTMLReferenceAnchor{ElementID: reference.Source.Anchor.HTML.ElementID, Tag: reference.Source.Anchor.HTML.Tag, Ordinal: reference.Source.Anchor.HTML.Ordinal}
			forged.Component = &protocol.ComponentReference{Type: reference.Component.Type, SourceExcerpt: reference.Component.SourceExcerpt}
			forged.Visual = &protocol.ReferenceVisual{ImageID: reference.Visual.ImageID, Name: reference.Visual.Name, Alt: reference.Visual.Alt}
			mutate(&forged)
			edited, convertErr := messageContentToProvider(protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartReference, Reference: &forged}}})
			require.NoError(t, convertErr)
			require.ErrorIs(t, queue.Edit(testID('M'), edited), ErrQueueInvalid)
		})
	}
	projected, err := messageContentFromProvider(queue.items[0].content, queue.items[0].descriptors)
	require.NoError(t, err)
	require.Equal(t, "Product photo", projected.Parts[0].Reference.Label)
}

func TestQueueEditReportsAndRemovesDeletedInlineVisual(t *testing.T) {
	resource := protocol.Resource{Kind: protocol.ResourceMarkdown, ID: testID('R'), CreatedAt: testTime(), UpdatedAt: testTime()}
	reference := protocol.ContextReference{
		ID: testID('I'), Kind: protocol.ReferenceImage, Label: "Diagram",
		Source: protocolTextReference(testID('X'), "source", "quote", resource, strings.Repeat("a", 64)).Source,
		Visual: &protocol.ReferenceVisual{ImageID: testID('V'), Name: "diagram.png", Alt: "Diagram"},
	}
	content, err := messageContentToProvider(protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartReference, Reference: &reference},
		{Type: protocol.MessagePartText, Text: " explain this"},
	}})
	require.NoError(t, err)
	input := provider.ImageInput{ID: testID('V'), Name: "diagram.png", MediaType: "image/png", Bytes: 4, Path: filepath.Join("/private/tmp", testID('V')+".png")}
	descriptor := protocol.ImageDescriptor{ImageID: input.ID, Name: input.Name, MediaType: input.MediaType}
	queue := NewQueue()
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: testID('T'), MessageID: testID('M'), Content: content, Images: []provider.ImageInput{input}, Descriptors: []protocol.ImageDescriptor{descriptor}}))

	removed, err := queue.EditAndRemovedImages(testID('M'), provider.TextMessage("explain without the diagram"))
	require.NoError(t, err)
	require.Equal(t, []string{testID('V')}, removed)
	require.Empty(t, queue.Items()[0].Images)
	require.Equal(t, "explain without the diagram", queue.Items()[0].Content.Parts[0].Text)
}

func protocolTextReference(id, label, quote string, resource protocol.Resource, digest string) protocol.ContextReference {
	return protocol.ContextReference{
		ID: id, Kind: protocol.ReferenceText, Label: label, Quote: quote,
		Source: protocol.ReferenceSource{
			ResourceKind: resource.Kind, ResourceID: resource.ID, ResourceUpdatedAt: resource.UpdatedAt, ContextDigest: digest,
			Anchor: protocol.ReferenceAnchor{Markdown: &protocol.MarkdownReferenceAnchor{
				HeadingPath: []protocol.HeadingReference{{Level: 2, Title: "Details", Ordinal: 1}},
				Start:       protocol.SourceAnchor{Block: 1, Line: 2, Offset: 0},
				End:         protocol.SourceAnchor{Block: 1, Line: 2, Offset: len(quote)},
			}},
		},
	}
}

func protocolComponentReference(id, label string, componentType protocol.ComponentType, resource protocol.Resource, digest string, visual *protocol.ReferenceVisual) protocol.ContextReference {
	return protocol.ContextReference{
		ID: id, Kind: protocol.ReferenceComponent, Label: label,
		Source: protocol.ReferenceSource{
			ResourceKind: resource.Kind, ResourceID: resource.ID, ResourceUpdatedAt: resource.UpdatedAt, ContextDigest: digest,
			Anchor: protocol.ReferenceAnchor{HTML: &protocol.HTMLReferenceAnchor{ElementID: "revenue-chart", Tag: "figure", Ordinal: 2}},
		},
		Component: &protocol.ComponentReference{Type: componentType, SourceExcerpt: `<figure id="revenue-chart">Revenue</figure>`},
		Visual:    visual,
	}
}
