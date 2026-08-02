package broker

import (
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const (
	MaxQueueItems = agentprotocol.MaxQueueItems
	MaxQueueBytes = agentprotocol.MaxQueueBytes
)

// QueuedTurn is the broker-owned form of one follow-up. Context is copied on
// enqueue and remains private to the queue until a turn is dequeued.
type QueuedTurn struct {
	TurnID    string
	MessageID string
	Message   string
	Context   *provider.PageContext
}

type queueItem struct {
	turnID    string
	messageID string
	message   string
	context   *provider.PageContext
}

// Queue is a pure, non-concurrent FIFO. Its byte bound counts UTF-8 message
// bytes exactly as the browser protocol does.
type Queue struct {
	items      []queueItem
	bytes      int
	hasContext bool
}

func NewQueue() *Queue { return &Queue{} }
func (queue *Queue) Len() int {
	if queue == nil {
		return 0
	}
	return len(queue.items)
}
func (queue *Queue) Bytes() int {
	if queue == nil {
		return 0
	}
	return queue.bytes
}
func (queue *Queue) Empty() bool { return queue == nil || len(queue.items) == 0 }

func (queue *Queue) ContainsTurnID(turnID string) bool {
	if queue == nil {
		return false
	}
	for _, item := range queue.items {
		if item.turnID == turnID {
			return true
		}
	}
	return false
}

func (queue *Queue) ContainsMessageID(messageID string) bool {
	if queue == nil {
		return false
	}
	for _, item := range queue.items {
		if item.messageID == messageID {
			return true
		}
	}
	return false
}

// attachContextToHead transfers one pending revision to the next queued turn.
// This preserves FIFO ordering when a revision is observed after follow-ups
// were already queued: the replacement precedes the next reader message.
func (queue *Queue) attachContextToHead(page *provider.PageContext) error {
	if queue == nil || len(queue.items) == 0 || page == nil || page.Validate() != nil || queue.hasContext {
		return ErrQueueContextConflict
	}
	queue.items[0].context = page
	queue.hasContext = true
	return nil
}

func (queue *Queue) discardContext() {
	if queue == nil || !queue.hasContext {
		return
	}
	for index := range queue.items {
		if queue.items[index].context == nil {
			continue
		}
		zeroProviderContext(queue.items[index].context)
		queue.items[index].context = nil
	}
	queue.hasContext = false
}

// Enqueue copies one broker-owned follow-up. At most one queued turn may hold
// complete page context, bounding retained context independently from messages.
func (queue *Queue) Enqueue(turn QueuedTurn) error {
	if queue == nil {
		return errors.New("nil queue")
	}
	if err := turn.validate(); err != nil {
		return err
	}
	for _, item := range queue.items {
		if item.turnID == turn.TurnID || item.messageID == turn.MessageID {
			return ErrQueueDuplicateID
		}
	}
	if turn.Context != nil && queue.hasContext {
		return ErrQueueContextConflict
	}
	if len(queue.items) >= MaxQueueItems || queue.bytes+len(turn.Message) > MaxQueueBytes {
		return ErrQueueFull
	}
	queue.items = append(queue.items, queueItem{
		turnID:    turn.TurnID,
		messageID: turn.MessageID,
		message:   turn.Message,
		context:   cloneProviderContext(turn.Context),
	})
	queue.bytes += len(turn.Message)
	queue.hasContext = queue.hasContext || turn.Context != nil
	return nil
}

func (turn QueuedTurn) validate() error {
	request := provider.TurnRequest{TurnID: turn.TurnID, MessageID: turn.MessageID, Message: turn.Message, Context: turn.Context}
	if err := request.Validate(); err != nil {
		return ErrQueueInvalid
	}
	return nil
}

// Edit changes only a still-queued message. Turn IDs and context are not
// editable, and the aggregate bound is checked before mutation.
func (queue *Queue) Edit(messageID, message string) error {
	if queue == nil {
		return errors.New("nil queue")
	}
	if err := (provider.TurnRequest{TurnID: validQueueID, MessageID: messageID, Message: message}).Validate(); err != nil {
		// The synthetic turn ID above is valid and exists only to reuse provider
		// message validation without duplicating its UTF-8/control-byte rules.
		if messageID == "" || len(message) == 0 {
			return ErrQueueInvalid
		}
		return ErrQueueInvalid
	}
	for index := range queue.items {
		if queue.items[index].messageID != messageID {
			continue
		}
		newBytes := queue.bytes - len(queue.items[index].message) + len(message)
		if newBytes > MaxQueueBytes {
			return ErrQueueFull
		}
		queue.bytes = newBytes
		queue.items[index].message = message
		return nil
	}
	return ErrQueueItemNotFound
}

// Remove drops one still-queued message and best-effort erases any page bytes
// owned by the queue before releasing the item.
func (queue *Queue) Remove(messageID string) error {
	if queue == nil {
		return errors.New("nil queue")
	}
	for index := range queue.items {
		if queue.items[index].messageID != messageID {
			continue
		}
		item := queue.items[index]
		if item.context != nil {
			queue.hasContext = false
		}
		zeroProviderContext(item.context)
		queue.bytes -= len(item.message)
		copy(queue.items[index:], queue.items[index+1:])
		queue.items[len(queue.items)-1] = queueItem{}
		queue.items = queue.items[:len(queue.items)-1]
		return nil
	}
	return ErrQueueItemNotFound
}

// peek exposes the actor-owned head only for synchronous dispatch preparation.
// Callers must not retain or mutate the returned context before Dequeue.
func (queue *Queue) peek() (provider.TurnRequest, bool) {
	if queue == nil || len(queue.items) == 0 {
		return provider.TurnRequest{}, false
	}
	item := queue.items[0]
	return provider.TurnRequest{TurnID: item.turnID, MessageID: item.messageID, Message: item.message, Context: item.context}, true
}

// Dequeue transfers ownership of the context to the caller. The queue itself
// no longer retains those bytes; unlike Remove, the transferred request must
// remain usable for provider submission.
func (queue *Queue) Dequeue() (provider.TurnRequest, bool) {
	if queue == nil || len(queue.items) == 0 {
		return provider.TurnRequest{}, false
	}
	item := queue.items[0]
	queue.items[0] = queueItem{}
	queue.items = queue.items[1:]
	queue.bytes -= len(item.message)
	if item.context != nil {
		queue.hasContext = false
	}
	return provider.TurnRequest{TurnID: item.turnID, MessageID: item.messageID, Message: item.message, Context: item.context}, true
}

// Items exposes only browser-safe queue values. Context bytes never enter the
// replay or protocol representation.
func (queue *Queue) Items() []agentprotocol.QueueItem {
	if queue == nil {
		return []agentprotocol.QueueItem{}
	}
	items := make([]agentprotocol.QueueItem, len(queue.items))
	for index, item := range queue.items {
		items[index] = agentprotocol.QueueItem{TurnID: item.turnID, MessageID: item.messageID, Message: item.message}
	}
	return items
}

// Clear releases every queued item and erases owned page buffers.
func (queue *Queue) Clear() {
	if queue == nil {
		return
	}
	for index := range queue.items {
		zeroProviderContext(queue.items[index].context)
		queue.items[index] = queueItem{}
	}
	queue.items = nil
	queue.bytes = 0
	queue.hasContext = false
}

func cloneProviderContext(context *provider.PageContext) *provider.PageContext {
	if context == nil {
		return nil
	}
	copyOfContext := *context
	copyOfContext.Markdown = cloneBytes(context.Markdown)
	copyOfContext.CreatorContext = cloneBytes(context.CreatorContext)
	copyOfContext.Resource.ExpiresAt = cloneTimePointer(context.Resource.ExpiresAt)
	return &copyOfContext
}

func zeroProviderContext(context *provider.PageContext) {
	if context == nil {
		return
	}
	for index := range context.Markdown {
		context.Markdown[index] = 0
	}
	for index := range context.CreatorContext {
		context.CreatorContext[index] = 0
	}
	context.Markdown = nil
	context.CreatorContext = nil
	context.Title = ""
	context.URL = ""
	context.Digest = ""
	context.Resource = provider.Resource{}
}

// common.ValidateID's valid shape is intentionally mirrored by this constant
// only for provider message validation in Edit. It is a canonical 32-byte ID.
const validQueueID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
