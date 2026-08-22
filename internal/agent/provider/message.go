package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	MaxMessageParts                = 64
	MaxMessageReferences           = 16
	MaxMessageSkills               = 16
	MaxReferenceQuoteBytes         = 16 << 10
	MaxReferenceMarkdownBytes      = 48 << 10
	MaxReferenceLabelBytes         = 256
	MaxHeadingPathItems            = 12
	MaxHeadingTitleBytes           = 256
	MaxReferenceAltBytes           = 512
	MaxReferenceElementIDBytes     = 256
	MaxReferenceTagBytes           = 32
	MaxReferenceSourceExcerptBytes = 48 << 10
	MaxComponentOrdinal            = 128
)

type MessagePartKind string

const (
	MessagePartText      MessagePartKind = "text"
	MessagePartReference MessagePartKind = "reference"
	MessagePartSkill     MessagePartKind = "skill"
)

type ReferenceKind string

const (
	ReferenceText      ReferenceKind = "text"
	ReferenceSection   ReferenceKind = "section"
	ReferenceImage     ReferenceKind = "image"
	ReferenceComponent ReferenceKind = "component"
)

type ComponentType string

const (
	ComponentSection ComponentType = "section"
	ComponentImage   ComponentType = "image"
	ComponentChart   ComponentType = "chart"
	ComponentTable   ComponentType = "table"
	ComponentCode    ComponentType = "code"
	ComponentQuote   ComponentType = "quote"
	ComponentCustom  ComponentType = "component"
)

type MessageContent struct {
	Parts []MessagePart `json:"parts"`
}

type MessagePart struct {
	Kind      MessagePartKind   `json:"type"`
	Text      string            `json:"text,omitempty"`
	Reference *ContextReference `json:"reference,omitempty"`
	Skill     *SkillInvocation  `json:"skill,omitempty"`
}

type SkillInvocation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ContextReference struct {
	ID           string              `json:"id"`
	Kind         ReferenceKind       `json:"kind"`
	Label        string              `json:"label"`
	Source       ReferenceSource     `json:"source"`
	Quote        string              `json:"quote,omitempty"`
	Markdown     string              `json:"markdown,omitempty"`
	SectionLines *SourceLineRange    `json:"section_lines,omitempty"`
	Component    *ComponentReference `json:"component,omitempty"`
	Visual       *ReferenceVisual    `json:"visual,omitempty"`
}

type ReferenceSource struct {
	ResourceKind      ResourceKind    `json:"resource_kind"`
	ResourceID        string          `json:"resource_id"`
	ResourceUpdatedAt time.Time       `json:"resource_updated_at"`
	ContextDigest     string          `json:"context_digest"`
	Anchor            ReferenceAnchor `json:"anchor"`
	// Deprecated internal compatibility projection. These fields are never
	// accepted from or emitted onto the native v4 wire.
	HeadingPath []HeadingReference `json:"-"`
	Start       SourceAnchor       `json:"-"`
	End         SourceAnchor       `json:"-"`
}

type ReferenceAnchor struct {
	Markdown *MarkdownReferenceAnchor `json:"markdown,omitempty"`
	HTML     *HTMLReferenceAnchor     `json:"html,omitempty"`
}

type MarkdownReferenceAnchor struct {
	HeadingPath []HeadingReference `json:"heading_path"`
	Start       SourceAnchor       `json:"start"`
	End         SourceAnchor       `json:"end"`
}

type HTMLReferenceAnchor struct {
	ElementID string `json:"element_id"`
	Tag       string `json:"tag"`
	Ordinal   int    `json:"ordinal"`
}

type ComponentReference struct {
	Type          ComponentType `json:"type"`
	SourceExcerpt string        `json:"source_excerpt"`
}

func (source ReferenceSource) MarshalJSON() ([]byte, error) {
	type wire struct {
		ResourceKind      ResourceKind    `json:"resource_kind"`
		ResourceID        string          `json:"resource_id"`
		ResourceUpdatedAt time.Time       `json:"resource_updated_at"`
		ContextDigest     string          `json:"context_digest"`
		Anchor            ReferenceAnchor `json:"anchor"`
	}
	return json.Marshal(wire{source.ResourceKind, source.ResourceID, source.ResourceUpdatedAt, source.ContextDigest, effectiveReferenceAnchor(source)})
}

type HeadingReference struct {
	Level   int    `json:"level"`
	Title   string `json:"title"`
	Ordinal int    `json:"ordinal"`
}

type SourceAnchor struct {
	Block  int `json:"block"`
	Line   int `json:"line"`
	Offset int `json:"offset"`
}

type SourceLineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type ReferenceVisual struct {
	ImageID string `json:"image_id"`
	Name    string `json:"name"`
	Alt     string `json:"alt"`
	Ordinal int    `json:"ordinal"`
}

func (content MessageContent) MarshalJSON() ([]byte, error) {
	type wire MessageContent
	copyOfContent := content
	if copyOfContent.Parts == nil {
		copyOfContent.Parts = []MessagePart{}
	}
	return json.Marshal(wire(copyOfContent))
}

func (content *MessageContent) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	parts, exists := fields["parts"]
	if !exists || bytes.Equal(parts, []byte("null")) {
		return errors.New("message content parts are required")
	}
	type wire MessageContent
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded wire
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid message content")
	}
	*content = MessageContent(decoded)
	return nil
}

func TextMessage(value string) MessageContent {
	if value == "" {
		return MessageContent{}
	}
	return MessageContent{Parts: []MessagePart{{Kind: MessagePartText, Text: value}}}
}

func NormalizeMessageContent(content MessageContent) (MessageContent, error) {
	result := MessageContent{Parts: make([]MessagePart, 0, len(content.Parts))}
	for _, part := range content.Parts {
		switch part.Kind {
		case MessagePartText:
			if part.Reference != nil || part.Skill != nil {
				return MessageContent{}, errors.New("text message part has structured content")
			}
			if part.Text == "" {
				continue
			}
			if len(result.Parts) > 0 && result.Parts[len(result.Parts)-1].Kind == MessagePartText {
				result.Parts[len(result.Parts)-1].Text += part.Text
				continue
			}
			result.Parts = append(result.Parts, MessagePart{Kind: MessagePartText, Text: part.Text})
		case MessagePartReference:
			if part.Text != "" || part.Reference == nil || part.Skill != nil {
				return MessageContent{}, errors.New("invalid reference message part")
			}
			cloned := cloneReference(*part.Reference)
			result.Parts = append(result.Parts, MessagePart{Kind: MessagePartReference, Reference: &cloned})
		case MessagePartSkill:
			if part.Text != "" || part.Reference != nil || part.Skill == nil {
				return MessageContent{}, errors.New("invalid skill message part")
			}
			cloned := *part.Skill
			result.Parts = append(result.Parts, MessagePart{Kind: MessagePartSkill, Skill: &cloned})
		default:
			return MessageContent{}, errors.New("invalid message part kind")
		}
	}
	if err := result.Validate(); err != nil {
		return MessageContent{}, err
	}
	return result, nil
}

func (content MessageContent) Validate() error {
	if len(content.Parts) > MaxMessageParts {
		return errors.New("too many message parts")
	}
	seenReferences := make(map[string]struct{})
	seenSkillIDs := make(map[string]struct{})
	seenSkillNames := make(map[string]struct{})
	references := 0
	skills := 0
	total := 0
	previousText := false
	for _, part := range content.Parts {
		switch part.Kind {
		case MessagePartText:
			if part.Reference != nil || part.Skill != nil || part.Text == "" || previousText || !validBoundedText(part.Text, MaxTurnMessageBytes, true) {
				return errors.New("invalid text message part")
			}
			total += len(part.Text)
			previousText = true
		case MessagePartReference:
			if part.Text != "" || part.Reference == nil || part.Skill != nil || validateContextReference(*part.Reference) != nil {
				return errors.New("invalid reference message part")
			}
			if _, duplicate := seenReferences[part.Reference.ID]; duplicate {
				return errors.New("duplicate message reference")
			}
			seenReferences[part.Reference.ID] = struct{}{}
			references++
			total += referenceSemanticBytes(*part.Reference)
			previousText = false
		case MessagePartSkill:
			if part.Text != "" || part.Reference != nil || part.Skill == nil || !validID(part.Skill.ID) || !validBoundedText(part.Skill.Name, MaxSkillNameBytes, true) {
				return errors.New("invalid skill message part")
			}
			if _, duplicate := seenSkillIDs[part.Skill.ID]; duplicate {
				return errors.New("duplicate message skill")
			}
			if _, duplicate := seenSkillNames[part.Skill.Name]; duplicate {
				return errors.New("duplicate message skill name")
			}
			seenSkillIDs[part.Skill.ID] = struct{}{}
			seenSkillNames[part.Skill.Name] = struct{}{}
			skills++
			total += len(part.Skill.ID) + len(part.Skill.Name)
			previousText = false
		default:
			return errors.New("invalid message part kind")
		}
		if total > MaxTurnMessageBytes || references > MaxMessageReferences || skills > MaxMessageSkills {
			return errors.New("message content exceeds limit")
		}
	}
	return nil
}

