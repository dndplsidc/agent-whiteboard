package protocol

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
	ErrorInvalidCommand               BrowserErrorCode = "invalid_command"
	ErrorInvalidState                 BrowserErrorCode = "invalid_state"
	ErrorQueueFull                    BrowserErrorCode = "queue_full"
	ErrorActiveTurnConflict           BrowserErrorCode = "active_turn_conflict"
	ErrorStaleReference               BrowserErrorCode = "stale_reference"
	ErrorReplayWindowUnavailable      BrowserErrorCode = "replay_window_unavailable"
	ErrorStateRepairFailed            BrowserErrorCode = "state_repair_failed"
	ErrorArchiveDeleteRetained        BrowserErrorCode = "archive_delete_retained"
	ErrorBrokerShuttingDown           BrowserErrorCode = "broker_shutting_down"
	ErrorProviderProtocolFailure      BrowserErrorCode = "provider_protocol_failure"
	ErrorProviderMalformedStream      BrowserErrorCode = "provider_malformed_stream"
	ErrorAcceptanceOutcomeUnknown     BrowserErrorCode = "acceptance_outcome_unknown"
	ErrorImageInputUnsupported        BrowserErrorCode = "image_input_unsupported"
	ErrorImageUnsupported             BrowserErrorCode = "image_unsupported"
	ErrorImageTooLarge                BrowserErrorCode = "image_too_large"
	ErrorImageTurnLimit               BrowserErrorCode = "image_turn_limit"
	ErrorImageWorkspaceLimit          BrowserErrorCode = "image_workspace_limit"
	ErrorImageMissing                 BrowserErrorCode = "image_missing"
	ErrorImageStorageFailure          BrowserErrorCode = "image_storage_failure"

	// Compatibility names describe the same frozen wire outcomes.
	ErrorActiveTurnBusy      = ErrorActiveTurnConflict
	ErrorReplayUnavailable   = ErrorReplayWindowUnavailable
	ErrorArchiveDeleteRetry  = ErrorArchiveDeleteRetained
	ErrorArchiveDeleteFailed = ErrorArchiveDeleteRetained
	ErrorAcceptanceUnknown   = ErrorAcceptanceOutcomeUnknown
)

