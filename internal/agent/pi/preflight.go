//go:build unix

package pi

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type preflightState struct {
	Model               json.RawMessage `json:"model"`
	IsStreaming         *bool           `json:"isStreaming"`
	IsCompacting        *bool           `json:"isCompacting"`
	PendingMessageCount *int            `json:"pendingMessageCount"`
	MessageCount        *int            `json:"messageCount"`
	SessionFile         *string         `json:"sessionFile"`
	SessionID           *string         `json:"sessionId"`
}

type preflightStats struct {
	ContextUsage json.RawMessage `json:"contextUsage"`
}
type contextUsage struct {
	Tokens        *int `json:"tokens"`
	ContextWindow *int `json:"contextWindow"`
}

func (s *Session) Preflight(ctx context.Context, request provider.PreflightRequest) (provider.PreflightResult, error) {
	if request.Validate() != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	captured := s.state
	s.mu.Unlock()
	targetModel := piModel{provider: captured.ModelProvider, id: captured.ModelID, name: captured.ModelName, images: captured.SupportsImages, contextWindow: captured.ContextWindow, maxTokens: captured.MaxTokens}
	resolved := captured.Model
	if request.Turn.Settings != nil {
		prepared, err := s.prepareSettings(ctx, *request.Turn.Settings)
		if err != nil {
			return provider.PreflightResult{}, err
		}
		targetModel = prepared.model
		resolved = request.Turn.Settings.Model
	}
	if len(request.Turn.Images) != 0 && !targetModel.images {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorImageInputUnsupported)
	}
	envelope, err := BuildEnvelope(request.Turn)
	if err != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	defer wipe(envelope)
	stateResponse, _, err := s.rpc.call(ctx, "get_state", nil)
	if err != nil || requireSuccessfulResponse(stateResponse, "get_state") != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	var state preflightState
	if decodeStartupData(stateResponse.Data, &state) != nil || state.IsStreaming == nil || state.IsCompacting == nil || state.PendingMessageCount == nil || state.MessageCount == nil || state.SessionFile == nil || state.SessionID == nil || *state.MessageCount < 0 || *state.IsStreaming || *state.IsCompacting || *state.PendingMessageCount != 0 || *state.SessionFile != captured.SessionFile || *state.SessionID != captured.SessionID {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	var model startupModel
	if decodeStartupData(state.Model, &model) != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	currentResolved := model.Provider + "/" + model.ID
	if !validModelModalities(model.Input) || currentResolved != captured.Model || model.ContextWindow != captured.ContextWindow || model.MaxTokens != captured.MaxTokens || modelSupportsImages(model.Input) != captured.SupportsImages {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	statsResponse, _, err := s.rpc.call(ctx, "get_session_stats", nil)
	if err != nil || requireSuccessfulResponse(statsResponse, "get_session_stats") != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	var stats preflightStats
	if decodeStartupData(statsResponse.Data, &stats) != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	messageCount := *state.MessageCount
	nativeTokens := 0
	trimmed := bytes.TrimSpace(stats.ContextUsage)
	if messageCount == 0 {
		if len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null")) {
			var usage contextUsage
			if decodeStartupData(stats.ContextUsage, &usage) != nil || usage.Tokens == nil || *usage.Tokens != 0 || (usage.ContextWindow != nil && *usage.ContextWindow != model.ContextWindow) {
				return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
			}
		}
		nativeTokens = 0
	} else {
		var usage contextUsage
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || decodeStartupData(stats.ContextUsage, &usage) != nil || usage.Tokens == nil || *usage.Tokens <= 0 || (usage.ContextWindow != nil && *usage.ContextWindow != model.ContextWindow) {
			return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		nativeTokens = *usage.Tokens
	}
	safety := targetModel.maxTokens
	if safety < 16384 {
		safety = 16384
	}
	effective := targetModel.contextWindow - safety
	estimated := nativeTokens + conservativeTokenBound(len(envelope))
	if effective <= 0 || estimated > effective {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorContextTooLarge)
	}
	result := provider.PreflightResult{ResolvedModel: resolved, EstimatedInputTokens: estimated, EffectiveCapacityTokens: effective, SafetyMarginTokens: safety}
	if result.Validate() != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return result, nil
}

// A tokenizer cannot produce more tokens than the number of input bytes when
// every fallback token consumes at least one byte. This intentionally
// overestimates ordinary text instead of relying on a bytes/4 average.
func conservativeTokenBound(bytes int) int { return bytes }
