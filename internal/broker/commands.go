package broker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"sort"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
)

const maxCommandLedgerEntries = 1024

var errCommandLedgerFull = errors.New("command ledger is full")

type commandDisposition uint8

const (
	commandNew commandDisposition = iota
	commandPending
	commandCompleted
	commandConflict
)

type commandWaiter struct {
	attachment *attachment
	response   chan commandResponse
}

type commandLedgerEntry struct {
	fingerprint [sha256.Size]byte
	result      *agentprotocol.Event
	waiters     []commandWaiter
}

type commandLedger struct {
	entries map[string]*commandLedgerEntry
	order   []string
}

func newCommandLedger() commandLedger {
	return commandLedger{entries: make(map[string]*commandLedgerEntry)}
}

func (ledger *commandLedger) begin(command agentprotocol.Command) (commandDisposition, agentprotocol.Event, error) {
	fingerprint, err := commandFingerprint(command)
	if err != nil {
		return commandConflict, agentprotocol.Event{}, err
	}
	if entry, exists := ledger.entries[command.CommandID]; exists {
		if entry.fingerprint != fingerprint {
			return commandConflict, agentprotocol.Event{}, nil
		}
		if entry.result == nil {
			return commandPending, agentprotocol.Event{}, nil
		}
		return commandCompleted, cloneEvent(*entry.result), nil
	}
	if !ledger.makeRoom() {
		return commandConflict, agentprotocol.Event{}, errCommandLedgerFull
	}
	ledger.entries[command.CommandID] = &commandLedgerEntry{fingerprint: fingerprint}
	return commandNew, agentprotocol.Event{}, nil
}

func (ledger *commandLedger) wait(commandID string, waiter commandWaiter) error {
	entry, exists := ledger.entries[commandID]
	if !exists || entry.result != nil || waiter.response == nil {
		return errors.New("command is not pending")
	}
	entry.waiters = append(entry.waiters, waiter)
	return nil
}

func (ledger *commandLedger) complete(commandID string, event agentprotocol.Event) ([]commandWaiter, error) {
	entry, exists := ledger.entries[commandID]
	if !exists || entry.result != nil {
		return nil, errors.New("command is not pending")
	}
	payload, ok := event.Payload.(agentprotocol.CommandResultPayload)
	if !ok || payload.CommandID != commandID {
		return nil, errors.New("invalid command result")
	}
	if _, err := agentprotocol.EncodeEvent(event); err != nil {
		return nil, err
	}
	result := cloneEvent(event)
	entry.result = &result
	waiters := entry.waiters
	entry.waiters = nil
	ledger.order = append(ledger.order, commandID)
	return waiters, nil
}

func (ledger *commandLedger) abandon(commandID string) []commandWaiter {
	entry, exists := ledger.entries[commandID]
	if !exists || entry.result != nil {
		return nil
	}
	delete(ledger.entries, commandID)
	waiters := entry.waiters
	entry.waiters = nil
	return waiters
}

func (ledger *commandLedger) makeRoom() bool {
	for len(ledger.entries) >= maxCommandLedgerEntries && len(ledger.order) != 0 {
		oldest := ledger.order[0]
		ledger.order[0] = ""
		ledger.order = ledger.order[1:]
		if entry, exists := ledger.entries[oldest]; exists && entry.result != nil {
			delete(ledger.entries, oldest)
		}
	}
	return len(ledger.entries) < maxCommandLedgerEntries
}

// commandFingerprint deliberately represents page bytes by their independently
// validated canonical digest. It never retains, serializes, or copies Markdown
// or creator-context content into the command ledger.
func commandFingerprint(command agentprotocol.Command) ([sha256.Size]byte, error) {
	writer := fingerprintWriter{hash: sha256.New()}
	writer.text(command.APIVersion)
	writer.text(command.CommandID)
	writer.text(command.ClientID)
	if command.ConversationID == nil {
		writer.boolean(false)
	} else {
		writer.boolean(true)
		writer.text(*command.ConversationID)
	}
	writer.text(string(command.Type))

	switch payload := command.Payload.(type) {
	case agentprotocol.ConnectPayload:
		writer.text(string(payload.Provider))
		writer.resource(payload.Resource)
		writer.text(payload.ContextDigest)
		writer.text(payload.ReplayAfter)
	case agentprotocol.SubmitPayload:
		writer.text(payload.TurnID)
		writer.text(payload.MessageID)
		encoded, err := json.Marshal(payload.Content)
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		writer.bytes(encoded)
		for _, image := range payload.Images {
			writer.text(image.ImageID)
			writer.text(image.Name)
		}
		writer.boolean(payload.Context != nil)
		if payload.Context != nil {
			writer.text(string(payload.Context.Revision))
			writer.text(payload.Context.Digest)
			writer.text(payload.Context.Title)
			writer.text(payload.Context.URL)
			writer.resource(payload.Context.Resource)
		}
	case agentprotocol.QueueEditPayload:
		writer.text(payload.MessageID)
		encoded, err := json.Marshal(payload.Content)
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		writer.bytes(encoded)
	case agentprotocol.MessageReferencePayload:
		writer.text(payload.MessageID)
	case agentprotocol.TurnReferencePayload:
		writer.text(payload.TurnID)
	case agentprotocol.EmptyPayload:
		// The command type fully identifies an empty payload.
	case agentprotocol.PageRequestPayload:
		writer.text(payload.Before)
		writer.integer(int64(payload.Limit))
	case agentprotocol.ArchiveReferencePayload:
		writer.text(payload.ArchiveID)
	case agentprotocol.ResyncPayload:
		writer.text(payload.AfterEventID)
	case agentprotocol.InteractionResponsePayload:
		writer.text(payload.RequestID)
		writer.text(string(payload.Kind))
		writer.text(payload.OptionID)
		keys := make([]string, 0, len(payload.Answers))
		for key := range payload.Answers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writer.text(key)
			for _, answer := range payload.Answers[key] {
				writer.text(answer)
			}
		}
	default:
		return [sha256.Size]byte{}, errors.New("unsupported command payload")
	}
	var result [sha256.Size]byte
	copy(result[:], writer.hash.Sum(nil))
	return result, nil
}

type fingerprintWriter struct{ hash hash.Hash }

func (writer fingerprintWriter) bytes(value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.hash.Write(size[:])
	_, _ = writer.hash.Write(value)
}
func (writer fingerprintWriter) text(value string) { writer.bytes([]byte(value)) }
func (writer fingerprintWriter) boolean(value bool) {
	if value {
		writer.bytes([]byte{1})
		return
	}
	writer.bytes([]byte{0})
}
func (writer fingerprintWriter) integer(value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	writer.bytes(encoded[:])
}
func (writer fingerprintWriter) timestamp(value time.Time) {
	writer.text(value.UTC().Format(time.RFC3339Nano))
}
func (writer fingerprintWriter) resource(resource agentprotocol.Resource) {
	writer.text(string(resource.Kind))
	writer.text(resource.ID)
	writer.timestamp(resource.CreatedAt)
	writer.timestamp(resource.UpdatedAt)
	writer.boolean(resource.ExpiresAt != nil)
	if resource.ExpiresAt != nil {
		writer.timestamp(*resource.ExpiresAt)
	}
}
