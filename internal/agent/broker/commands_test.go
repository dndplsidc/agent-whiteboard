package broker

import (
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestCommandLedgerDeduplicatesPendingAndCompletedCommands(t *testing.T) {
	ledger := newCommandLedger()
	command := ledgerSubmitCommand(sequenceID(3001), sequenceID(3002), "question")

	disposition, event, err := ledger.begin(command)
	require.NoError(t, err)
	require.Equal(t, commandNew, disposition)
	require.Equal(t, protocol.Event{}, event)

	disposition, event, err = ledger.begin(command)
	require.NoError(t, err)
	require.Equal(t, commandPending, disposition)
	require.Equal(t, protocol.Event{}, event)

	waiter := commandWaiter{response: make(chan commandResponse, 1)}
	require.NoError(t, ledger.wait(command.CommandID, waiter))
	result := commandResultEvent(command, sequenceID(3003), protocol.CommandSucceeded, nil)
	waiters, err := ledger.complete(command.CommandID, result)
	require.NoError(t, err)
	require.Equal(t, []commandWaiter{waiter}, waiters)

	disposition, replayed, err := ledger.begin(command)
	require.NoError(t, err)
	require.Equal(t, commandCompleted, disposition)
	require.Equal(t, result, replayed)
}

func TestCommandLedgerRejectsReusedIDWithDifferentCanonicalContent(t *testing.T) {
	ledger := newCommandLedger()
	original := ledgerSubmitCommand(sequenceID(3010), sequenceID(3011), "first")
	disposition, _, err := ledger.begin(original)
	require.NoError(t, err)
	require.Equal(t, commandNew, disposition)

	changed := original
	payload := changed.Payload.(protocol.SubmitPayload)
	payload.Content = protocol.TextContent("second")
	changed.Payload = payload
	disposition, _, err = ledger.begin(changed)
	require.NoError(t, err)
	require.Equal(t, commandConflict, disposition)

	contextChanged := original
	payload = contextChanged.Payload.(protocol.SubmitPayload)
	page := *payload.Context
	page.Title = "different title"
	payload.Context = &page
	contextChanged.Payload = payload
	disposition, _, err = ledger.begin(contextChanged)
	require.NoError(t, err)
	require.Equal(t, commandConflict, disposition)
}

func TestCommandLedgerHasBoundedCompletedHorizon(t *testing.T) {
	ledger := newCommandLedger()
	var oldest protocol.Command
	for index := 0; index < maxCommandLedgerEntries+1; index++ {
		command := ledgerSubmitCommand(sequenceID(uint64(3100+index)), sequenceID(uint64(4200+index)), "message")
		if index == 0 {
			oldest = command
		}
		disposition, _, err := ledger.begin(command)
		require.NoError(t, err)
		require.Equal(t, commandNew, disposition)
		_, err = ledger.complete(command.CommandID, commandResultEvent(command, sequenceID(uint64(5300+index)), protocol.CommandSucceeded, nil))
		require.NoError(t, err)
	}
	require.Len(t, ledger.entries, maxCommandLedgerEntries)
	disposition, _, err := ledger.begin(oldest)
	require.NoError(t, err)
	require.Equal(t, commandNew, disposition, "commands beyond the documented memory-only horizon may be admitted again")
}

func TestCommandFingerprintDoesNotDependOnPageBufferIdentity(t *testing.T) {
	original := ledgerSubmitCommand(sequenceID(6401), sequenceID(6402), "question")
	cloned := original
	payload := cloned.Payload.(protocol.SubmitPayload)
	contextCopy := *payload.Context
	contextCopy.Source = string(append([]byte(nil), []byte(payload.Context.Source)...))
	contextCopy.CreatorContext = string(append([]byte(nil), []byte(payload.Context.CreatorContext)...))
	payload.Context = &contextCopy
	cloned.Payload = payload

	first, err := commandFingerprint(original)
	require.NoError(t, err)
	second, err := commandFingerprint(cloned)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func testPageContext(resource protocol.Resource) protocol.PageContext {
	source := "# Page\n"
	creator := "creator context"
	digest, _ := agent.CalculateContextDigestForKind(string(resource.Kind), []byte(source), []byte(creator))
	return protocol.PageContext{
		Revision:       protocol.ContextInitial,
		Source:         source,
		CreatorContext: creator,
		Title:          "Page",
		URL:            "https://example.com/whiteboards/markdown/" + resource.ID,
		Resource:       resource,
		Digest:         digest,
	}
}

func ledgerSubmitCommand(commandID, conversationID, message string) protocol.Command {
	resource := testResource(sequenceID(6403))
	context := testPageContext(resource)
	return protocol.Command{
		APIVersion:     protocol.APIVersion,
		CommandID:      commandID,
		ClientID:       sequenceID(6404),
		ConversationID: &conversationID,
		Type:           protocol.CommandSubmit,
		Payload:        protocol.SubmitPayload{TurnID: sequenceID(6405), MessageID: sequenceID(6406), Content: protocol.TextContent(message), Context: &context},
	}
}

func commandResultEvent(command protocol.Command, eventID string, status protocol.CommandStatus, browserError *protocol.BrowserError) protocol.Event {
	return protocol.Event{
		APIVersion:     protocol.APIVersion,
		EventID:        eventID,
		ConversationID: *command.ConversationID,
		Type:           protocol.EventCommandResult,
		Timestamp:      testTime(),
		Payload:        protocol.CommandResultPayload{CommandID: command.CommandID, Status: status, Error: browserError},
	}
}
