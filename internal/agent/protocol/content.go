package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	MaxMessageParts           = 64
	MaxMessageReferences      = 16
	MaxMessageSkills          = 16
	MaxReferenceQuoteBytes    = 16 << 10
	MaxReferenceMarkdownBytes = 48 << 10
	MaxReferenceLabelBytes    = 256
	MaxHeadingPathItems       = 12
	MaxHeadingTitleBytes      = 256
	MaxReferenceAltBytes      = 512
)

type MessagePartType string

const (
	MessagePartText      MessagePartType = "text"
	MessagePartReference MessagePartType = "reference"
	MessagePartSkill     MessagePartType = "skill"
)

type ReferenceKind string

const (
	ReferenceText    ReferenceKind = "text"
	ReferenceSection ReferenceKind = "section"
	ReferenceImage   ReferenceKind = "image"
)

type MessageContent struct {
	Parts []MessagePart `json:"parts"`
}

type MessagePart struct {
	Type      MessagePartType   `json:"type"`
	Text      string            `json:"text,omitempty"`
	Reference *ContextReference `json:"reference,omitempty"`
	Skill     *SkillInvocation  `json:"skill,omitempty"`
}

type SkillInvocation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ContextReference struct {
	ID           string           `json:"id"`
	Kind         ReferenceKind    `json:"kind"`
	Label        string           `json:"label"`
	Source       ReferenceSource  `json:"source"`
	Quote        string           `json:"quote,omitempty"`
	Markdown     string           `json:"markdown,omitempty"`
	SectionLines *SourceLineRange `json:"section_lines,omitempty"`
	Visual       *ReferenceVisual `json:"visual,omitempty"`
}

type ReferenceSource struct {
	ResourceKind      ResourceKind       `json:"resource_kind"`
	ResourceID        string             `json:"resource_id"`
	ResourceUpdatedAt time.Time          `json:"resource_updated_at"`
	ContextDigest     string             `json:"context_digest"`
	HeadingPath       []HeadingReference `json:"heading_path"`
	Start             SourceAnchor       `json:"start"`
	End               SourceAnchor       `json:"end"`
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
	ImageID   string `json:"image_id"`
	Name      string `json:"name"`
	Alt       string `json:"alt"`
	MediaType string `json:"media_type,omitempty"`
}

func (content MessageContent) MarshalJSON() ([]byte, error) {
	type wire MessageContent
	copyOfContent := content
	if copyOfContent.Parts == nil {
		copyOfContent.Parts = []MessagePart{}
	}
	return marshalApplicationJSON(wire(copyOfContent))
}

