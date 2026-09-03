package protocol_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestCursorIdentityStrictSnapshotAndUnsupportedDeleteError(t *testing.T) {
	require.Equal(t, []protocol.ProviderName{protocol.ProviderPi, protocol.ProviderCodex, protocol.ProviderCursor}, protocol.AllProviderNames())
	require.True(t, protocol.ProviderCursor.Valid())
	require.False(t, protocol.ProviderName("Cursor").Valid())

	skillsState := protocol.SkillsReady
	maxSkills := 1
	payload := protocol.SnapshotPayload{
		Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextPending,
		SupportsArchiveDelete: false, Catalog: []protocol.CatalogModel{}, SkillsState: &skillsState,
		Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &maxSkills,
		BusyPolicy: protocol.BusyTurnPreserveDraft, ComposerAdmission: protocol.ComposerPreserveDraft,
	}
	require.NoError(t, payload.ValidateForProvider(protocol.ProviderCursor))
	event := protocol.Event{APIVersion: protocol.APIVersion, EventID: strings.Repeat("A", 32), ConversationID: strings.Repeat("B", 32), Type: protocol.EventSnapshot, Timestamp: time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC), Payload: payload}
	encoded, err := protocol.EncodeEvent(event)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"supports_archive_delete":false`)
	decoded, err := protocol.DecodeEvent(encoded)
	require.NoError(t, err)
	require.False(t, decoded.Payload.(protocol.SnapshotPayload).SupportsArchiveDelete)

	missing := strings.Replace(string(encoded), `,"supports_archive_delete":false`, "", 1)
	_, err = protocol.DecodeEvent([]byte(missing))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
	duplicate := strings.Replace(string(encoded), `"supports_archive_delete":false`, `"supports_archive_delete":false,"supports_archive_delete":false`, 1)
	_, err = protocol.DecodeEvent([]byte(duplicate))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	browserErr := protocol.NewBrowserError(protocol.ErrorArchiveDeleteUnsupported)
	require.Equal(t, protocol.ActionNone, browserErr.Action())
	require.Contains(t, protocol.AllBrowserErrorCodes(), protocol.ErrorArchiveDeleteUnsupported)
}
