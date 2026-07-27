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

type evictedReplayEntry struct {
	eventID        string
	targetClientID string
}

type ReplayLog struct {
	entries      []ReplayEntry
	total        int
	evicted      map[string]string
	evictedOrder []evictedReplayEntry
}

func NewReplayLog() *ReplayLog {
	return &ReplayLog{evicted: make(map[string]string)}
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

func (log *ReplayLog) evictedLen() int {
	if log == nil {
		return 0
	}
	return len(log.evictedOrder)
}

// Append adds a broadcast event. The event's bound is measured from the exact
// agentprotocol wire encoding, not from a logical payload estimate.
func (log *ReplayLog) Append(event agentprotocol.Event) error {
	return log.append(event, "")
}

// AppendForClient adds an event visible only to the identified client.
func (log *ReplayLog) AppendForClient(clientID string, event agentprotocol.Event) error {
	return log.append(event, clientID)
}

func (log *ReplayLog) append(event agentprotocol.Event, target string) error {
	if log == nil {
		return errors.New("nil replay log")
	}
	if target != "" && common.ValidateID(target) != nil {
		return errors.New("invalid replay target")
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

func (log *ReplayLog) evictOldest() {
	if len(log.entries) == 0 {
		return
	}
	oldest := log.entries[0]
	log.entries[0] = ReplayEntry{}
	log.entries = log.entries[1:]
	log.total -= oldest.encodedBytes
	if len(log.evictedOrder) == MaxReplayEvents {
		forgotten := log.evictedOrder[0]
		delete(log.evicted, forgotten.eventID)
		copy(log.evictedOrder, log.evictedOrder[1:])
		log.evictedOrder[len(log.evictedOrder)-1] = evictedReplayEntry{}
		log.evictedOrder = log.evictedOrder[:len(log.evictedOrder)-1]
	}
	record := evictedReplayEntry{eventID: oldest.Event.EventID, targetClientID: oldest.TargetClientID}
	log.evicted[record.eventID] = record.targetClientID
	log.evictedOrder = append(log.evictedOrder, record)
}

// Replay returns visible events strictly after afterEventID. An empty cursor
// starts at the beginning. Cursor lookup occurs before visibility filtering so
// a client cannot use another client's targeted event as an ordering anchor.
func (log *ReplayLog) Replay(clientID, afterEventID string) ([]agentprotocol.Event, error) {
	if log == nil {
		return nil, errors.New("nil replay log")
	}
	if common.ValidateID(clientID) != nil {
		return nil, errors.New("invalid replay client")
	}
	client := clientID
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
			if target, evicted := log.evicted[afterEventID]; evicted {
				if target != "" && target != client {
					return nil, ReplayCursorError{classification: ReplayCursorMissing}
				}
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

var ErrReplayDuplicateID = errors.New("duplicate replay event ID")

func cloneEvent(event agentprotocol.Event) agentprotocol.Event {
	event.Payload = clonePayload(event.Payload)
	return event
}

func clonePayload(payload agentprotocol.EventPayload) agentprotocol.EventPayload {
	switch value := payload.(type) {
	case agentprotocol.SnapshotPayload:
		value.Queue = append([]agentprotocol.QueueItem(nil), value.Queue...)
		if value.ActiveTurnID != nil {
			active := *value.ActiveTurnID
			value.ActiveTurnID = &active
		}
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
	case agentprotocol.LifecyclePayload:
		if value.TurnID != nil {
			turnID := *value.TurnID
			value.TurnID = &turnID
		}
		return value
	case *agentprotocol.LifecyclePayload:
		if value == nil {
			return (*agentprotocol.LifecyclePayload)(nil)
		}
		copyOfValue := *value
		if value.TurnID != nil {
			turnID := *value.TurnID
			copyOfValue.TurnID = &turnID
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
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			value.NextCursor = &cursor
		}
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
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			value.NextCursor = &cursor
		}
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
		if value.Error != nil {
			errorValue := *value.Error
			value.Error = &errorValue
		}
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
