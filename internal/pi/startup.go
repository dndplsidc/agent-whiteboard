package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type startupState struct {
	SessionID     string
	SessionFile   string
	Workspace     string
	Model         string
	ModelProvider string
	ModelID       string
	ContextWindow int
	MaxTokens     int
}

func (state startupState) valid() bool {
	return validStartupText(state.SessionID, provider.MaxNativeReferenceBytes) && validLaunchPath(state.SessionFile) && validLaunchPath(state.Workspace) &&
		validStartupText(state.ModelProvider, provider.MaxTitleBytes) && validStartupText(state.ModelID, provider.MaxTitleBytes) &&
		state.Model == state.ModelProvider+"/"+state.ModelID && len(state.Model) <= provider.MaxTitleBytes && state.ContextWindow > 0 && state.MaxTokens > 0
}

type startupModel struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	ContextWindow int    `json:"contextWindow"`
	MaxTokens     int    `json:"maxTokens"`
}

type startupRPCState struct {
	Model               json.RawMessage `json:"model"`
	IsStreaming         *bool           `json:"isStreaming"`
	IsCompacting        *bool           `json:"isCompacting"`
	PendingMessageCount *int            `json:"pendingMessageCount"`
	SessionFile         *string         `json:"sessionFile"`
	SessionID           *string         `json:"sessionId"`
}

type startupCommand struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type startupCommands struct {
	Commands []startupCommand `json:"commands"`
}

type startupAnswer struct {
	command  string
	response rpcResponse
	err      error
}

func startup(ctx context.Context, client *rpcClient, expectedSessionFile, workspace string) (startupState, error) {
	if client == nil || !validLaunchPath(expectedSessionFile) || !validLaunchPath(workspace) || filepath.Dir(expectedSessionFile) == expectedSessionFile {
		return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	if startupHasEvent(client) {
		return startupState{}, provider.NewProviderError(provider.ErrorContentOnlyUnavailable)
	}
	answers := make(chan startupAnswer, 2)
	for _, command := range []string{"get_state", "get_commands"} {
		command := command
		go func() {
			response, _, err := client.call(ctx, command, nil)
			answers <- startupAnswer{command: command, response: response, err: err}
		}()
	}
	responses := make(map[string]rpcResponse, 2)
	for range 2 {
		select {
		case answer := <-answers:
			if answer.err != nil || requireSuccessfulResponse(answer.response, answer.command) != nil {
				return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
			}
			responses[answer.command] = answer.response
		case <-ctx.Done():
			return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
		case _, ok := <-client.events:
			if !ok {
				return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
			}
			return startupState{}, provider.NewProviderError(provider.ErrorContentOnlyUnavailable)
		}
	}
	if startupHasEvent(client) {
		return startupState{}, provider.NewProviderError(provider.ErrorContentOnlyUnavailable)
	}
	response, _, err := client.call(ctx, "set_auto_retry", map[string]any{"enabled": false})
	if err != nil || requireSuccessfulResponse(response, "set_auto_retry") != nil {
		return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	if startupHasEvent(client) {
		return startupState{}, provider.NewProviderError(provider.ErrorContentOnlyUnavailable)
	}

	var commands startupCommands
	if decodeStartupData(responses["get_commands"].Data, &commands) != nil || !contentOnlyCommands(commands.Commands) {
		return startupState{}, provider.NewProviderError(provider.ErrorContentOnlyUnavailable)
	}
	var native startupRPCState
	if decodeStartupData(responses["get_state"].Data, &native) != nil || native.IsStreaming == nil || native.IsCompacting == nil || native.PendingMessageCount == nil || native.SessionFile == nil || native.SessionID == nil {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	if *native.IsStreaming || *native.IsCompacting || *native.PendingMessageCount != 0 {
		return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	if bytes.Equal(bytes.TrimSpace(native.Model), []byte("null")) || len(bytes.TrimSpace(native.Model)) == 0 {
		return startupState{}, provider.NewProviderError(provider.ErrorNoUsableModel)
	}
	var model startupModel
	if decodeStartupData(native.Model, &model) != nil || !validStartupText(model.Provider, provider.MaxTitleBytes) || !validStartupText(model.ID, provider.MaxTitleBytes) || model.ContextWindow <= 0 || model.MaxTokens <= 0 {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	state := startupState{
		SessionID: *native.SessionID, SessionFile: *native.SessionFile, Workspace: workspace,
		ModelProvider: model.Provider, ModelID: model.ID, Model: model.Provider + "/" + model.ID,
		ContextWindow: model.ContextWindow, MaxTokens: model.MaxTokens,
	}
	if !state.valid() || state.SessionFile != expectedSessionFile {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	if err := validateSessionFile(expectedSessionFile, state.SessionID, workspace); err != nil {
		return startupState{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	if startupHasEvent(client) {
		return startupState{}, provider.NewProviderError(provider.ErrorContentOnlyUnavailable)
	}
	return state, nil
}

func contentOnlyCommands(commands []startupCommand) bool {
	if commands == nil || len(commands) == 0 {
		return commands != nil
	}
	// Pi 0.82.1 always registers its bundled /llama command even under
	// --no-extensions. In RPC mode that handler only reports that it is TUI-only
	// and cannot access its model-management client. No discovered command is
	// permitted.
	return len(commands) == 1 && commands[0].Name == "llama" && commands[0].Source == "extension"
}

func decodeStartupData(data json.RawMessage, destination any) error {
	if len(data) == 0 || len(data) > maxRPCRecordBytes {
		return errors.New("invalid Pi startup response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid Pi startup response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("invalid Pi startup response")
	}
	return nil
}

func startupHasEvent(client *rpcClient) bool {
	select {
	case _, ok := <-client.events:
		return ok
	default:
		return false
	}
}

func validStartupText(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
