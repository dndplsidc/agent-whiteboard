//go:build unix

package pi

import (
	"context"

	"github.com/edocsss/agent-whiteboard/internal/provider"
)

func (s *Session) Reconcile(ctx context.Context, reference provider.TurnReference) (provider.TurnState, error) {
	if reference.Validate() != nil {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	view, err := s.queryEntries(ctx)
	if err != nil {
		return provider.TurnUnknown, err
	}
	items, ok := deriveBrokerItems(view.entries)
	if !ok || !view.linear {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	users := 0
	assistants := 0
	aborted := false
	for _, item := range items {
		if item.item.TurnID != reference.TurnID {
			continue
		}
		if item.item.Role == provider.HistoryUser {
			users++
		} else {
			assistants++
			aborted = item.aborted
		}
	}
	if users == 0 {
		return provider.TurnNotAccepted, nil
	}
	if users != 1 {
		return provider.TurnUnknown, nil
	}
	if assistants > 1 {
		return provider.TurnUnknown, nil
	}
	if assistants == 1 {
		if aborted {
			return provider.TurnInterrupted, nil
		}
		return provider.TurnCompleted, nil
	}
	response, _, err := s.rpc.call(ctx, "get_state", nil)
	if err != nil || requireSuccessfulResponse(response, "get_state") != nil {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	var state preflightState
	if decodeStartupData(response.Data, &state) != nil || state.IsStreaming == nil || state.IsCompacting == nil || state.PendingMessageCount == nil || *state.PendingMessageCount < 0 {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	if *state.IsStreaming || *state.IsCompacting || *state.PendingMessageCount > 0 {
		return provider.TurnRunning, nil
	}
	return provider.TurnAccepted, nil
}
