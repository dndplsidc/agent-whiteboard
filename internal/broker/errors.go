package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

// BrokerError is the only error representation that crosses the broker
// boundary. It contains a frozen browser code and never retains provider
// causes, native identifiers, paths, or protocol payloads.
type BrokerError struct {
	code agentprotocol.BrowserErrorCode
}

// NewBrokerError constructs a browser-safe broker error. Invalid codes produce
// the zero value and are therefore not serializable or externally useful.
func NewBrokerError(code agentprotocol.BrowserErrorCode) BrokerError {
	for _, candidate := range agentprotocol.AllBrowserErrorCodes() {
		if candidate == code {
			return BrokerError{code: code}
		}
	}
	return BrokerError{}
}

func (e BrokerError) Code() agentprotocol.BrowserErrorCode { return e.code }
func (e BrokerError) BrowserErrorCode() agentprotocol.BrowserErrorCode {
	return e.code
}
func (e BrokerError) Valid() bool {
	return e.Code() != "" && agentprotocol.NewBrowserError(e.code).Code() == e.code
}
func (e BrokerError) Error() string {
	if !e.Valid() {
		return "invalid broker error"
	}
	return agentprotocol.NewBrowserError(e.code).Message()
}

// BrowserError returns the corresponding frozen protocol value. It is useful
// at the final protocol adapter boundary; the broker error itself exposes only
// the code.
func (e BrokerError) BrowserError() agentprotocol.BrowserError {
	return agentprotocol.NewBrowserError(e.code)
}

// Is supports sentinel comparisons without exposing any underlying cause.
func (e BrokerError) Is(target error) bool {
	other, ok := target.(BrokerError)
	return ok && e.code == other.code
}

var (
	ErrQueueFull            = NewBrokerError(agentprotocol.ErrorQueueFull)
	ErrQueueItemNotFound    = errors.New("queued item not found")
	ErrQueueDuplicateID     = errors.New("duplicate queued identifier")
	ErrQueueContextConflict = errors.New("queued context already retained")
	ErrQueueInvalid         = errors.New("invalid queued turn")
	ErrReplayCursorMissing  = errors.New("replay cursor missing")
	ErrReplayCursorEvicted  = errors.New("replay cursor evicted")
	errActorRetired         = errors.New("broker actor retired")
)

// MapProviderError exhaustively translates provider failures to frozen browser
// outcomes. Unknown/zero provider values fail closed to a generic provider
// protocol failure; no error text is copied.
func MapProviderError(failure provider.ProviderError) BrokerError {
	var code agentprotocol.BrowserErrorCode
	switch failure.Code() {
	case provider.ErrorNotReady, provider.ErrorReadinessFailed:
		code = agentprotocol.ErrorProviderStartupFailed
	case provider.ErrorMissingExecutable:
		code = agentprotocol.ErrorProviderMissing
	case provider.ErrorStartupFailed:
		code = agentprotocol.ErrorProviderStartupFailed
	case provider.ErrorAuthenticationRequired:
		code = agentprotocol.ErrorAuthenticationRequired
	case provider.ErrorNoUsableModel:
		code = agentprotocol.ErrorNoUsableModel
	case provider.ErrorContentOnlyUnavailable:
		code = agentprotocol.ErrorContentOnlyUnavailable
	case provider.ErrorProtocolIncompatible, provider.ErrorProtocolFailure:
		code = agentprotocol.ErrorProviderProtocolFailure
	case provider.ErrorMalformedStream:
		code = agentprotocol.ErrorProviderMalformedStream
	case provider.ErrorChildExited:
		code = agentprotocol.ErrorProviderCrashed
	case provider.ErrorNativeSessionMissing:
		code = agentprotocol.ErrorNativeSessionMissing
	case provider.ErrorContextTooLarge:
		code = agentprotocol.ErrorContextTooLarge
	case provider.ErrorAcceptanceUnknown:
		code = agentprotocol.ErrorAcceptanceOutcomeUnknown
	default:
		code = agentprotocol.ErrorProviderProtocolFailure
	}
	return NewBrokerError(code)
}

// MapReadiness translates every non-ready provider readiness state. Ready is
// represented by a false second result rather than an error.
func MapReadiness(readiness provider.Readiness) (BrokerError, bool) {
	if err := readiness.Validate(); err != nil {
		return NewBrokerError(agentprotocol.ErrorProviderProtocolFailure), true
	}
	switch readiness.State {
	case provider.Ready:
		return BrokerError{}, false
	case provider.MissingExecutable:
		return NewBrokerError(agentprotocol.ErrorProviderMissing), true
	case provider.AuthenticationRequired:
		return NewBrokerError(agentprotocol.ErrorAuthenticationRequired), true
	case provider.StartupFailed:
		return NewBrokerError(agentprotocol.ErrorProviderStartupFailed), true
	case provider.NoUsableModel:
		return NewBrokerError(agentprotocol.ErrorNoUsableModel), true
	case provider.ContentOnlyUnavailable:
		return NewBrokerError(agentprotocol.ErrorContentOnlyUnavailable), true
	case provider.ProtocolIncompatible:
		return NewBrokerError(agentprotocol.ErrorProviderProtocolFailure), true
	default:
		return NewBrokerError(agentprotocol.ErrorProviderProtocolFailure), true
	}
}

// MapError maps only known provider errors. All other failures become the
// generic provider protocol outcome, with no interpolation of the original
// error's text.
func MapError(failure error) BrokerError {
	var providerFailure provider.ProviderError
	if errors.As(failure, &providerFailure) {
		return MapProviderError(providerFailure)
	}
	return NewBrokerError(agentprotocol.ErrorProviderProtocolFailure)
}

type browserErrorCoder interface {
	BrowserErrorCode() agentprotocol.BrowserErrorCode
}

var (
	_ error             = BrokerError{}
	_ browserErrorCoder = BrokerError{}
)
