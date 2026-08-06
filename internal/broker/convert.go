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
			Message:   "message",
			Context:   &context,
		},
	})
	return err
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
