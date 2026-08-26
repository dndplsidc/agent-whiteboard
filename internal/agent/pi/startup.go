package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"unicode/utf8"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

type startupState struct {
	SessionID      string
	SessionFile    string
	Workspace      string
	Model          string
	ModelName      string
	ModelProvider  string
	ModelID        string
	ThinkingLevel  string
	ContextWindow  int
	MaxTokens      int
	SupportsImages bool
}

func (state startupState) valid() bool {
	return validStartupText(state.SessionID, provider.MaxNativeReferenceBytes) && validLaunchPath(state.SessionFile) && validLaunchPath(state.Workspace) &&
		validStartupText(state.ModelProvider, provider.MaxTitleBytes) && validStartupText(state.ModelID, provider.MaxTitleBytes) &&
		(state.ModelName == "" || validStartupText(state.ModelName, provider.MaxTitleBytes)) && (state.ThinkingLevel == "" || validStartupText(state.ThinkingLevel, provider.MaxEffortValueBytes)) &&
		state.Model == state.ModelProvider+"/"+state.ModelID && len(state.Model) <= provider.MaxModelValueBytes && state.ContextWindow > 0 && state.MaxTokens > 0
}

func (state startupState) settings() (provider.ExecutionSettings, provider.ModelPresentation) {
	effort := state.ThinkingLevel
	if effort == "" {
		effort = "off"
	}
	name := state.ModelName
	if name == "" {
		name = state.ModelID
	}
	return provider.ExecutionSettings{Model: state.Model, Effort: effort, Speed: provider.SpeedStandard}, provider.ModelPresentation{ModelDisplayName: name, Selectable: true}
}

type startupModel struct {
	Provider      string   `json:"provider"`
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens"`
	Input         []string `json:"input"`
}

type startupRPCState struct {
	Model               json.RawMessage `json:"model"`
	ThinkingLevel       *string         `json:"thinkingLevel"`
	IsStreaming         *bool           `json:"isStreaming"`
	IsCompacting        *bool           `json:"isCompacting"`
	PendingMessageCount *int            `json:"pendingMessageCount"`
	SessionFile         *string         `json:"sessionFile"`
	SessionID           *string         `json:"sessionId"`
}

func startup(ctx context.Context, client *rpcClient, expectedSessionFile, workspace string) (startupState, error) {
	if client == nil || !validLaunchPath(expectedSessionFile) || !validLaunchPath(workspace) || filepath.Dir(expectedSessionFile) == expectedSessionFile {
		return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	response, _, err := client.call(ctx, "get_state", nil)
	if err != nil || requireSuccessfulResponse(response, "get_state") != nil {
		return startupState{}, provider.NewProviderError(provider.ErrorStartupFailed)
	}

	var native startupRPCState
	if decodeStartupData(response.Data, &native) != nil || native.IsStreaming == nil || native.IsCompacting == nil || native.PendingMessageCount == nil || native.SessionFile == nil || native.SessionID == nil {
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
	thinkingLevel := "off"
	if native.ThinkingLevel != nil {
		thinkingLevel = *native.ThinkingLevel
	}
	modelName := model.Name
	if modelName == "" {
		modelName = model.ID
	}
	state := startupState{
		SessionID: *native.SessionID, SessionFile: *native.SessionFile, Workspace: workspace,
		ModelProvider: model.Provider, ModelID: model.ID, Model: model.Provider + "/" + model.ID, ModelName: modelName,
		ThinkingLevel: thinkingLevel, ContextWindow: model.ContextWindow, MaxTokens: model.MaxTokens,
		SupportsImages: modelSupportsImages(model.Input),
	}
	if !state.valid() || state.SessionFile != expectedSessionFile {
		return startupState{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	if err := validateSessionFile(expectedSessionFile, state.SessionID, workspace); err != nil {
		return startupState{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return state, nil
}

func modelSupportsImages(modalities []string) bool {
	for _, modality := range modalities {
		if modality == "image" {
			return true
		}
	}
	return false
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