func (content *MessageContent) UnmarshalJSON(data []byte) error {
	if err := requireFields(data, "parts"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || bytes.Equal(fields["parts"], []byte("null")) {
		return errors.New("invalid message content")
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

func TextContent(value string) MessageContent {
	if value == "" {
		return MessageContent{Parts: []MessagePart{}}
	}
	return MessageContent{Parts: []MessagePart{{Type: MessagePartText, Text: value}}}
}

func (content MessageContent) ValidateCommand() error { return content.validate(false) }

func (content MessageContent) ValidateEvent() error { return content.validate(true) }

func (content MessageContent) ValidateForProvider(provider ProviderName, event bool) error {
	if !provider.Valid() {
		return invalid(nil)
	}
	return content.validate(event)
}

func (content MessageContent) validate(event bool) error {
	if len(content.Parts) > MaxMessageParts {
		return errors.New("invalid message parts")
	}
	seen := make(map[string]struct{})
	seenSkillIDs := make(map[string]struct{})
	seenSkillNames := make(map[string]struct{})
	references := 0
	skills := 0
	total := 0
	previousText := false
	for _, part := range content.Parts {
		switch part.Type {
		case MessagePartText:
			if part.Reference != nil || part.Skill != nil || part.Text == "" || previousText || !validBoundedText(part.Text, MaxMessageBytes, true) {
				return errors.New("invalid text message part")
			}
			total += len(part.Text)
			previousText = true
		case MessagePartReference:
			if part.Text != "" || part.Reference == nil || part.Skill != nil || validateContextReference(*part.Reference, event) != nil {
				return errors.New("invalid reference message part")
			}
			if _, duplicate := seen[part.Reference.ID]; duplicate {
				return errors.New("duplicate message reference")
			}
			seen[part.Reference.ID] = struct{}{}
			references++
			total += referenceBytes(*part.Reference)
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
			return errors.New("invalid message part type")
		}
		if total > MaxMessageBytes || references > MaxMessageReferences || skills > MaxMessageSkills {
			return ErrMessageTooLarge
		}
	}
	return nil
}

func (content MessageContent) Empty() bool { return len(content.Parts) == 0 }

func (content MessageContent) Clone() MessageContent {
	result := MessageContent{Parts: make([]MessagePart, len(content.Parts))}
	for index, part := range content.Parts {
		result.Parts[index] = MessagePart{Type: part.Type, Text: part.Text}
		if part.Reference != nil {
			cloned := cloneContextReference(*part.Reference)
			result.Parts[index].Reference = &cloned
		}
		if part.Skill != nil {
			cloned := *part.Skill
			result.Parts[index].Skill = &cloned
		}
	}
	return result
}

func (content MessageContent) SemanticBytes() int {
	total := 0
	for _, part := range content.Parts {
		if part.Type == MessagePartText {
			total += len(part.Text)
		} else if part.Reference != nil {
			total += referenceBytes(*part.Reference)
		} else if part.Skill != nil {
			total += len(part.Skill.ID) + len(part.Skill.Name)
		}
	}
	return total
}

func (content MessageContent) InlineImages() []ImageReference {
	images := make([]ImageReference, 0)
	for _, part := range content.Parts {
		if part.Reference != nil && part.Reference.Visual != nil {
			images = append(images, ImageReference{ImageID: part.Reference.Visual.ImageID, Name: part.Reference.Visual.Name})
		}
	}
	return images
}

func validateContextReference(reference ContextReference, event bool) error {
	if !validID(reference.ID) || !validBoundedText(reference.Label, MaxReferenceLabelBytes, true) || validateReferenceSource(reference.Source) != nil {
		return errors.New("invalid context reference")
	}
	switch reference.Kind {
	case ReferenceText:
		if !validBoundedText(reference.Quote, MaxReferenceQuoteBytes, true) || reference.Markdown != "" || reference.SectionLines != nil || reference.Visual != nil {
			return errors.New("invalid text reference")
		}
	case ReferenceSection:
		if reference.Quote != "" || !validBoundedText(reference.Markdown, MaxReferenceMarkdownBytes, true) || reference.SectionLines == nil || reference.SectionLines.Start < 1 || reference.SectionLines.End <= reference.SectionLines.Start || reference.Visual != nil {
			return errors.New("invalid section reference")
		}
	case ReferenceImage:
		if reference.Quote != "" || reference.Markdown != "" || reference.SectionLines != nil || validateReferenceVisual(reference.Visual, event) != nil {
			return errors.New("invalid image reference")
		}
	default:
		return errors.New("invalid reference kind")
	}
	return nil
}

func validateReferenceSource(source ReferenceSource) error {
	if source.ResourceKind != ResourceMarkdown || !validID(source.ResourceID) || source.ResourceUpdatedAt.IsZero() || !validDigest(source.ContextDigest) || len(source.HeadingPath) > MaxHeadingPathItems || !validAnchor(source.Start) || !validAnchor(source.End) || compareAnchors(source.Start, source.End) > 0 {
		return errors.New("invalid reference source")
	}
	for _, heading := range source.HeadingPath {
		if heading.Level < 1 || heading.Level > 6 || heading.Ordinal < 1 || !validBoundedText(heading.Title, MaxHeadingTitleBytes, true) {
			return errors.New("invalid heading reference")
		}
	}
	return nil
}

func validateReferenceVisual(visual *ReferenceVisual, event bool) error {
	if visual == nil || !validID(visual.ImageID) || !validBoundedText(visual.Name, MaxImageNameBytes, true) || !validBoundedText(visual.Alt, MaxReferenceAltBytes, false) {
		return errors.New("invalid reference visual")
	}
	if event {
		if !validImageMediaType(visual.MediaType) {
			return errors.New("invalid reference visual descriptor")
		}
	} else if visual.MediaType != "" {
		return errors.New("command visual contains media type")
	}
	return nil
}

func validAnchor(anchor SourceAnchor) bool {
	return anchor.Block >= 0 && anchor.Line >= 1 && anchor.Offset >= 0
}

func compareAnchors(left, right SourceAnchor) int {
	if left.Block != right.Block {
		return left.Block - right.Block
	}
	return left.Offset - right.Offset
}

func referenceBytes(reference ContextReference) int {
	total := len(reference.ID) + len(reference.Label) + len(reference.Quote) + len(reference.Markdown) + len(reference.Source.ResourceID) + len(reference.Source.ContextDigest)
	for _, heading := range reference.Source.HeadingPath {
		total += len(heading.Title)
	}
	if reference.Visual != nil {
		total += len(reference.Visual.ImageID) + len(reference.Visual.Name) + len(reference.Visual.Alt) + len(reference.Visual.MediaType)
	}
	return total
}

func cloneContextReference(reference ContextReference) ContextReference {
	result := reference
	result.Source.HeadingPath = append([]HeadingReference(nil), reference.Source.HeadingPath...)
	if reference.SectionLines != nil {
		lines := *reference.SectionLines
		result.SectionLines = &lines
	}
	if reference.Visual != nil {
		visual := *reference.Visual
		result.Visual = &visual
	}
	return result
}
