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
	Origin   string
	Provider agentprotocol.ProviderName
	Resource agentprotocol.Resource
}

func (identity ConnectIdentity) Validate(authorizedOrigin string) error {
	canonical, err := config.CanonicalOrigin(identity.Origin)
	if err != nil || canonical != identity.Origin {
		return errors.New("invalid connect origin")
	}
	authorized, err := config.CanonicalOrigin(authorizedOrigin)
	if err != nil || authorized != authorizedOrigin || authorized != identity.Origin {
		return errors.New("connect origin is not the authorized canonical origin")
	}
	if identity.Provider != agentprotocol.ProviderPi || validateProtocolResource(identity.Resource) != nil {
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
	return agentstate.Identity{
		Origin:       identity.Origin,
		Kind:         agentstate.ResourceMarkdown,
		CapabilityID: identity.Resource.ID,
		Provider:     provider.NamePi,
	}, nil
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
	return ConnectIdentityToState(ConnectIdentity{Origin: origin, Provider: payload.Provider, Resource: payload.Resource}, authorizedOrigin)
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
	if err := requireSameOriginHTTPS(context.URL, identity.Origin); err != nil {
		return provider.PageContext{}, err
	}
	resource, err := ResourceToProvider(context.Resource)
	if err != nil {
		return provider.PageContext{}, err
	}
	converted := provider.PageContext{
		Revision:       provider.ContextRevision(context.Revision),
		Markdown:       cloneBytes([]byte(context.Markdown)),
		CreatorContext: cloneBytes([]byte(context.CreatorContext)),
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
	if err := requireSameOriginHTTPS(context.URL, identity.Origin); err != nil {
		return agentprotocol.PageContext{}, err
	}
	resource, err := ResourceFromProvider(context.Resource)
	if err != nil {
		return agentprotocol.PageContext{}, err
	}
	converted := agentprotocol.PageContext{
		Revision:       agentprotocol.ContextRevision(context.Revision),
		Markdown:       string(cloneBytes(context.Markdown)),
		CreatorContext: string(cloneBytes(context.CreatorContext)),
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

// ConvertPageContext aliases the direction used when handing browser context
// to a provider. The explicit directional functions above should be preferred
// where the ownership transfer is important.
func ConvertPageContext(context agentprotocol.PageContext, identity ConnectIdentity, authorizedOrigin string) (provider.PageContext, error) {
	return PageContextToProvider(context, identity, authorizedOrigin)
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

func requireSameOriginHTTPS(pageURL, expectedOrigin string) error {
	parsed, err := url.Parse(pageURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return errors.New("page URL must be HTTPS and same-origin")
	}
	pageOrigin, err := config.CanonicalOrigin("https://" + parsed.Host)
	if err != nil || pageOrigin != expectedOrigin {
		return errors.New("page URL must be HTTPS and same-origin")
	}
	return nil
}
