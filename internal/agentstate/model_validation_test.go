package agentstate

import (
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func TestExportedMappingAndSessionValidationUseCompleteInvariants(t *testing.T) {
	identity := testIdentity()
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	session := testSession(t, testID, "sessions/current", at)
	mapping := Mapping{SchemaVersion: SchemaVersion, Identity: identity, Current: &session, Archives: []Session{}, CreatedAt: at, UpdatedAt: at}
	require.NoError(t, session.Validate())
	require.NoError(t, mapping.Validate(identity))

	wrong := identity
	wrong.CapabilityID = "emJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"
	require.Error(t, mapping.Validate(wrong))

	invalidSession := session
	invalidSession.ModelLabel = string([]byte{0})
	require.Error(t, invalidSession.Validate())

	invalidMapping := mapping
	invalidMapping.SchemaVersion++
	require.Error(t, invalidMapping.Validate(identity))
}

func TestIdentityAcceptsEachClosedProviderAndKeepsKeysSeparate(t *testing.T) {
	pi := testIdentity()
	codex := pi
	codex.Provider = provider.NameCodex
	require.NoError(t, pi.Validate())
	require.NoError(t, codex.Validate())
	piKey, err := ConversationKey(pi)
	require.NoError(t, err)
	codexKey, err := ConversationKey(codex)
	require.NoError(t, err)
	require.NotEqual(t, piKey, codexKey)

	invalid := pi
	invalid.Provider = provider.Name("other")
	require.Error(t, invalid.Validate())
}
