package broker

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

// ConnectIdentity is the browser-visible identity needed to address one
// conversation. Origin is deliberately kept outside agentprotocol's payload:
// it is supplied by the authenticated HTTP request boundary.
type ConnectIdentity struct {
	Origin        string
	Provider      agentprotocol.ProviderName
	Resource      agentprotocol.Resource
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
func ConnectIdentityToState(identity ConnectIdentity, authorizedOrigin string) (agentstate.Identity, error) {
	if err := identity.Validate(authorizedOrigin); err != nil {
		return agentstate.Identity{}, err
	}
	name, err := providerNameToDomain(identity.Provider)
	if err != nil {
		return agentstate.Identity{}, err
	}
	return agentstate.Identity{
		Origin:       identity.Origin,
		Kind:         agentstate.ResourceMarkdown,
		CapabilityID: identity.Resource.ID,
		Provider:     name,
	}, nil
}

func providerNameToDomain(name agentprotocol.ProviderName) (provider.Name, error) {
	switch name {
	case agentprotocol.ProviderPi:
		return provider.NamePi, nil
	case agentprotocol.ProviderCodex:
		return provider.NameCodex, nil
	default:
		return "", errors.New("invalid provider")
	}
}

func providerNameFromDomain(name provider.Name) (agentprotocol.ProviderName, error) {
	switch name {
	case provider.NamePi:
		return agentprotocol.ProviderPi, nil
	case provider.NameCodex:
		return agentprotocol.ProviderCodex, nil
	default:
		return "", errors.New("invalid provider")
	}
}

// IdentityFromConnect adapts a protocol connect payload after binding the
// request's Origin. ReplayAfter is transport state and is intentionally not
// part of the durable identity.
func IdentityFromConnect(origin string, payload agentprotocol.ConnectPayload, authorizedOrigin string) (agentstate.Identity, error) {
	if payload.ReplayAfter != "" {
		if err := common.ValidateID(payload.ReplayAfter); err != nil {
			return agentstate.Identity{}, errors.New("invalid replay cursor")
		}
	}
	return ConnectIdentityToState(ConnectIdentity{Origin: origin, Provider: payload.Provider, Resource: payload.Resource, ContextDigest: payload.ContextDigest}, authorizedOrigin)
}

func ResourceToProvider(resource agentprotocol.Resource) (provider.Resource, error) {
	if err := validateProtocolResource(resource); err != nil {
		return provider.Resource{}, err
	}
	return provider.Resource{
		Kind:      provider.ResourceMarkdown,
		ID:        resource.ID,
		CreatedAt: resource.CreatedAt,
		UpdatedAt: resource.UpdatedAt,
		ExpiresAt: cloneTimePointer(resource.ExpiresAt),
	}, nil
}

func ResourceFromProvider(resource provider.Resource) (agentprotocol.Resource, error) {
	converted := agentprotocol.Resource{
		Kind:      agentprotocol.ResourceMarkdown,
		ID:        resource.ID,
		CreatedAt: resource.CreatedAt,
		UpdatedAt: resource.UpdatedAt,
		ExpiresAt: cloneTimePointer(resource.ExpiresAt),
	}
	if err := validateProtocolResource(converted); err != nil {
		return agentprotocol.Resource{}, err
	}
	if resource.Kind != provider.ResourceMarkdown {
		return agentprotocol.Resource{}, errors.New("invalid provider resource kind")
	}
	return converted, nil
}

func PageContextToProvider(context agentprotocol.PageContext, identity ConnectIdentity, authorizedOrigin string) (provider.PageContext, error) {
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
		Markdown:       []byte(context.Markdown),
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

func PageContextFromProvider(context provider.PageContext, identity ConnectIdentity, authorizedOrigin string) (agentprotocol.PageContext, error) {
	if err := identity.Validate(authorizedOrigin); err != nil {
		return agentprotocol.PageContext{}, err
	}
	if err := context.Validate(); err != nil {
		return agentprotocol.PageContext{}, errors.New("invalid provider page context")
	}
	resource, err := ResourceFromProvider(context.Resource)
	if err != nil {
		return agentprotocol.PageContext{}, err
	}
	if !equalProtocolResource(resource, identity.Resource) || context.Digest != identity.ContextDigest {
		return agentprotocol.PageContext{}, errors.New("provider page context does not match the connected revision")
	}
	if err := requireSamePageOrigin(context.URL, identity.Origin); err != nil {
		return agentprotocol.PageContext{}, err
	}
	converted := agentprotocol.PageContext{
		Revision:       agentprotocol.ContextRevision(context.Revision),
		Markdown:       string(context.Markdown),
		CreatorContext: string(context.CreatorContext),
		Title:          context.Title,
		URL:            context.URL,
		Resource:       resource,
		Digest:         context.Digest,
	}
	if err := validateProtocolPageContext(converted); err != nil {
		return agentprotocol.PageContext{}, errors.New("invalid converted page context")
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

func validateProtocolResource(resource agentprotocol.Resource) error {
	// Resource validation is intentionally delegated to the frozen protocol
	// validator by using its smallest valid command envelope.
	id := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, err := agentprotocol.EncodeCommand(agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion,
		CommandID:  id,
		ClientID:   id,
		Type:       agentprotocol.CommandConnect,
		Payload: agentprotocol.ConnectPayload{
			Provider:      agentprotocol.ProviderPi,
			Resource:      resource,
			ContextDigest: strings.Repeat("0", 64),
		},
	})
	return err
}

func validateProtocolPageContext(context agentprotocol.PageContext) error {
	id := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	conversationID := id
	_, err := agentprotocol.EncodeCommand(agentprotocol.Command{
		APIVersion:     agentprotocol.APIVersion,
		CommandID:      id,
		ClientID:       id,
		ConversationID: &conversationID,
		Type:           agentprotocol.CommandSubmit,
		Payload: agentprotocol.SubmitPayload{
			TurnID:    id,
			MessageID: id,
			Content:   agentprotocol.TextContent("message"),
			Context:   &context,
		},
	})
	return err
}

func messageContentToProvider(content agentprotocol.MessageContent) (provider.MessageContent, error) {
	if content.ValidateCommand() != nil {
		return provider.MessageContent{}, errors.New("invalid protocol message content")
	}
	converted := provider.MessageContent{Parts: make([]provider.MessagePart, len(content.Parts))}
	imageOrdinal := 0
	for index, part := range content.Parts {
		switch part.Type {
		case agentprotocol.MessagePartText:
			converted.Parts[index] = provider.MessagePart{Kind: provider.MessagePartText, Text: part.Text}
		case agentprotocol.MessagePartReference:
			reference, err := contextReferenceToProvider(*part.Reference)
			if err != nil {
				return provider.MessageContent{}, err
			}
			if reference.Visual != nil {
				imageOrdinal++
				reference.Visual.Ordinal = imageOrdinal
			}
			converted.Parts[index] = provider.MessagePart{Kind: provider.MessagePartReference, Reference: &reference}
		default:
			return provider.MessageContent{}, errors.New("invalid protocol message part")
		}
	}
	if converted.Validate() != nil {
		return provider.MessageContent{}, errors.New("invalid converted message content")
	}
	return converted, nil
}

func messageContentFromProvider(content provider.MessageContent, descriptors []agentprotocol.ImageDescriptor) (agentprotocol.MessageContent, error) {
	if content.Validate() != nil {
		return agentprotocol.MessageContent{}, errors.New("invalid provider message content")
	}
	byID := make(map[string]agentprotocol.ImageDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byID[descriptor.ImageID] = descriptor
	}
	converted := agentprotocol.MessageContent{Parts: make([]agentprotocol.MessagePart, len(content.Parts))}
	for index, part := range content.Parts {
		switch part.Kind {
		case provider.MessagePartText:
			converted.Parts[index] = agentprotocol.MessagePart{Type: agentprotocol.MessagePartText, Text: part.Text}
		case provider.MessagePartReference:
			reference, err := contextReferenceFromProvider(*part.Reference, byID)
			if err != nil {
				return agentprotocol.MessageContent{}, err
			}
			converted.Parts[index] = agentprotocol.MessagePart{Type: agentprotocol.MessagePartReference, Reference: &reference}
		default:
			return agentprotocol.MessageContent{}, errors.New("invalid provider message part")
		}
	}
	if converted.ValidateEvent() != nil {
		return agentprotocol.MessageContent{}, errors.New("invalid converted browser message content")
	}
	return converted, nil
}

func contextReferenceToProvider(reference agentprotocol.ContextReference) (provider.ContextReference, error) {
	converted := provider.ContextReference{
		ID: reference.ID, Kind: provider.ReferenceKind(reference.Kind), Label: reference.Label, Quote: reference.Quote, Markdown: reference.Markdown,
		Source: provider.ReferenceSource{
			ResourceKind: provider.ResourceKind(reference.Source.ResourceKind), ResourceID: reference.Source.ResourceID,
			ResourceUpdatedAt: reference.Source.ResourceUpdatedAt, ContextDigest: reference.Source.ContextDigest,
			Start: provider.SourceAnchor{Block: reference.Source.Start.Block, Line: reference.Source.Start.Line, Offset: reference.Source.Start.Offset},
			End:   provider.SourceAnchor{Block: reference.Source.End.Block, Line: reference.Source.End.Line, Offset: reference.Source.End.Offset},
			HeadingPath: make([]provider.HeadingReference, len(reference.Source.HeadingPath)),
		},
	}
	for index, heading := range reference.Source.HeadingPath {
		converted.Source.HeadingPath[index] = provider.HeadingReference{Level: heading.Level, Title: heading.Title, Ordinal: heading.Ordinal}
	}
	if reference.SectionLines != nil {
		converted.SectionLines = &provider.SourceLineRange{Start: reference.SectionLines.Start, End: reference.SectionLines.End}
	}
	if reference.Visual != nil {
		converted.Visual = &provider.ReferenceVisual{ImageID: reference.Visual.ImageID, Name: reference.Visual.Name, Alt: reference.Visual.Alt}
	}
	return converted, nil
}

func contextReferenceFromProvider(reference provider.ContextReference, descriptors map[string]agentprotocol.ImageDescriptor) (agentprotocol.ContextReference, error) {
	converted := agentprotocol.ContextReference{
		ID: reference.ID, Kind: agentprotocol.ReferenceKind(reference.Kind), Label: reference.Label, Quote: reference.Quote, Markdown: reference.Markdown,
		Source: agentprotocol.ReferenceSource{
			ResourceKind: agentprotocol.ResourceKind(reference.Source.ResourceKind), ResourceID: reference.Source.ResourceID,
			ResourceUpdatedAt: reference.Source.ResourceUpdatedAt, ContextDigest: reference.Source.ContextDigest,
			Start: agentprotocol.SourceAnchor{Block: reference.Source.Start.Block, Line: reference.Source.Start.Line, Offset: reference.Source.Start.Offset},
			End:   agentprotocol.SourceAnchor{Block: reference.Source.End.Block, Line: reference.Source.End.Line, Offset: reference.Source.End.Offset},
			HeadingPath: make([]agentprotocol.HeadingReference, len(reference.Source.HeadingPath)),
		},
	}
	for index, heading := range reference.Source.HeadingPath {
		converted.Source.HeadingPath[index] = agentprotocol.HeadingReference{Level: heading.Level, Title: heading.Title, Ordinal: heading.Ordinal}
	}
	if reference.SectionLines != nil {
		converted.SectionLines = &agentprotocol.SourceLineRange{Start: reference.SectionLines.Start, End: reference.SectionLines.End}
	}
	if reference.Visual != nil {
		descriptor, exists := descriptors[reference.Visual.ImageID]
		if !exists || descriptor.Name != reference.Visual.Name {
			return agentprotocol.ContextReference{}, errors.New("missing inline image descriptor")
		}
		converted.Visual = &agentprotocol.ReferenceVisual{ImageID: descriptor.ImageID, Name: descriptor.Name, Alt: reference.Visual.Alt, MediaType: descriptor.MediaType}
	}
	return converted, nil
}

func referencesMatchCurrentPage(content provider.MessageContent, resource agentprotocol.Resource, digest string) bool {
	for _, part := range content.Parts {
		if part.Reference == nil {
			continue
		}
		source := part.Reference.Source
		if source.ResourceKind != provider.ResourceMarkdown || source.ResourceID != resource.ID || source.ContextDigest != digest || !source.ResourceUpdatedAt.Equal(resource.UpdatedAt) {
			return false
		}
	}
	return true
}

func equalProtocolResource(left, right agentprotocol.Resource) bool {
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
