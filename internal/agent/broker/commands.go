package broker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"sort"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
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
	attachment *clientAttachment
	response   chan commandResponse
}

type commandLedgerEntry struct {
	fingerprint [sha256.Size]byte
	result      *protocol.Event
	waiters     []commandWaiter
}

type commandLedger struct {
	entries map[string]*commandLedgerEntry
	order   []string
}

func newCommandLedger() commandLedger {
	return commandLedger{entries: make(map[string]*commandLedgerEntry)}
}

func (ledger *commandLedger) begin(command protocol.Command) (commandDisposition, protocol.Event, error) {
	fingerprint, err := commandFingerprint(command)
	if err != nil {
		return commandConflict, protocol.Event{}, err
	}
	if entry, exists := ledger.entries[command.CommandID]; exists {
		if entry.fingerprint != fingerprint {
			return commandConflict, protocol.Event{}, nil
		}
		if entry.result == nil {
			return commandPending, protocol.Event{}, nil
		}
		return commandCompleted, cloneEvent(*entry.result), nil
	}
	if !ledger.makeRoom() {
		return commandConflict, protocol.Event{}, errCommandLedgerFull
	}
	ledger.entries[command.CommandID] = &commandLedgerEntry{fingerprint: fingerprint}
	return commandNew, protocol.Event{}, nil
}

func (ledger *commandLedger) wait(commandID string, waiter commandWaiter) error {
	entry, exists := ledger.entries[commandID]
	if !exists || entry.result != nil || waiter.response == nil {
		return errors.New("command is not pending")
	}
	entry.waiters = append(entry.waiters, waiter)
	return nil
}

func (ledger *commandLedger) complete(commandID string, event protocol.Event) ([]commandWaiter, error) {
	entry, exists := ledger.entries[commandID]
	if !exists || entry.result != nil {
		return nil, errors.New("command is not pending")
	}
	payload, ok := event.Payload.(protocol.CommandResultPayload)
	if !ok || payload.CommandID != commandID {
		return nil, errors.New("invalid command result")
	}
	if _, err := protocol.EncodeEvent(event); err != nil {
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
func commandFingerprint(command protocol.Command) ([sha256.Size]byte, error) {
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
	case protocol.ConnectPayload:
		writer.text(string(payload.Provider))
		writer.resource(payload.Resource)
		writer.text(payload.ContextDigest)
		writer.text(payload.ReplayAfter)
	case protocol.SubmitPayload:
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
	case protocol.QueueEditPayload:
		writer.text(payload.MessageID)
		encoded, err := json.Marshal(payload.Content)
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		writer.bytes(encoded)
	case protocol.MessageReferencePayload:
		writer.text(payload.MessageID)
	case protocol.TurnReferencePayload:
		writer.text(payload.TurnID)
	case protocol.EmptyPayload:
		// The command type fully identifies an empty payload.
	case protocol.PageRequestPayload:
		writer.text(payload.Before)
		writer.integer(int64(payload.Limit))
	case protocol.ArchiveReferencePayload:
		writer.text(payload.ArchiveID)
	case protocol.ResyncPayload:
		writer.text(payload.AfterEventID)
	case protocol.InteractionResponsePayload:
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
func (writer fingerprintWriter) resource(resource protocol.Resource) {
	writer.text(string(resource.Kind))
	writer.text(resource.ID)
	writer.timestamp(resource.CreatedAt)
	writer.timestamp(resource.UpdatedAt)
	writer.boolean(resource.ExpiresAt != nil)
	if resource.ExpiresAt != nil {
		writer.timestamp(*resource.ExpiresAt)
	}
}
