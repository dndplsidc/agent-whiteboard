package codex

import (
	"encoding/json"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

func (session *Session) handleSettingsUpdated(params json.RawMessage) {
	var notification struct {
		ThreadID       string                     `json:"threadId"`
		ThreadSettings map[string]json.RawMessage `json:"threadSettings"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.ThreadID != session.threadID || notification.ThreadSettings == nil {
		session.settingsProtocolFailure()
		return
	}
	modelRaw, hasModel := notification.ThreadSettings["model"]
	effortRaw, hasEffort := notification.ThreadSettings["effort"]
	tierRaw, hasTier := notification.ThreadSettings["serviceTier"]
	var model string
	if !hasModel || !hasEffort || !hasTier || json.Unmarshal(modelRaw, &model) != nil || model == "" {
		session.settingsProtocolFailure()
		return
	}
	effort := ""
	if !isJSONNull(effortRaw) && json.Unmarshal(effortRaw, &effort) != nil {
		session.settingsProtocolFailure()
		return
	}
	tier := ""
	if !isJSONNull(tierRaw) && json.Unmarshal(tierRaw, &tier) != nil {
		session.settingsProtocolFailure()
		return
	}
	session.mu.Lock()
	generation := session.settingsGeneration
	session.mu.Unlock()
	catalog := session.currentCatalog()
	settings, presentation, capabilities, err := catalog.resolveEffective(model, effort, tier)
	if err != nil {
		session.settingsProtocolFailure()
		return
	}
	now := session.driver.config.Clock.Now().UTC()
	if now.IsZero() {
		session.settingsProtocolFailure()
		return
	}
	session.mu.Lock()
	if session.closed || session.settingsGeneration != generation {
		session.mu.Unlock()
		return
	}
	if session.settingsUnverified && session.rerouteTarget != "" && settings.Model != session.rerouteTarget {
		session.mu.Unlock()
		return
	}
	turnID := ""
	if session.active != nil {
		turnID = session.active.request.TurnID
	}
	copyOfSettings := settings
	copyOfPresentation := presentation
	session.native.Model = settings.Model
	session.native.Settings = &copyOfSettings
	session.native.Presentation = &copyOfPresentation
	session.native.UpdatedAt = now
	session.capabilities = capabilities
	session.catalog = catalog.clone()
	session.settingsUnverified = false
	session.rerouteTarget = ""
	session.mu.Unlock()
	session.emit(provider.NewVerifiedSettingsEvent(turnID, settings, presentation))
}

func (session *Session) handleModelRerouted(params json.RawMessage) {
	var notification struct {
		ThreadID  string `json:"threadId"`
		TurnID    string `json:"turnId"`
		FromModel string `json:"fromModel"`
		ToModel   string `json:"toModel"`
		Reason    string `json:"reason"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.ThreadID != session.threadID || notification.TurnID == "" ||
		!validCatalogText(notification.FromModel, provider.MaxModelValueBytes, true) || !validCatalogText(notification.ToModel, provider.MaxModelValueBytes, true) || notification.Reason != "highRiskCyberActivity" {
		session.settingsProtocolFailure()
		return
	}
	catalog := session.currentCatalog()
	target := notification.ToModel
	if canonical := catalog.aliases[target]; canonical != "" {
		target = canonical
	}
	session.mu.Lock()
	if session.closed || session.active == nil || session.active.nativeID != notification.TurnID {
		session.mu.Unlock()
		return
	}
	turnID := session.active.request.TurnID
	session.settingsGeneration++
	session.settingsUnverified = true
	session.rerouteTarget = target
	session.mu.Unlock()
	session.emit(provider.NewUnverifiedSettingsEvent(turnID))
}

func (session *Session) settingsProtocolFailure() {
	session.mu.Lock()
	turnID := ""
	if session.active != nil {
		turnID = session.active.request.TurnID
	}
	session.settingsUnverified = true
	session.settingsGeneration++
	session.closed = true
	session.mu.Unlock()
	session.emit(provider.NewTerminalFailureEvent(turnID, provider.NewProviderError(provider.ErrorProtocolIncompatible)))
}
