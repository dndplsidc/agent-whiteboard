package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

const (
	MaxReplayEvents = agentprotocol.MaxReplayEvents
	MaxReplayBytes  = agentprotocol.MaxReplayBytes
)

type ReplayCursorClassification string

const (
	ReplayCursorMissing ReplayCursorClassification = "missing"
	ReplayCursorEvicted ReplayCursorClassification = "evicted"
)

// ReplayCursorError classifies why a requested cursor cannot be used. Its
// Error text is fixed so event IDs and other request data never leak through an
// error boundary.
type ReplayCursorError struct {
	classification ReplayCursorClassification
}

func (e ReplayCursorError) Error() string {
	if e.classification == ReplayCursorEvicted {
		return ErrReplayCursorEvicted.Error()
	}
	return ErrReplayCursorMissing.Error()
}
func (e ReplayCursorError) Classification() ReplayCursorClassification { return e.classification }
func (e ReplayCursorError) Is(target error) bool {
	switch target {
	case ErrReplayCursorEvicted:
		return e.classification == ReplayCursorEvicted
	case ErrReplayCursorMissing:
		return e.classification == ReplayCursorMissing
	default:
		return false
	}
}

// ReplayEntry holds one normalized browser event and its optional visibility
// target. An empty target is broadcast to every attached client.
type ReplayEntry struct {
	Event          agentprotocol.Event
	TargetClientID string
	encodedBytes   int
}

type ReplayLog struct {
	entries []ReplayEntry
	total   int
	evicted map[string]struct{}
}

func NewReplayLog() *ReplayLog {
	return &ReplayLog{evicted: make(map[string]struct{})}
}

func (log *ReplayLog) Len() int {
	if log == nil {
		return 0
	}
	return len(log.entries)
}
func (log *ReplayLog) Bytes() int {
	if log == nil {
		return 0
	}
	return log.total
}

// Append adds a broadcast event unless an optional client target is supplied.
// The event's bound is measured from the exact agentprotocol wire encoding,
// not from a logical payload estimate.
func (log *ReplayLog) Append(event agentprotocol.Event, targetClientID ...string) error {
	if log == nil {
		return errors.New("nil replay log")
	}
	target := ""
	if len(targetClientID) > 1 {
		return errors.New("invalid replay target")
	}
	if len(targetClientID) == 1 {
		target = targetClientID[0]
		if target != "" && common.ValidateID(target) != nil {
			return errors.New("invalid replay target")
		}
	}
	encoded, err := agentprotocol.EncodeEvent(event)
	if err != nil {
		return err
	}
	for _, entry := range log.entries {
		if entry.Event.EventID == event.EventID {
			return ErrReplayDuplicateID
		}
	}
	if _, exists := log.evicted[event.EventID]; exists {
		return ErrReplayDuplicateID
	}
	entry := ReplayEntry{Event: cloneEvent(event), TargetClientID: target, encodedBytes: len(encoded)}
	log.entries = append(log.entries, entry)
	log.total += entry.encodedBytes
	for len(log.entries) > MaxReplayEvents || log.total > MaxReplayBytes {
		log.evictOldest()
	}
	return nil
}

// AppendForClient is the explicit targeted form of Append.
func (log *ReplayLog) AppendForClient(clientID string, event agentprotocol.Event) error {
	return log.Append(event, clientID)
}

func (log *ReplayLog) evictOldest() {
	if len(log.entries) == 0 {
		return
	}
	oldest := log.entries[0]
	log.entries[0] = ReplayEntry{}
	log.entries = log.entries[1:]
	log.total -= oldest.encodedBytes
	if len(log.evicted) < MaxReplayEvents {
		log.evicted[oldest.Event.EventID] = struct{}{}
	}
}

