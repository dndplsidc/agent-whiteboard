//go:build unix

package pi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type nativeEntry struct {
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Type      string          `json:"type"`
	Timestamp any             `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}
type entriesData struct {
	Entries  []nativeEntry     `json:"entries"`
	Messages []json.RawMessage `json:"messages"`
	LeafID   json.RawMessage   `json:"leafId"`
}
type nativeView struct {
	entries []nativeEntry
	linear  bool
}
type brokerNativeItem struct {
	item    provider.HistoryItem
	aborted bool
}

func (s *Session) queryEntries(ctx context.Context) (nativeView, error) {
	response, _, err := s.rpc.call(ctx, "get_entries", nil)
	if err != nil {
		return nativeView{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if requireSuccessfulResponse(response, "get_entries") != nil {
		response, _, err = s.rpc.call(ctx, "get_messages", nil)
		if err != nil || requireSuccessfulResponse(response, "get_messages") != nil {
			return nativeView{}, provider.NewProviderError(provider.ErrorProtocolFailure)
		}
		var wrapped entriesData
		if json.Unmarshal(response.Data, &wrapped) != nil || wrapped.Messages == nil {
			return nativeView{}, provider.NewProviderError(provider.ErrorMalformedStream)
		}
		converted := make([]nativeEntry, len(wrapped.Messages))
		for index, message := range wrapped.Messages {
			converted[index] = nativeEntry{Type: "message", Message: message}
		}
		return nativeView{entries: converted, linear: true}, nil
	}
	var wrapped entriesData
	if json.Unmarshal(response.Data, &wrapped) != nil || wrapped.Entries == nil || len(wrapped.LeafID) == 0 {
		return nativeView{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	entries, ok := activeBranchEntries(wrapped.Entries, wrapped.LeafID)
	if !ok {
		return nativeView{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	return nativeView{entries: entries, linear: true}, nil
}

func activeBranchEntries(entries []nativeEntry, rawLeaf json.RawMessage) ([]nativeEntry, bool) {
	if len(entries) > 100000 {
		return nil, false
	}
	if string(rawLeaf) == "null" {
		return []nativeEntry{}, len(entries) == 0
	}
	var leaf string
	if json.Unmarshal(rawLeaf, &leaf) != nil || leaf == "" {
		return nil, false
	}
	byID := make(map[string]nativeEntry, len(entries))
	for _, entry := range entries {
		if entry.ID == "" {
			return nil, false
		}
		if _, duplicate := byID[entry.ID]; duplicate {
			return nil, false
		}
		byID[entry.ID] = entry
	}
	reversed := make([]nativeEntry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for current := leaf; current != ""; {
		if _, duplicate := seen[current]; duplicate {
			return nil, false
		}
		seen[current] = struct{}{}
		entry, exists := byID[current]
		if !exists {
			return nil, false
		}
		reversed = append(reversed, entry)
		if entry.ParentID == nil {
			break
		}
		current = *entry.ParentID
		if current == "" {
			return nil, false
		}
	}
	result := make([]nativeEntry, len(reversed))
	for index := range reversed {
		result[len(reversed)-1-index] = reversed[index]
	}
	return result, true
}

func deriveBrokerItems(entries []nativeEntry) ([]brokerNativeItem, bool) {
	if len(entries) > 100000 {
		return nil, false
	}
	items := make([]brokerNativeItem, 0)
	var current *provider.HistoryItem
	assistantSeen := false
	seenMessages := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Type != "message" && entry.Type != "" {
			continue
		}
		msg, ok := parseNativeMessage(entry.Message)
		if !ok {
			return nil, false
		}
		at := eventTime(msg.Timestamp, time.Time{})
		if at.IsZero() {
			at = eventTime(entry.Timestamp, time.Time{})
		}
		if at.IsZero() {
			return nil, false
		}
		switch msg.Role {
		case "user":
			text, textOK := messageText(msg.Content)
			if !textOK {
				return nil, false
			}
			envelope, err := ParseEnvelope([]byte(text))
			if err != nil {
				return nil, false
			}
			if _, duplicate := seenMessages[envelope.MessageID]; duplicate {
				return nil, false
			}
			seenMessages[envelope.MessageID] = struct{}{}
			item := provider.HistoryItem{TurnID: envelope.TurnID, MessageID: envelope.MessageID, Role: provider.HistoryUser, Content: envelope.ReaderContent.Clone(), CreatedAt: at}
			items = append(items, brokerNativeItem{item: item})
			current = &items[len(items)-1].item
			assistantSeen = false
		case "assistant":
			if current == nil {
				continue
			}
			if assistantSeen {
				return nil, false
			}
			assistantSeen = true
			messageID := assistantMessageID(current.TurnID)
			if _, duplicate := seenMessages[messageID]; duplicate {
				return nil, false
			}
			seenMessages[messageID] = struct{}{}
			text := ""
			if msg.StopReason != "error" && msg.StopReason != "aborted" {
				var textOK bool
				text, textOK = messageText(msg.Content)
				if !textOK {
					return nil, false
				}
			}
			item := provider.HistoryItem{TurnID: current.TurnID, MessageID: messageID, Role: provider.HistoryAssistant, Text: text, CreatedAt: at}
			items = append(items, brokerNativeItem{item: item, aborted: msg.StopReason == "aborted"})
		default:
			return nil, false
		}
	}
	return items, true
}

func (s *Session) History(ctx context.Context, request provider.HistoryRequest) (provider.HistoryPage, error) {
	if request.Validate() != nil {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	view, err := s.queryEntries(ctx)
	if err != nil {
		return provider.HistoryPage{}, err
	}
	derived, ok := deriveBrokerItems(view.entries)
	if !ok || !view.linear {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	eligible := make([]provider.HistoryItem, 0, len(derived))
	for _, value := range derived {
		if value.item.Role == provider.HistoryUser && !value.item.Content.Empty() || value.item.Role == provider.HistoryAssistant && value.item.Text != "" {
			eligible = append(eligible, value.item)
		}
	}
	end := len(eligible)
	if request.BeforeMessageID != "" {
		found := false
		for index, item := range eligible {
			if item.MessageID == request.BeforeMessageID {
				end = index
				found = true
				break
			}
		}
		if !found {
			return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	limit := request.Limit
	if limit == 0 {
		limit = provider.MaxHistoryItems
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	total := 0
	actualStart := end
	for index := end - 1; index >= start; index-- {
		item := eligible[index]
		bytes := len(item.Text)
		if item.Role == provider.HistoryUser {
			bytes = item.Content.SemanticBytes()
		}
		if bytes > provider.MaxHistoryItemBytes {
			return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorMalformedStream)
		}
		if total+bytes > provider.MaxHistoryBytes {
			break
		}
		total += bytes
		actualStart = index
	}
	page := provider.HistoryPage{Items: make([]provider.HistoryItem, 0, end-actualStart)}
	for index := end - 1; index >= actualStart; index-- {
		page.Items = append(page.Items, eligible[index])
	}
	if actualStart > 0 && len(page.Items) > 0 {
		page.NextCursor = page.Items[len(page.Items)-1].MessageID
	}
	if page.Validate() != nil {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	return page, nil
}
