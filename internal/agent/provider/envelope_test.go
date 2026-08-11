package provider_test

import (
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestConfiguredEnvelopeAllowsHostCapabilitiesWithoutChangingContextFraming(t *testing.T) {
	request := configuredTurn()
	configured, err := provider.Build(request, provider.PolicyConfigured)
	require.NoError(t, err)
	parsed, err := provider.Parse(configured)
	require.NoError(t, err)
	require.Equal(t, provider.PolicyConfigured, parsed.Policy)
	require.Contains(t, parsed.ApplicationInstructions, "capabilities made available by the host application")
	require.NotContains(t, parsed.ApplicationInstructions, "Do not request or use tools")
	require.Equal(t, request.TurnID, parsed.TurnID)
	require.Equal(t, request.MessageID, parsed.MessageID)
	require.Equal(t, request.Content, parsed.ReaderContent)
	require.Equal(t, request.Content.PlainText(), parsed.ReaderMessage)
	require.Equal(t, request.Context.Markdown, parsed.Markdown)
	require.Equal(t, request.Context.CreatorContext, parsed.CreatorContext)

	contentOnly, err := provider.Build(request, provider.PolicyContentOnly)
	require.NoError(t, err)
	require.NotEqual(t, configured, contentOnly)
	contentOnlyParsed, err := provider.Parse(contentOnly)
	require.NoError(t, err)
	require.Equal(t, provider.PolicyContentOnly, contentOnlyParsed.Policy)
	require.Contains(t, contentOnlyParsed.ApplicationInstructions, "Do not request or use tools")
}

func TestEnvelopeRejectsUnknownPolicyAndPreservesAssistantIdentity(t *testing.T) {
	_, err := provider.Build(configuredTurn(), provider.Policy("other"))
	require.Error(t, err)
	require.Equal(t, "stVSb4YOw9X2A6f7QhzOnTHe9i-MheoK", provider.AssistantMessageID("YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"))
}

func configuredTurn() provider.TurnRequest {
	at := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	markdown := []byte("# board\n")
	creator := []byte("creator context")
	return provider.TurnRequest{
		TurnID: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4", MessageID: "eHl6YWJjZGVmZ2hpamtsbW5vcHFyc3R1", Content: provider.TextMessage("use the available tools when useful"),
		Context: &provider.PageContext{
			Revision: provider.ContextInitial, Markdown: markdown, CreatorContext: creator, Title: "Board", URL: "https://example.test/board",
			Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFB", CreatedAt: at, UpdatedAt: at},
			Digest:   agent.CalculateContextDigest(markdown, creator),
		},
	}
}
