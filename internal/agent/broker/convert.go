package broker

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/config"
)

// ConnectIdentity is the browser-visible identity needed to address one
// conversation. Origin is deliberately kept outside protocol's payload:
// it is supplied by the authenticated HTTP request boundary.
type ConnectIdentity struct {
	Origin        string
	Provider      protocol.ProviderName
	Resource      protocol.Resource
	ContextDigest string
}

func (identity ConnectIdentity) Validate(authorizedOrigin string) error {
	canonical, err := config.CanonicalBrowserOrigin(identity.Origin)
	if err != nil || canonical != identity.Origin {
		return errors.New("invalid connect origin")
	}
	authorized, err := config.CanonicalBrowserOrigin(authorizedOrigin)
	if err != nil || authorized != authorizedOrigin || authorized != identity.Origin {
		return errors.New("connect origin is not the authorized canonical origin")
	}
	if !identity.Provider.Valid() || validateProtocolResource(identity.Resource) != nil || !validDigest(identity.ContextDigest) {
		return errors.New("invalid connect identity")
	}
	return nil
}

// ConnectIdentityToState converts only after requiring an exact authorized
// canonical origin. No provider-native identity is accepted here.
func ConnectIdentityToState(identity ConnectIdentity, authorizedOrigin string) (statepkg.Identity, error) {
	if err := identity.Validate(authorizedOrigin); err != nil {
		return statepkg.Identity{}, err
	}
	name, err := providerNameToDomain(identity.Provider)
	if err != nil {
		return statepkg.Identity{}, err
	}
	kind, err := resourceKindToState(identity.Resource.Kind)
	if err != nil {
		return statepkg.Identity{}, err
	}
	return statepkg.Identity{
		Origin:       identity.Origin,
		Kind:         kind,
		CapabilityID: identity.Resource.ID,
		Provider:     name,
	}, nil
}

func providerNameToDomain(name protocol.ProviderName) (provider.Name, error) {
	switch name {
	case protocol.ProviderPi:
		return provider.NamePi, nil
	case protocol.ProviderCodex:
		return provider.NameCodex, nil
	default:
		return "", errors.New("invalid provider")
	}
}

func providerNameFromDomain(name provider.Name) (protocol.ProviderName, error) {
	switch name {
	case provider.NamePi:
		return protocol.ProviderPi, nil
	case provider.NameCodex:
		return protocol.ProviderCodex, nil
	default:
		return "", errors.New("invalid provider")
	}
}

// IdentityFromConnect adapts a protocol connect payload after binding the
// request's Origin. ReplayAfter is transport state and is intentionally not
// part of the durable identity.
func IdentityFromConnect(origin string, payload protocol.ConnectPayload, authorizedOrigin string) (statepkg.Identity, error) {
	if payload.ReplayAfter != "" {
		if err := common.ValidateID(payload.ReplayAfter); err != nil {
			return statepkg.Identity{}, errors.New("invalid replay cursor")
		}
	}
	return ConnectIdentityToState(ConnectIdentity{Origin: origin, Provider: payload.Provider, Resource: payload.Resource, ContextDigest: payload.ContextDigest}, authorizedOrigin)
}

func ResourceToProvider(resource protocol.Resource) (provider.Resource, error) {
	if err := validateProtocolResource(resource); err != nil {
		return provider.Resource{}, err
	}
	kind, err := resourceKindToProvider(resource.Kind)
	if err != nil {
		return provider.Resource{}, err
	}
	return provider.Resource{
		Kind:      kind,
		ID:        resource.ID,
		CreatedAt: resource.CreatedAt,
		UpdatedAt: resource.UpdatedAt,
		ExpiresAt: cloneTimePointer(resource.ExpiresAt),
	}, nil
}

func ResourceFromProvider(resource provider.Resource) (protocol.Resource, error) {
	kind, err := resourceKindFromProvider(resource.Kind)
	if err != nil {
		return protocol.Resource{}, err
	}
	converted := protocol.Resource{
		Kind:      kind,
		ID:        resource.ID,
		CreatedAt: resource.CreatedAt,
		UpdatedAt: resource.UpdatedAt,
		ExpiresAt: cloneTimePointer(resource.ExpiresAt),
	}
	if err := validateProtocolResource(converted); err != nil {
		return protocol.Resource{}, err
	}
	return converted, nil
}

func resourceKindToProvider(kind protocol.ResourceKind) (provider.ResourceKind, error) {
	switch kind {
	case protocol.ResourceMarkdown:
		return provider.ResourceMarkdown, nil
	case protocol.ResourceHTML:
		return provider.ResourceHTML, nil
	default:
		return "", errors.New("invalid protocol resource kind")
	}
}

