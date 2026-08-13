package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

const (
	MaxReplayEvents = protocol.MaxReplayEvents
	MaxReplayBytes  = protocol.MaxReplayBytes
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
	Event          protocol.Event
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
// protocol wire encoding, not from a logical payload estimate.
func (log *ReplayLog) Append(event protocol.Event) error {
	return log.append(event, "")
}

// AppendForClient adds an event visible only to the identified client.
func (log *ReplayLog) AppendForClient(clientID string, event protocol.Event) error {
	return log.append(event, clientID)
}

func (log *ReplayLog) append(event protocol.Event, target string) error {
	if log == nil {
		return errors.New("nil replay log")
	}
	if target != "" && common.ValidateID(target) != nil {
		return errors.New("invalid replay target")
	}
	encoded, err := protocol.EncodeEvent(event)
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
func (log *ReplayLog) Replay(clientID, afterEventID string) ([]protocol.Event, error) {
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
	result := make([]protocol.Event, 0, len(log.entries)-start)
	for _, entry := range log.entries[start:] {
		if entry.TargetClientID != "" && entry.TargetClientID != client {
			continue
		}
		result = append(result, cloneEvent(entry.Event))
	}
	return result, nil
}

var ErrReplayDuplicateID = errors.New("duplicate replay event ID")

func cloneEvent(event protocol.Event) protocol.Event {
	event.Payload = clonePayload(event.Payload)
	return event
}

func clonePayload(payload protocol.EventPayload) protocol.EventPayload {
	switch value := payload.(type) {
	case protocol.SnapshotPayload:
		value.Queue = cloneQueueItems(value.Queue)
		value.Catalog = cloneCatalog(value.Catalog)
		value.Skills = append([]protocol.SkillDescriptor{}, value.Skills...)
		value.EffectiveSettings = clonePresentedSettings(value.EffectiveSettings)
		if value.SettingsState != nil {
			state := *value.SettingsState
			value.SettingsState = &state
		}
		if value.SkillsState != nil {
			state := *value.SkillsState
			value.SkillsState = &state
		}
		if value.ActiveWork != nil {
			active := *value.ActiveWork
			value.ActiveWork = &active
		}
		return value
	case *protocol.SnapshotPayload:
		if value == nil {
			return (*protocol.SnapshotPayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Queue = cloneQueueItems(value.Queue)
		copyOfValue.Catalog = cloneCatalog(value.Catalog)
		copyOfValue.Skills = append([]protocol.SkillDescriptor{}, value.Skills...)
		copyOfValue.EffectiveSettings = clonePresentedSettings(value.EffectiveSettings)
		if value.SettingsState != nil {
			state := *value.SettingsState
			copyOfValue.SettingsState = &state
		}
		if value.SkillsState != nil {
			state := *value.SkillsState
			copyOfValue.SkillsState = &state
		}
		if value.ActiveWork != nil {
			active := *value.ActiveWork
			copyOfValue.ActiveWork = &active
		}
		return &copyOfValue
	case protocol.LifecyclePayload:
		if value.ActiveWork != nil {
			work := *value.ActiveWork
			value.ActiveWork = &work
		}
		return value
	case *protocol.LifecyclePayload:
		if value == nil {
			return (*protocol.LifecyclePayload)(nil)
		}
		copyOfValue := *value
		if value.ActiveWork != nil {
			work := *value.ActiveWork
			copyOfValue.ActiveWork = &work
		}
		return &copyOfValue
	case protocol.SettingsPayload:
		value.Catalog = cloneCatalog(value.Catalog)
		value.EffectiveSettings = clonePresentedSettings(value.EffectiveSettings)
		if value.AcceptedTurnID != nil {
			turnID := *value.AcceptedTurnID
			value.AcceptedTurnID = &turnID
		}
		return value
	case *protocol.SettingsPayload:
		if value == nil {
			return (*protocol.SettingsPayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Catalog = cloneCatalog(value.Catalog)
		copyOfValue.EffectiveSettings = clonePresentedSettings(value.EffectiveSettings)
		if value.AcceptedTurnID != nil {
			turnID := *value.AcceptedTurnID
			copyOfValue.AcceptedTurnID = &turnID
		}
		return &copyOfValue
	case protocol.SkillCatalogPayload:
		value.Skills = append([]protocol.SkillDescriptor{}, value.Skills...)
		return value
	case *protocol.SkillCatalogPayload:
		if value == nil {
			return (*protocol.SkillCatalogPayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Skills = append([]protocol.SkillDescriptor{}, value.Skills...)
		return &copyOfValue
	case protocol.QueuePayload:
		value.Items = cloneQueueItems(value.Items)
		return value
	case *protocol.QueuePayload:
		if value == nil {
			return (*protocol.QueuePayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Items = cloneQueueItems(value.Items)
		return &copyOfValue
	case protocol.TimelinePayload:
		value.Items = cloneTimelineItems(value.Items)
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			value.NextCursor = &cursor
		}
		return value
	case *protocol.TimelinePayload:
		if value == nil {
			return (*protocol.TimelinePayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Items = cloneTimelineItems(value.Items)
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			copyOfValue.NextCursor = &cursor
		}
		return &copyOfValue
	case protocol.UserMessagePayload:
		value.Content = value.Content.Clone()
		value.Images = append([]protocol.ImageDescriptor(nil), value.Images...)
		return value
	case *protocol.UserMessagePayload:
		if value == nil {
			return (*protocol.UserMessagePayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Content = value.Content.Clone()
		copyOfValue.Images = append([]protocol.ImageDescriptor(nil), value.Images...)
		return &copyOfValue
	case protocol.HistoryPayload:
		value.Items = append([]protocol.ArchiveItem{}, value.Items...)
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			value.NextCursor = &cursor
		}
		return value
	case *protocol.HistoryPayload:
		if value == nil {
			return (*protocol.HistoryPayload)(nil)
		}
		copyOfValue := *value
		copyOfValue.Items = append([]protocol.ArchiveItem{}, value.Items...)
		if value.NextCursor != nil {
			cursor := *value.NextCursor
			copyOfValue.NextCursor = &cursor
		}
		return &copyOfValue
	case protocol.CommandResultPayload:
		if value.Error != nil {
			errorValue := *value.Error
			value.Error = &errorValue
		}
		return value
	case *protocol.CommandResultPayload:
		if value == nil {
			return (*protocol.CommandResultPayload)(nil)
		}
		copyOfValue := *value
		if value.Error != nil {
			errorValue := *value.Error
			copyOfValue.Error = &errorValue
		}
		return &copyOfValue
	case protocol.InteractionRequestPayload:
		return cloneInteractionRequestPayload(value)
	case *protocol.InteractionRequestPayload:
		if value == nil {
			return (*protocol.InteractionRequestPayload)(nil)
		}
		copyOfValue := cloneInteractionRequestPayload(*value)
		return &copyOfValue
	default:
		return payload
	}
}

func cloneQueueItems(items []protocol.QueueItem) []protocol.QueueItem {
	result := append([]protocol.QueueItem{}, items...)
	for index := range result {
		result[index].Content = items[index].Content.Clone()
		result[index].Images = append([]protocol.ImageDescriptor(nil), items[index].Images...)
		result[index].Settings = clonePresentedSettings(items[index].Settings)
	}
	return result
}

func clonePresentedSettings(settings *protocol.PresentedExecutionSettings) *protocol.PresentedExecutionSettings {
	if settings == nil {
		return nil
	}
	copyOfSettings := *settings
	return &copyOfSettings
}

func cloneCatalog(catalog []protocol.CatalogModel) []protocol.CatalogModel {
	result := append([]protocol.CatalogModel{}, catalog...)
	for index := range result {
		result[index].SupportedReasoningEfforts = append([]protocol.ReasoningEffortOption{}, catalog[index].SupportedReasoningEfforts...)
	}
	return result
}

func cloneTimelineItems(items []protocol.TimelineItem) []protocol.TimelineItem {
	result := append([]protocol.TimelineItem{}, items...)
	for index := range result {
		if items[index].Content != nil {
			content := items[index].Content.Clone()
			result[index].Content = &content
		}
		result[index].Images = append([]protocol.ImageDescriptor(nil), items[index].Images...)
	}
	return result
}

func cloneInteractionRequestPayload(value protocol.InteractionRequestPayload) protocol.InteractionRequestPayload {
	value.Options = append([]protocol.InteractionOption(nil), value.Options...)
	value.Questions = append([]protocol.InteractionQuestion(nil), value.Questions...)
	for index := range value.Questions {
		value.Questions[index].Options = append([]protocol.InteractionOption(nil), value.Questions[index].Options...)
	}
	value.Fields = append([]protocol.InteractionField(nil), value.Fields...)
	for index := range value.Fields {
		value.Fields[index].Options = append([]protocol.InteractionOption(nil), value.Fields[index].Options...)
	}
	return value
}