func (content MessageContent) Empty() bool { return len(content.Parts) == 0 }

func (content MessageContent) TextOnly() bool {
	return len(content.Parts) == 0 || len(content.Parts) == 1 && content.Parts[0].Kind == MessagePartText
}

func (content MessageContent) PlainText() string {
	var builder strings.Builder
	for _, part := range content.Parts {
		if part.Kind == MessagePartText {
			builder.WriteString(part.Text)
		} else if part.Reference != nil {
			builder.WriteByte('[')
			builder.WriteString(part.Reference.Label)
			builder.WriteByte(']')
		} else if part.Skill != nil {
			builder.WriteByte('$')
			builder.WriteString(part.Skill.Name)
		}
	}
	return builder.String()
}

func (content MessageContent) Clone() MessageContent {
	result := MessageContent{Parts: make([]MessagePart, len(content.Parts))}
	for index, part := range content.Parts {
		result.Parts[index] = MessagePart{Kind: part.Kind, Text: part.Text}
		if part.Reference != nil {
			cloned := cloneReference(*part.Reference)
			result.Parts[index].Reference = &cloned
		}
		if part.Skill != nil {
			cloned := *part.Skill
			result.Parts[index].Skill = &cloned
		}
	}
	return result
}

func (content MessageContent) InlineImageIDs() []string {
	result := make([]string, 0)
	for _, part := range content.Parts {
		if part.Reference != nil && part.Reference.Visual != nil {
			result = append(result, part.Reference.Visual.ImageID)
		}
	}
	return result
}

func (content MessageContent) SemanticBytes() int {
	total := 0
	for _, part := range content.Parts {
		if part.Kind == MessagePartText {
			total += len(part.Text)
		} else if part.Reference != nil {
			total += referenceSemanticBytes(*part.Reference)
		} else if part.Skill != nil {
			total += len(part.Skill.ID) + len(part.Skill.Name)
		}
	}
	return total
}

func (content MessageContent) ValidateImages(images []ImageInput) error {
	for _, part := range content.Parts {
		if part.Reference == nil || part.Reference.Visual == nil {
			continue
		}
		visual := part.Reference.Visual
		if visual == nil || visual.Ordinal < 1 || visual.Ordinal > len(images) || images[visual.Ordinal-1].ID != visual.ImageID {
			return errors.New("inline image reference does not match provider input")
		}
	}
	return nil
}

func validateContextReference(reference ContextReference) error {
	if !validID(reference.ID) || !validBoundedText(reference.Label, MaxReferenceLabelBytes, true) || validateReferenceSource(reference.Source) != nil {
		return errors.New("invalid context reference")
	}
	switch reference.Kind {
	case ReferenceText:
		if reference.Source.ResourceKind != ResourceMarkdown || !validBoundedText(reference.Quote, MaxReferenceQuoteBytes, true) || reference.Markdown != "" || reference.SectionLines != nil || reference.Component != nil || reference.Visual != nil {
			return errors.New("invalid text reference")
		}
	case ReferenceSection:
		if reference.Source.ResourceKind != ResourceMarkdown || reference.Quote != "" || !validBoundedText(reference.Markdown, MaxReferenceMarkdownBytes, true) || reference.SectionLines == nil || reference.SectionLines.Start < 1 || reference.SectionLines.End <= reference.SectionLines.Start || reference.Component != nil || reference.Visual != nil {
			return errors.New("invalid section reference")
		}
	case ReferenceImage:
		if reference.Source.ResourceKind != ResourceMarkdown || reference.Quote != "" || reference.Markdown != "" || reference.SectionLines != nil || reference.Component != nil || validateReferenceVisual(reference.Visual) != nil {
			return errors.New("invalid image reference")
		}
	case ReferenceComponent:
		if reference.Source.ResourceKind != ResourceHTML || reference.Quote != "" || reference.Markdown != "" || reference.SectionLines != nil || reference.Component == nil || !reference.Component.Type.Valid() || !validBoundedText(reference.Component.SourceExcerpt, MaxReferenceSourceExcerptBytes, true) || (reference.Visual != nil && (reference.Component.Type != ComponentImage || validateReferenceVisual(reference.Visual) != nil)) {
			return errors.New("invalid component reference")
		}
	default:
		return errors.New("invalid reference kind")
	}
	return nil
}

