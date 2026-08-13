package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

// BrokerError is the only error representation that crosses the broker
// boundary. It contains a frozen browser code and never retains provider
// causes, native identifiers, paths, or protocol payloads.
type BrokerError struct {
	code protocol.BrowserErrorCode
}

// NewBrokerError constructs a browser-safe broker error. Invalid codes produce
// the zero value and are therefore not serializable or externally useful.
func NewBrokerError(code protocol.BrowserErrorCode) BrokerError {
	for _, candidate := range protocol.AllBrowserErrorCodes() {
		if candidate == code {
			return BrokerError{code: code}
		}
	}
	return BrokerError{}
}

func (e BrokerError) Code() protocol.BrowserErrorCode { return e.code }
func (e BrokerError) BrowserErrorCode() protocol.BrowserErrorCode {
	return e.code
}
func (e BrokerError) Valid() bool {
	return e.Code() != "" && protocol.NewBrowserError(e.code).Code() == e.code
}
func (e BrokerError) Error() string {
	if !e.Valid() {
		return "invalid broker error"
	}
	return protocol.NewBrowserError(e.code).Message()
}

// BrowserError returns the corresponding frozen protocol value. It is useful
// at the final protocol adapter boundary; the broker error itself exposes only
// the code.
func (e BrokerError) BrowserError() protocol.BrowserError {
	return protocol.NewBrowserError(e.code)
}

// Is supports sentinel comparisons without exposing any underlying cause.
func (e BrokerError) Is(target error) bool {
	other, ok := target.(BrokerError)
	return ok && e.code == other.code
}

var (
	ErrQueueFull            = NewBrokerError(protocol.ErrorQueueFull)
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
	var code protocol.BrowserErrorCode
	switch failure.Code() {
	case provider.ErrorNotReady, provider.ErrorReadinessFailed:
		code = protocol.ErrorProviderStartupFailed
	case provider.ErrorMissingExecutable:
		code = protocol.ErrorProviderMissing
	case provider.ErrorStartupFailed:
		code = protocol.ErrorProviderStartupFailed
	case provider.ErrorAuthenticationRequired:
		code = protocol.ErrorAuthenticationRequired
	case provider.ErrorNoUsableModel:
		code = protocol.ErrorNoUsableModel
	case provider.ErrorContentOnlyUnavailable:
		code = protocol.ErrorContentOnlyUnavailable
	case provider.ErrorProtocolIncompatible, provider.ErrorProtocolFailure:
		code = protocol.ErrorProviderProtocolFailure
	case provider.ErrorMalformedStream:
		code = protocol.ErrorProviderMalformedStream
	case provider.ErrorChildExited:
		code = protocol.ErrorProviderCrashed
	case provider.ErrorNativeSessionMissing:
		code = protocol.ErrorNativeSessionMissing
	case provider.ErrorContextTooLarge:
		code = protocol.ErrorContextTooLarge
	case provider.ErrorAcceptanceUnknown:
		code = protocol.ErrorAcceptanceOutcomeUnknown
	case provider.ErrorInvalidModelConfiguration:
		code = protocol.ErrorInvalidModelConfiguration
	case provider.ErrorImageInputUnsupported:
		code = protocol.ErrorImageInputUnsupported
	case provider.ErrorImageUnsupported:
		code = protocol.ErrorImageUnsupported
	case provider.ErrorImageTooLarge:
		code = protocol.ErrorImageTooLarge
	case provider.ErrorImageTurnLimit:
		code = protocol.ErrorImageTurnLimit
	case provider.ErrorImageMissing:
		code = protocol.ErrorImageMissing
	case provider.ErrorImageStorageFailure:
		code = protocol.ErrorImageStorageFailure
	default:
		code = protocol.ErrorProviderProtocolFailure
	}
	return NewBrokerError(code)
}

// MapReadiness translates every non-ready provider readiness state. Ready is
// represented by a false second result rather than an error.
func MapReadiness(readiness provider.Readiness) (BrokerError, bool) {
	if err := readiness.Validate(); err != nil {
		return NewBrokerError(protocol.ErrorProviderProtocolFailure), true
	}
	switch readiness.State {
	case provider.Ready:
		return BrokerError{}, false
	case provider.MissingExecutable:
		return NewBrokerError(protocol.ErrorProviderMissing), true
	case provider.AuthenticationRequired:
		return NewBrokerError(protocol.ErrorAuthenticationRequired), true
	case provider.StartupFailed:
		return NewBrokerError(protocol.ErrorProviderStartupFailed), true
	case provider.NoUsableModel:
		return NewBrokerError(protocol.ErrorNoUsableModel), true
	case provider.ContentOnlyUnavailable:
		return NewBrokerError(protocol.ErrorContentOnlyUnavailable), true
	case provider.ProtocolIncompatible:
		return NewBrokerError(protocol.ErrorProviderProtocolFailure), true
	default:
		return NewBrokerError(protocol.ErrorProviderProtocolFailure), true
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
	return NewBrokerError(protocol.ErrorProviderProtocolFailure)
}

type browserErrorCoder interface {
	BrowserErrorCode() protocol.BrowserErrorCode
}

var (
	_ error             = BrokerError{}
	_ browserErrorCoder = BrokerError{}
)