const (
	ActionNone               BrowserAction = "none"
	ActionRetryConnection    BrowserAction = "retry_connection"
	ActionEditPort           BrowserAction = "edit_port"
	ActionGrantLocalNetwork  BrowserAction = "grant_local_network"
	ActionTrustOrigin        BrowserAction = "trust_origin"
	ActionUpdateBroker       BrowserAction = "update_broker"
	ActionInstallProvider    BrowserAction = "install_provider"
	ActionProviderLogin      BrowserAction = "provider_login"
	ActionTryAgain           BrowserAction = "try_again"
	ActionConfigureModel     BrowserAction = "configure_model"
	ActionRestartProvider    BrowserAction = "restart_provider"
	ActionReduceContext      BrowserAction = "reduce_context"
	ActionRestoreSession     BrowserAction = "restore_session"
	ActionRetryTurn          BrowserAction = "retry_turn"
	ActionReloadBoard        BrowserAction = "reload_board"
	ActionRefreshState       BrowserAction = "refresh_state"
	ActionEditQueue          BrowserAction = "edit_queue"
	ActionWaitForTurn        BrowserAction = "wait_for_turn"
	ActionReloadConversation BrowserAction = "reload_conversation"
	ActionRetryArchiveDelete BrowserAction = "retry_archive_delete"
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
	ErrorProviderMissing:              {"The selected provider executable is not available.", ActionInstallProvider},
	ErrorAuthenticationRequired:       {"The selected provider requires provider-native authentication.", ActionProviderLogin},
	ErrorNoUsableModel:                {"The selected provider has no usable default model.", ActionConfigureModel},
	ErrorProviderStartupFailed:        {"The selected provider could not be started.", ActionTryAgain},
	ErrorContentOnlyUnavailable:       {"The selected provider cannot enforce the required access policy.", ActionTryAgain},
	ErrorContextTooLarge:              {"The complete page context does not fit safely in the selected model.", ActionReduceContext},
	ErrorNativeSessionMissing:         {"The provider session for this conversation is unavailable.", ActionRestoreSession},
	ErrorProviderCrashed:              {"The selected provider stopped unexpectedly and the active turn was interrupted.", ActionRetryTurn},
	ErrorProviderRecoveryFailed:       {"The selected provider could not recover the conversation.", ActionRestartProvider},
	ErrorTurnInterrupted:              {"The active turn was interrupted and was not replayed.", ActionRetryTurn},
	ErrorBoardRevisionUnavailable:     {"The current whiteboard revision is unavailable.", ActionReloadBoard},
	ErrorBoardRevisionMalformed:       {"The current whiteboard revision is malformed.", ActionReloadBoard},
	ErrorInvalidCommand:               {"The broker rejected an invalid command.", ActionNone},
	ErrorInvalidState:                 {"The command is not valid for the current conversation state.", ActionRefreshState},
	ErrorQueueFull:                    {"The follow-up queue is full.", ActionEditQueue},
	ErrorActiveTurnConflict:           {"Another turn is already active for this conversation.", ActionWaitForTurn},
	ErrorStaleReference:               {"The referenced conversation item is no longer current.", ActionRefreshState},
	ErrorReplayWindowUnavailable:      {"The requested replay window is no longer available.", ActionReloadConversation},
	ErrorStateRepairFailed:            {"The broker could not repair the saved conversation state.", ActionTryAgain},
	ErrorArchiveDeleteRetained:        {"The archive was retained because provider deletion did not complete.", ActionRetryArchiveDelete},
	ErrorBrokerShuttingDown:           {"The local agent broker is shutting down.", ActionRetryConnection},
	ErrorProviderProtocolFailure:      {"The provider protocol operation failed.", ActionRestartProvider},
	ErrorProviderMalformedStream:      {"The provider returned a malformed event stream.", ActionRestartProvider},
	ErrorAcceptanceOutcomeUnknown:     {"The provider turn acceptance outcome is unknown.", ActionRefreshState},
	ErrorImageInputUnsupported:        {"The selected model does not support image input.", ActionConfigureModel},
	ErrorImageUnsupported:             {"The selected file is not a supported image.", ActionNone},
	ErrorImageTooLarge:                {"The selected image is too large.", ActionNone},
	ErrorImageTurnLimit:               {"The message has too many or too much image data.", ActionNone},
	ErrorImageWorkspaceLimit:          {"This conversation has reached its image storage limit.", ActionNone},
	ErrorImageMissing:                 {"The selected image is no longer available.", ActionNone},
	ErrorImageStorageFailure:          {"The selected image could not be stored safely.", ActionTryAgain},
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
	return []BrowserErrorCode{
		ErrorBrokerUnavailable, ErrorWrongPort, ErrorLocalNetworkPermissionDenied, ErrorUntrustedOrigin,
		ErrorIncompatibleAPI, ErrorProviderMissing, ErrorAuthenticationRequired, ErrorNoUsableModel,
		ErrorProviderStartupFailed, ErrorContentOnlyUnavailable, ErrorContextTooLarge, ErrorNativeSessionMissing,
		ErrorProviderCrashed, ErrorProviderRecoveryFailed, ErrorTurnInterrupted, ErrorBoardRevisionUnavailable,
		ErrorBoardRevisionMalformed, ErrorInvalidCommand, ErrorInvalidState, ErrorQueueFull, ErrorActiveTurnConflict,
		ErrorStaleReference, ErrorReplayWindowUnavailable, ErrorStateRepairFailed, ErrorArchiveDeleteRetained,
		ErrorBrokerShuttingDown, ErrorProviderProtocolFailure, ErrorProviderMalformedStream, ErrorAcceptanceOutcomeUnknown,
		ErrorImageInputUnsupported, ErrorImageUnsupported, ErrorImageTooLarge, ErrorImageTurnLimit,
		ErrorImageWorkspaceLimit, ErrorImageMissing, ErrorImageStorageFailure,
	}
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