func validateReferenceSource(source ReferenceSource) error {
	anchorUnion := effectiveReferenceAnchor(source)
	if !validID(source.ResourceID) || source.ResourceUpdatedAt.IsZero() || !validContextDigest(source.ContextDigest) || (anchorUnion.Markdown == nil) == (anchorUnion.HTML == nil) {
		return errors.New("invalid reference source")
	}
	if source.ResourceKind == ResourceMarkdown && anchorUnion.Markdown != nil {
		anchor := anchorUnion.Markdown
		if len(anchor.HeadingPath) > MaxHeadingPathItems || !validSourceAnchor(anchor.Start) || !validSourceAnchor(anchor.End) || compareSourceAnchors(anchor.Start, anchor.End) > 0 {
			return errors.New("invalid Markdown reference source")
		}
		for _, heading := range anchor.HeadingPath {
			if heading.Level < 1 || heading.Level > 6 || heading.Ordinal < 1 || !validBoundedText(heading.Title, MaxHeadingTitleBytes, true) {
				return errors.New("invalid heading reference")
			}
		}
		return nil
	}
	if source.ResourceKind == ResourceHTML && anchorUnion.HTML != nil {
		anchor := anchorUnion.HTML
		if !validBoundedText(anchor.ElementID, MaxReferenceElementIDBytes, true) || !validElementTag(anchor.Tag) || anchor.Ordinal < 1 || anchor.Ordinal > MaxComponentOrdinal {
			return errors.New("invalid HTML reference source")
		}
		return nil
	}
	return errors.New("reference resource kind does not match anchor")
}

func effectiveReferenceAnchor(source ReferenceSource) ReferenceAnchor {
	if source.Anchor.Markdown != nil || source.Anchor.HTML != nil {
		return source.Anchor
	}
	if validSourceAnchor(source.Start) && validSourceAnchor(source.End) {
		return ReferenceAnchor{Markdown: &MarkdownReferenceAnchor{HeadingPath: source.HeadingPath, Start: source.Start, End: source.End}}
	}
	return source.Anchor
}

func (kind ComponentType) Valid() bool {
	switch kind {
	case ComponentSection, ComponentImage, ComponentChart, ComponentTable, ComponentCode, ComponentQuote, ComponentCustom:
		return true
	default:
		return false
	}
}

func validElementTag(value string) bool {
	if !validBoundedText(value, MaxReferenceTagBytes, true) {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func validSourceAnchor(anchor SourceAnchor) bool {
	return anchor.Block >= 0 && anchor.Line >= 1 && anchor.Offset >= 0
}

func compareSourceAnchors(left, right SourceAnchor) int {
	if left.Block != right.Block {
		return left.Block - right.Block
	}
	return left.Offset - right.Offset
}

func validateReferenceVisual(visual *ReferenceVisual) error {
	if visual == nil || !validID(visual.ImageID) || !validBoundedText(visual.Name, MaxImageNameBytes, true) || !validBoundedText(visual.Alt, MaxReferenceAltBytes, false) || visual.Ordinal < 1 || visual.Ordinal > MaxImagesPerTurn {
		return errors.New("invalid reference visual")
	}
	return nil
}

func referenceSemanticBytes(reference ContextReference) int {
	total := len(reference.ID) + len(reference.Label) + len(reference.Quote) + len(reference.Markdown) + len(reference.Source.ResourceID) + len(reference.Source.ContextDigest)
	anchorUnion := effectiveReferenceAnchor(reference.Source)
	if anchorUnion.Markdown != nil {
		for _, heading := range anchorUnion.Markdown.HeadingPath {
			total += len(heading.Title)
		}
	}
	if anchorUnion.HTML != nil {
		total += len(anchorUnion.HTML.ElementID) + len(anchorUnion.HTML.Tag)
	}
	if reference.Component != nil {
		total += len(reference.Component.Type) + len(reference.Component.SourceExcerpt)
	}
	if reference.Visual != nil {
		total += len(reference.Visual.ImageID) + len(reference.Visual.Name) + len(reference.Visual.Alt)
	}
	return total
}

func cloneReference(reference ContextReference) ContextReference {
	result := reference
	anchorUnion := effectiveReferenceAnchor(reference.Source)
	if anchorUnion.Markdown != nil {
		anchor := *anchorUnion.Markdown
		anchor.HeadingPath = append([]HeadingReference(nil), anchor.HeadingPath...)
		result.Source.Anchor.Markdown = &anchor
	}
	if anchorUnion.HTML != nil {
		anchor := *anchorUnion.HTML
		result.Source.Anchor.HTML = &anchor
	}
	if reference.SectionLines != nil {
		lines := *reference.SectionLines
		result.SectionLines = &lines
	}
	if reference.Component != nil {
		component := *reference.Component
		result.Component = &component
	}
	if reference.Visual != nil {
		visual := *reference.Visual
		result.Visual = &visual
	}
	return result
}

func validContextDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