// Replay returns visible events strictly after afterEventID. An empty cursor
// starts at the beginning. Cursor lookup occurs before visibility filtering so
// a client cannot use another client's targeted event as an ordering anchor.
func (log *ReplayLog) Replay(afterEventID string, clientID ...string) ([]agentprotocol.Event, error) {
	if log == nil {
		return nil, errors.New("nil replay log")
	}
	if len(clientID) > 1 {
		return nil, errors.New("invalid replay client")
	}
	client := ""
	if len(clientID) == 1 {
		client = clientID[0]
		if client != "" && common.ValidateID(client) != nil {
			return nil, errors.New("invalid replay client")
		}
	}
	start := 0
	if afterEventID != "" {
		if common.ValidateID(afterEventID) != nil {
			return nil, ReplayCursorError{classification: ReplayCursorMissing}
		}
		found := false
		for index, entry := range log.entries {
			if entry.Event.EventID == afterEventID {
				found = true
				if entry.TargetClientID != "" && entry.TargetClientID != client {
					return nil, ReplayCursorError{classification: ReplayCursorMissing}
				}
				start = index + 1
				break
			}
		}
		if !found {
			if _, evicted := log.evicted[afterEventID]; evicted {
				return nil, ReplayCursorError{classification: ReplayCursorEvicted}
			}
			return nil, ReplayCursorError{classification: ReplayCursorMissing}
		}
	}
	result := make([]agentprotocol.Event, 0, len(log.entries)-start)
	for _, entry := range log.entries[start:] {
		if entry.TargetClientID != "" && entry.TargetClientID != client {
			continue
		}
		result = append(result, cloneEvent(entry.Event))
	}
	return result, nil
}

// ReplayForClient makes the visibility-first call shape explicit.
func (log *ReplayLog) ReplayForClient(clientID, afterEventID string) ([]agentprotocol.Event, error) {
	return log.Replay(afterEventID, clientID)
}

func (log *ReplayLog) EventsAfter(afterEventID, clientID string) ([]agentprotocol.Event, error) {
	return log.Replay(afterEventID, clientID)
}

var ErrReplayDuplicateID = errors.New("duplicate replay event ID")

func cloneEvent(event agentprotocol.Event) agentprotocol.Event {
	event.Payload = clonePayload(event.Payload)
	return event
}

func clonePayload(payload agentprotocol.EventPayload) agentprotocol.EventPayload {
	switch value := payload.(type) {
	case agentprotocol.SnapshotPayload:
		value.Queue = append([]agentprotocol.QueueItem(nil), value.Queue...)
		return value
	case *agentprotocol.SnapshotPayload:
		if value == nil {
			return (*agentprotocol.SnapshotPayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Queue = append([]agentprotocol.QueueItem(nil), value.Queue...)
		if value.ActiveTurnID != nil {
			active := *value.ActiveTurnID
			copyOfValue.ActiveTurnID = &active
		}
		return &copyOfValue
	case agentprotocol.QueuePayload:
		value.Items = append([]agentprotocol.QueueItem(nil), value.Items...)
		return value
	case *agentprotocol.QueuePayload:
		if value == nil {
			return (*agentprotocol.QueuePayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Items = append([]agentprotocol.QueueItem(nil), value.Items...)
		return &copyOfValue
	case agentprotocol.TimelinePayload:
		value.Items = append([]agentprotocol.TimelineItem(nil), value.Items...)
		return value
	case *agentprotocol.TimelinePayload:
		if value == nil {
			return (*agentprotocol.TimelinePayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Items = append([]agentprotocol.TimelineItem(nil), value.Items...)
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			copyOfValue.NextCursor = &cursor
		}
		return &copyOfValue
	case agentprotocol.HistoryPayload:
		value.Items = append([]agentprotocol.ArchiveItem(nil), value.Items...)
		return value
	case *agentprotocol.HistoryPayload:
		if value == nil {
			return (*agentprotocol.HistoryPayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Items = append([]agentprotocol.ArchiveItem(nil), value.Items...)
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			copyOfValue.NextCursor = &cursor
		}
		return &copyOfValue
	case agentprotocol.CommandResultPayload:
		return value
	case *agentprotocol.CommandResultPayload:
		if value == nil {
			return (*agentprotocol.CommandResultPayload)(nil)
		}
		copyOfValue := *value
		if value.Error != nil {
			errorValue := *value.Error
			copyOfValue.Error = &errorValue
		}
		return &copyOfValue
	default:
		return payload
	}
}