func resourceKindFromProvider(kind provider.ResourceKind) (protocol.ResourceKind, error) {
	switch kind {
	case provider.ResourceMarkdown:
		return protocol.ResourceMarkdown, nil
	case provider.ResourceHTML:
		return protocol.ResourceHTML, nil
	default:
		return "", errors.New("invalid provider resource kind")
	}
}

func resourceKindToState(kind protocol.ResourceKind) (statepkg.ResourceKind, error) {
	switch kind {
	case protocol.ResourceMarkdown:
		return statepkg.ResourceMarkdown, nil
	case protocol.ResourceHTML:
		return statepkg.ResourceHTML, nil
	default:
		return "", errors.New("invalid state resource kind")
	}
}

func PageContextToProvider(context protocol.PageContext, identity ConnectIdentity, authorizedOrigin string) (provider.PageContext, error) {
	if err := identity.Validate(authorizedOrigin); err != nil {
		return provider.PageContext{}, err
	}
	if err := validateProtocolPageContext(context); err != nil {
		return provider.PageContext{}, err
	}
	if !equalProtocolResource(context.Resource, identity.Resource) || context.Digest != identity.ContextDigest {
		return provider.PageContext{}, errors.New("page context does not match the connected revision")
	}
	if err := requireSamePageOrigin(context.URL, identity.Origin); err != nil {
		return provider.PageContext{}, err
	}
	resource, err := ResourceToProvider(context.Resource)
	if err != nil {
		return provider.PageContext{}, err
	}
	converted := provider.PageContext{
		Revision:       provider.ContextRevision(context.Revision),
		Source:         []byte(context.Source),
		CreatorContext: []byte(context.CreatorContext),
		Title:          context.Title,
		URL:            context.URL,
		Resource:       resource,
		Digest:         context.Digest,
	}
	if err := converted.Validate(); err != nil {
		return provider.PageContext{}, errors.New("invalid converted page context")
	}
	return converted, nil
}

