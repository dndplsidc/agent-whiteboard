package agentprotocol

import (
	"encoding/json"
	"errors"
)

type BrowserErrorCode string
type BrowserAction string

const (
	ErrorBrokerUnavailable            BrowserErrorCode = "broker_unavailable"
	ErrorWrongPort                    BrowserErrorCode = "wrong_port"
	ErrorLocalNetworkPermissionDenied BrowserErrorCode = "local_network_permission_denied"
	ErrorUntrustedOrigin              BrowserErrorCode = "untrusted_origin"
	ErrorIncompatibleAPI              BrowserErrorCode = "incompatible_api"
	ErrorProviderMissing              BrowserErrorCode = "provider_missing"
	ErrorAuthenticationRequired       BrowserErrorCode = "authentication_required"
	ErrorNoUsableModel                BrowserErrorCode = "no_usable_model"
	ErrorProviderStartupFailed        BrowserErrorCode = "provider_startup_failed"
	ErrorContentOnlyUnavailable       BrowserErrorCode = "content_only_unavailable"
	ErrorContextTooLarge              BrowserErrorCode = "context_too_large"
	ErrorNativeSessionMissing         BrowserErrorCode = "native_session_missing"
	ErrorProviderCrashed              BrowserErrorCode = "provider_crashed"
	ErrorProviderRecoveryFailed       BrowserErrorCode = "provider_recovery_failed"
	ErrorTurnInterrupted              BrowserErrorCode = "turn_interrupted"
	ErrorBoardRevisionUnavailable     BrowserErrorCode = "board_revision_unavailable"
	ErrorBoardRevisionMalformed       BrowserErrorCode = "board_revision_malformed"
)

const (
	ActionRetryConnection   BrowserAction = "retry_connection"
	ActionEditPort          BrowserAction = "edit_port"
	ActionGrantLocalNetwork BrowserAction = "grant_local_network"
	ActionTrustOrigin       BrowserAction = "trust_origin"
	ActionUpdateBroker      BrowserAction = "update_broker"
	ActionInstallProvider   BrowserAction = "install_provider"
	ActionProviderLogin     BrowserAction = "provider_login"
	ActionTryAgain          BrowserAction = "try_again"
	ActionConfigureModel    BrowserAction = "configure_model"
	ActionRestartProvider   BrowserAction = "restart_provider"
	ActionReduceContext     BrowserAction = "reduce_context"
	ActionRestoreSession    BrowserAction = "restore_session"
	ActionRetryTurn         BrowserAction = "retry_turn"
	ActionReloadBoard       BrowserAction = "reload_board"
)

type browserErrorDefinition struct {
	message string
	action  BrowserAction
}

var browserErrorDefinitions = map[BrowserErrorCode]browserErrorDefinition{
	ErrorBrokerUnavailable:            {"The local agent broker is unavailable.", ActionRetryConnection},
	ErrorWrongPort:                    {"No compatible local agent broker was found on this port.", ActionEditPort},
	ErrorLocalNetworkPermissionDenied: {"Browser permission to reach the local agent broker was denied.", ActionGrantLocalNetwork},
	ErrorUntrustedOrigin:              {"This whiteboard origin is not trusted by the local agent broker.", ActionTrustOrigin},
	ErrorIncompatibleAPI:              {"The local agent broker uses an incompatible API version.", ActionUpdateBroker},
	ErrorProviderMissing:              {"The Pi provider executable is not available.", ActionInstallProvider},
	ErrorAuthenticationRequired:       {"Pi requires provider-native authentication.", ActionProviderLogin},
	ErrorNoUsableModel:                {"Pi has no usable default model.", ActionConfigureModel},
	ErrorProviderStartupFailed:        {"Pi could not be started.", ActionTryAgain},
	ErrorContentOnlyUnavailable:       {"Pi cannot enforce content-only access.", ActionTryAgain},
	ErrorContextTooLarge:              {"The complete page context does not fit safely in the selected model.", ActionReduceContext},
	ErrorNativeSessionMissing:         {"The provider session for this conversation is unavailable.", ActionRestoreSession},
	ErrorProviderCrashed:              {"Pi stopped unexpectedly and the active turn was interrupted.", ActionRetryTurn},
	ErrorProviderRecoveryFailed:       {"Pi could not recover the conversation.", ActionRestartProvider},
	ErrorTurnInterrupted:              {"The active turn was interrupted and was not replayed.", ActionRetryTurn},
	ErrorBoardRevisionUnavailable:     {"The current whiteboard revision is unavailable.", ActionReloadBoard},
	ErrorBoardRevisionMalformed:       {"The current whiteboard revision is malformed.", ActionReloadBoard},
}

type BrowserError struct{ code BrowserErrorCode }

func NewBrowserError(code BrowserErrorCode) BrowserError {
	if _, ok := browserErrorDefinitions[code]; !ok {
		return BrowserError{}
	}
	return BrowserError{code: code}
}
func (e BrowserError) Code() BrowserErrorCode { return e.code }
func (e BrowserError) Message() string        { return browserErrorDefinitions[e.code].message }
func (e BrowserError) Action() BrowserAction  { return browserErrorDefinitions[e.code].action }
func (e BrowserError) valid() bool            { _, ok := browserErrorDefinitions[e.code]; return ok }

func AllBrowserErrorCodes() []BrowserErrorCode {
	return []BrowserErrorCode{ErrorBrokerUnavailable, ErrorWrongPort, ErrorLocalNetworkPermissionDenied, ErrorUntrustedOrigin, ErrorIncompatibleAPI, ErrorProviderMissing, ErrorAuthenticationRequired, ErrorNoUsableModel, ErrorProviderStartupFailed, ErrorContentOnlyUnavailable, ErrorContextTooLarge, ErrorNativeSessionMissing, ErrorProviderCrashed, ErrorProviderRecoveryFailed, ErrorTurnInterrupted, ErrorBoardRevisionUnavailable, ErrorBoardRevisionMalformed}
}

func (e BrowserError) MarshalJSON() ([]byte, error) {
	if !e.valid() {
		return nil, errors.New("invalid browser error")
	}
	definition := browserErrorDefinitions[e.code]
	return json.Marshal(struct {
		Code    BrowserErrorCode `json:"code"`
		Message string           `json:"message"`
		Action  BrowserAction    `json:"action"`
	}{e.code, definition.message, definition.action})
}

func (e *BrowserError) UnmarshalJSON(data []byte) error {
	if e == nil {
		return errors.New("nil browser error")
	}
	if err := inspectJSON(data, map[string]bool{}); err != nil {
		return err
	}
	var wire struct {
		Code    BrowserErrorCode `json:"code"`
		Message string           `json:"message"`
		Action  BrowserAction    `json:"action"`
	}
	if err := strictDecode(data, &wire); err != nil {
		return err
	}
	definition, ok := browserErrorDefinitions[wire.Code]
	if !ok || wire.Message != definition.message || wire.Action != definition.action {
		return errors.New("invalid browser error")
	}
	e.code = wire.Code
	return nil
}
