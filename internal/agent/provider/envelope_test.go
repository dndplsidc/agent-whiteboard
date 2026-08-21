package provider_test

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestEnvelopeV3UsesFormatNeutralFramingAndParsesHTML(t *testing.T) {
	request := configuredTurn()
	request.Context.Resource.Kind = provider.ResourceHTML
	request.Context.Source = []byte("<!doctype html><title>Exact</title>")
	request.Context.Digest, _ = agent.CalculateContextDigestForKind(agent.ResourceKindHTML, request.Context.Source, request.Context.CreatorContext)
	encoded, err := provider.Build(request, provider.PolicyConfigured)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(encoded, []byte("agent-whiteboard-turn-v3\n")))
	require.Contains(t, string(encoded), "page-source-untrusted")
	require.NotContains(t, string(encoded), "markdown-source-untrusted")
	parsed, err := provider.Parse(encoded)
	require.NoError(t, err)
	require.Equal(t, string(provider.ResourceHTML), parsed.ResourceKind)
	require.Equal(t, request.Context.Source, parsed.Source)
	require.Contains(t, parsed.ApplicationInstructions, "page source")
}

func TestEnvelopeParsesHistoricalV1Markdown(t *testing.T) {
	encoded, err := os.ReadFile("testdata/turn-v1-initial.envelope")
	require.NoError(t, err)
	parsed, err := provider.Parse(encoded)
	require.NoError(t, err)
	require.Equal(t, provider.PolicyConfigured, parsed.Policy)
	require.Equal(t, string(provider.ResourceMarkdown), parsed.ResourceKind)
	require.Equal(t, "Board", parsed.PageTitle)
	require.Equal(t, "https://example.test/board", parsed.PageURL)
	require.Equal(t, []byte("# board\n"), parsed.Source)
	require.Equal(t, []byte("creator context"), parsed.CreatorContext)
	require.Equal(t, "use the available tools when useful", parsed.ReaderMessage)
	require.Equal(t, provider.TextMessage("use the available tools when useful"), parsed.ReaderContent)
	require.Contains(t, parsed.ApplicationInstructions, "reader message")
}

func TestEnvelopeParsesHistoricalV2Markdown(t *testing.T) {
	encoded, err := os.ReadFile("testdata/turn-v2-initial.envelope")
	require.NoError(t, err)
	parsed, err := provider.Parse(encoded)
	require.NoError(t, err)
	require.Equal(t, provider.PolicyConfigured, parsed.Policy)
	require.Equal(t, string(provider.ResourceMarkdown), parsed.ResourceKind)
	require.Equal(t, []byte("# board\n"), parsed.Source)
	require.Equal(t, provider.TextMessage("use the available tools when useful"), parsed.ReaderContent)
}

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
	require.Equal(t, request.Context.Source, parsed.Source)
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
	turnID := "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"
	require.Equal(t, "stVSb4YOw9X2A6f7QhzOnTHe9i-MheoK", provider.AssistantMessageID(turnID))

	first := provider.AssistantItemMessageID(turnID, "native-agent-item-1")
	require.Equal(t, first, provider.AssistantItemMessageID(turnID, "native-agent-item-1"))
	require.NotEqual(t, first, provider.AssistantItemMessageID(turnID, "native-agent-item-2"))
	require.NotEqual(t, first, provider.AssistantMessageID(turnID))
	require.Len(t, first, 32)
	require.NotContains(t, first, "native-agent-item")
}

func configuredTurn() provider.TurnRequest {
	at := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	markdown := []byte("# board\n")
	creator := []byte("creator context")
	return provider.TurnRequest{
		TurnID: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4", MessageID: "eHl6YWJjZGVmZ2hpamtsbW5vcHFyc3R1", Content: provider.TextMessage("use the available tools when useful"),
		Context: &provider.PageContext{
			Revision: provider.ContextInitial, Source: markdown, CreatorContext: creator, Title: "Board", URL: "https://example.test/board",
			Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFB", CreatedAt: at, UpdatedAt: at},
			Digest:   agent.CalculateContextDigest(markdown, creator),
		},
	}
}