func PageContextFromProvider(context provider.PageContext, identity ConnectIdentity, authorizedOrigin string) (protocol.PageContext, error) {
	if err := identity.Validate(authorizedOrigin); err != nil {
		return protocol.PageContext{}, err
	}
	if err := context.Validate(); err != nil {
		return protocol.PageContext{}, errors.New("invalid provider page context")
	}
	resource, err := ResourceFromProvider(context.Resource)
	if err != nil {
		return protocol.PageContext{}, err
	}
	if !equalProtocolResource(resource, identity.Resource) || context.Digest != identity.ContextDigest {
		return protocol.PageContext{}, errors.New("provider page context does not match the connected revision")
	}
	if err := requireSamePageOrigin(context.URL, identity.Origin); err != nil {
		return protocol.PageContext{}, err
	}
	converted := protocol.PageContext{
		Revision:       protocol.ContextRevision(context.Revision),
		Source:         string(context.Source),
		CreatorContext: string(context.CreatorContext),
		Title:          context.Title,
		URL:            context.URL,
		Resource:       resource,
		Digest:         context.Digest,
	}
	if err := validateProtocolPageContext(converted); err != nil {
		return protocol.PageContext{}, errors.New("invalid converted page context")
	}
	return converted, nil
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	copyOfValue := make([]byte, len(value))
	copy(copyOfValue, value)
	return copyOfValue
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func validateProtocolResource(resource protocol.Resource) error {
	// Resource validation is intentionally delegated to the frozen protocol
	// validator by using its smallest valid command envelope.
	id := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, err := protocol.EncodeCommand(protocol.Command{
		APIVersion: protocol.APIVersion,
		CommandID:  id,
		ClientID:   id,
		Type:       protocol.CommandConnect,
		Payload: protocol.ConnectPayload{
			Provider:      protocol.ProviderPi,
			Resource:      resource,
			ContextDigest: strings.Repeat("0", 64),
			Settings:      nil,
		},
	})
	return err
}

func validateProtocolPageContext(context protocol.PageContext) error {
	id := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	conversationID := id
	_, err := protocol.EncodeCommand(protocol.Command{
		APIVersion:     protocol.APIVersion,
		CommandID:      id,
		ClientID:       id,
		ConversationID: &conversationID,
		Type:           protocol.CommandSubmit,
		Payload: protocol.SubmitPayload{
			TurnID:    id,
			MessageID: id,
			Content:   protocol.TextContent("message"),
			Context:   &context,
			Settings:  nil,
		},
	})
	return err
}

func messageContentToProvider(content protocol.MessageContent) (provider.MessageContent, error) {
	if content.ValidateCommand() != nil {
		return provider.MessageContent{}, errors.New("invalid protocol message content")
	}
	converted := provider.MessageContent{Parts: make([]provider.MessagePart, len(content.Parts))}
	imageOrdinal := 0
	for index, part := range content.Parts {
		switch part.Type {
		case protocol.MessagePartText:
			converted.Parts[index] = provider.MessagePart{Kind: provider.MessagePartText, Text: part.Text}
		case protocol.MessagePartReference:
			reference, err := contextReferenceToProvider(*part.Reference)
			if err != nil {
				return provider.MessageContent{}, err
			}
			if reference.Visual != nil {
				imageOrdinal++
				reference.Visual.Ordinal = imageOrdinal
			}
			converted.Parts[index] = provider.MessagePart{Kind: provider.MessagePartReference, Reference: &reference}
		case protocol.MessagePartSkill:
			if part.Skill == nil {
				return provider.MessageContent{}, errors.New("invalid skill message part")
			}
			skill := provider.SkillInvocation{ID: part.Skill.ID, Name: part.Skill.Name}
			converted.Parts[index] = provider.MessagePart{Kind: provider.MessagePartSkill, Skill: &skill}
		default:
			return provider.MessageContent{}, errors.New("invalid protocol message part")
		}
	}
	if converted.Validate() != nil {
		return provider.MessageContent{}, errors.New("invalid converted message content")
	}
	return converted, nil
}

func messageContentFromProvider(content provider.MessageContent, descriptors []protocol.ImageDescriptor) (protocol.MessageContent, error) {
	if content.Validate() != nil {
		return protocol.MessageContent{}, errors.New("invalid provider message content")
	}
	byID := make(map[string]protocol.ImageDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		if _, duplicate := byID[descriptor.ImageID]; duplicate {
			return protocol.MessageContent{}, errors.New("duplicate inline image descriptor")
		}
		byID[descriptor.ImageID] = descriptor
	}
	converted := protocol.MessageContent{Parts: make([]protocol.MessagePart, len(content.Parts))}
	for index, part := range content.Parts {
		switch part.Kind {
		case provider.MessagePartText:
			converted.Parts[index] = protocol.MessagePart{Type: protocol.MessagePartText, Text: part.Text}
		case provider.MessagePartReference:
			reference, err := contextReferenceFromProvider(*part.Reference, byID)
			if err != nil {
				return protocol.MessageContent{}, err
			}
			converted.Parts[index] = protocol.MessagePart{Type: protocol.MessagePartReference, Reference: &reference}
		case provider.MessagePartSkill:
			if part.Skill == nil {
				return protocol.MessageContent{}, errors.New("invalid provider skill message part")
			}
			skill := protocol.SkillInvocation{ID: part.Skill.ID, Name: part.Skill.Name}
			converted.Parts[index] = protocol.MessagePart{Type: protocol.MessagePartSkill, Skill: &skill}
		default:
			return protocol.MessageContent{}, errors.New("invalid provider message part")
		}
	}
	if converted.ValidateEvent() != nil {
		return protocol.MessageContent{}, errors.New("invalid converted browser message content")
	}
	return converted, nil
}

func contextReferenceToProvider(reference protocol.ContextReference) (provider.ContextReference, error) {
	converted := provider.ContextReference{
		ID: reference.ID, Kind: provider.ReferenceKind(reference.Kind), Label: reference.Label, Quote: reference.Quote, Markdown: reference.Markdown,
		Source: provider.ReferenceSource{
			ResourceKind: provider.ResourceKind(reference.Source.ResourceKind), ResourceID: reference.Source.ResourceID,
			ResourceUpdatedAt: reference.Source.ResourceUpdatedAt, ContextDigest: reference.Source.ContextDigest,
		},
	}
	if anchor := reference.Source.Anchor.Markdown; anchor != nil {
		convertedAnchor := &provider.MarkdownReferenceAnchor{
			HeadingPath: make([]provider.HeadingReference, len(anchor.HeadingPath)),
			Start:       provider.SourceAnchor{Block: anchor.Start.Block, Line: anchor.Start.Line, Offset: anchor.Start.Offset},
			End:         provider.SourceAnchor{Block: anchor.End.Block, Line: anchor.End.Line, Offset: anchor.End.Offset},
		}
		for index, heading := range anchor.HeadingPath {
			convertedAnchor.HeadingPath[index] = provider.HeadingReference{Level: heading.Level, Title: heading.Title, Ordinal: heading.Ordinal}
		}
		converted.Source.Anchor.Markdown = convertedAnchor
	}
	if anchor := reference.Source.Anchor.HTML; anchor != nil {
		converted.Source.Anchor.HTML = &provider.HTMLReferenceAnchor{ElementID: anchor.ElementID, Tag: anchor.Tag, Ordinal: anchor.Ordinal}
	}
	if reference.SectionLines != nil {
		converted.SectionLines = &provider.SourceLineRange{Start: reference.SectionLines.Start, End: reference.SectionLines.End}
	}
	if reference.Component != nil {
		converted.Component = &provider.ComponentReference{Type: provider.ComponentType(reference.Component.Type), SourceExcerpt: reference.Component.SourceExcerpt}
	}
	if reference.Visual != nil {
		converted.Visual = &provider.ReferenceVisual{ImageID: reference.Visual.ImageID, Name: reference.Visual.Name, Alt: reference.Visual.Alt}
	}
	return converted, nil
}

func contextReferenceFromProvider(reference provider.ContextReference, descriptors map[string]protocol.ImageDescriptor) (protocol.ContextReference, error) {
	converted := protocol.ContextReference{
		ID: reference.ID, Kind: protocol.ReferenceKind(reference.Kind), Label: reference.Label, Quote: reference.Quote, Markdown: reference.Markdown,
		Source: protocol.ReferenceSource{
			ResourceKind: protocol.ResourceKind(reference.Source.ResourceKind), ResourceID: reference.Source.ResourceID,
			ResourceUpdatedAt: reference.Source.ResourceUpdatedAt, ContextDigest: reference.Source.ContextDigest,
		},
	}
	if anchor := reference.Source.Anchor.Markdown; anchor != nil {
		convertedAnchor := &protocol.MarkdownReferenceAnchor{
			HeadingPath: make([]protocol.HeadingReference, len(anchor.HeadingPath)),
			Start:       protocol.SourceAnchor{Block: anchor.Start.Block, Line: anchor.Start.Line, Offset: anchor.Start.Offset},
			End:         protocol.SourceAnchor{Block: anchor.End.Block, Line: anchor.End.Line, Offset: anchor.End.Offset},
		}
		for index, heading := range anchor.HeadingPath {
			convertedAnchor.HeadingPath[index] = protocol.HeadingReference{Level: heading.Level, Title: heading.Title, Ordinal: heading.Ordinal}
		}
		converted.Source.Anchor.Markdown = convertedAnchor
	}
	if anchor := reference.Source.Anchor.HTML; anchor != nil {
		converted.Source.Anchor.HTML = &protocol.HTMLReferenceAnchor{ElementID: anchor.ElementID, Tag: anchor.Tag, Ordinal: anchor.Ordinal}
	}
	if reference.SectionLines != nil {
		converted.SectionLines = &protocol.SourceLineRange{Start: reference.SectionLines.Start, End: reference.SectionLines.End}
	}
	if reference.Component != nil {
		converted.Component = &protocol.ComponentReference{Type: protocol.ComponentType(reference.Component.Type), SourceExcerpt: reference.Component.SourceExcerpt}
	}
	if reference.Visual != nil {
		descriptor, exists := descriptors[reference.Visual.ImageID]
		if !exists || descriptor.Name != reference.Visual.Name {
			return protocol.ContextReference{}, errors.New("missing inline image descriptor")
		}
		converted.Visual = &protocol.ReferenceVisual{ImageID: descriptor.ImageID, Name: descriptor.Name, Alt: reference.Visual.Alt, MediaType: descriptor.MediaType}
	}
	return converted, nil
}

func referencesMatchCurrentPage(content provider.MessageContent, resource protocol.Resource, digest string) bool {
	for _, part := range content.Parts {
		if part.Reference == nil {
			continue
		}
		source := part.Reference.Source
		if source.ResourceKind != provider.ResourceKind(resource.Kind) || source.ResourceID != resource.ID || source.ContextDigest != digest || !source.ResourceUpdatedAt.Equal(resource.UpdatedAt) {
			return false
		}
	}
	return true
}

func equalProtocolResource(left, right protocol.Resource) bool {
	if left.Kind != right.Kind || left.ID != right.ID || left.CreatedAt != right.CreatedAt || left.UpdatedAt != right.UpdatedAt {
		return false
	}
	if left.ExpiresAt == nil || right.ExpiresAt == nil {
		return left.ExpiresAt == nil && right.ExpiresAt == nil
	}
	return *left.ExpiresAt == *right.ExpiresAt
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func requireSamePageOrigin(pageURL, expectedOrigin string) error {
	parsed, err := url.Parse(pageURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return errors.New("page URL must have an authorized same origin")
	}
	rawOrigin := parsed.Scheme + "://" + parsed.Host
	pageOrigin, err := config.CanonicalBrowserOrigin(rawOrigin)
	if err != nil || pageOrigin != expectedOrigin || (parsed.Scheme == "http" && pageOrigin != rawOrigin) {
		return errors.New("page URL must have an authorized same origin")
	}
	return nil
}
