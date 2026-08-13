package broker

import (
	"errors"
	"reflect"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

const (
	MaxQueueItems = protocol.MaxQueueItems
	MaxQueueBytes = protocol.MaxQueueBytes
)

// QueuedTurn is the broker-owned form of one follow-up. Context is copied on
// enqueue and remains private to the queue until a turn is dequeued.
type QueuedTurn struct {
	TurnID       string
	MessageID    string
	Content      provider.MessageContent
	Images       []provider.ImageInput
	Descriptors  []protocol.ImageDescriptor
	Context      *provider.PageContext
	Settings     *provider.ExecutionSettings
	Presentation *provider.ModelPresentation
}

type queueItem struct {
	turnID       string
	messageID    string
	content      provider.MessageContent
	images       []provider.ImageInput
	descriptors  []protocol.ImageDescriptor
	context      *provider.PageContext
	settings     *provider.ExecutionSettings
	presentation *provider.ModelPresentation
}

// Queue is a pure, non-concurrent FIFO. Its byte bound counts UTF-8 message
// bytes exactly as the browser protocol does.
type Queue struct {
	items      []queueItem
	bytes      int
	hasContext bool
}

func NewQueue() *Queue { return &Queue{} }

func cloneProviderSettings(settings *provider.ExecutionSettings) *provider.ExecutionSettings {
	if settings == nil {
		return nil
	}
	copyOfSettings := *settings
	return &copyOfSettings
}

func cloneProviderPresentation(presentation *provider.ModelPresentation) *provider.ModelPresentation {
	if presentation == nil {
		return nil
	}
	copyOfPresentation := *presentation
	return &copyOfPresentation
}

func providerSettingsBytes(settings *provider.ExecutionSettings, presentation *provider.ModelPresentation) int {
	if settings == nil || presentation == nil {
		return 0
	}
	return len(settings.Model) + len(settings.Effort) + len(settings.Speed) + len(presentation.ModelDisplayName)
}

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
	settingsBytes := providerSettingsBytes(turn.Settings, turn.Presentation)
	if len(queue.items) >= MaxQueueItems || queue.bytes+turn.Content.SemanticBytes()+settingsBytes > MaxQueueBytes {
		return ErrQueueFull
	}
	queue.items = append(queue.items, queueItem{
		turnID:       turn.TurnID,
		messageID:    turn.MessageID,
		content:      turn.Content.Clone(),
		images:       append([]provider.ImageInput(nil), turn.Images...),
		descriptors:  append([]protocol.ImageDescriptor(nil), turn.Descriptors...),
		context:      cloneProviderContext(turn.Context),
		settings:     cloneProviderSettings(turn.Settings),
		presentation: cloneProviderPresentation(turn.Presentation),
	})
	queue.bytes += turn.Content.SemanticBytes() + settingsBytes
	queue.hasContext = queue.hasContext || turn.Context != nil
	return nil
}

func (turn QueuedTurn) validate() error {
	request := provider.TurnRequest{TurnID: turn.TurnID, MessageID: turn.MessageID, Content: turn.Content, Images: turn.Images, Context: turn.Context, Settings: turn.Settings}
	if err := request.Validate(); err != nil || (turn.Settings == nil) != (turn.Presentation == nil) || turn.Presentation != nil && turn.Presentation.Validate() != nil {
		return ErrQueueInvalid
	}
	if len(turn.Images) != len(turn.Descriptors) {
		return ErrQueueInvalid
	}
	for index := range turn.Images {
		image := turn.Images[index]
		descriptor := turn.Descriptors[index]
		if descriptor.ImageID != image.ID || descriptor.Name != image.Name || descriptor.MediaType != image.MediaType {
			return ErrQueueInvalid
		}
	}
	return nil
}

// Edit changes only a still-queued message. Turn IDs and context are not
// editable, and the aggregate bound is checked before mutation.
func (queue *Queue) Edit(messageID string, content provider.MessageContent) error {
	_, err := queue.EditAndRemovedImages(messageID, content)
	return err
}

func (queue *Queue) EditAndRemovedImages(messageID string, content provider.MessageContent) ([]string, error) {
	if queue == nil {
		return nil, errors.New("nil queue")
	}
	for index := range queue.items {
		if queue.items[index].messageID != messageID {
			continue
		}
		if !referencesAreImmutable(queue.items[index].content, content) {
			return nil, ErrQueueInvalid
		}
		images, descriptors, ok := reorderEditedImages(queue.items[index], content)
		if !ok || (provider.TurnRequest{TurnID: validQueueID, MessageID: messageID, Content: content, Images: images, Settings: queue.items[index].settings}).Validate() != nil {
			return nil, ErrQueueInvalid
		}
		newBytes := queue.bytes - queue.items[index].content.SemanticBytes() + content.SemanticBytes()
		if newBytes > MaxQueueBytes {
			return nil, ErrQueueFull
		}
		retained := make(map[string]struct{})
		for _, id := range content.InlineImageIDs() {
			retained[id] = struct{}{}
		}
		removed := make([]string, 0)
		for _, id := range queue.items[index].content.InlineImageIDs() {
			if _, exists := retained[id]; !exists {
				removed = append(removed, id)
			}
		}
		queue.bytes = newBytes
		queue.items[index].content = content.Clone()
		queue.items[index].images = images
		queue.items[index].descriptors = descriptors
		return removed, nil
	}
	return nil, ErrQueueItemNotFound
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
		queue.bytes -= item.content.SemanticBytes() + providerSettingsBytes(item.settings, item.presentation)
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
	return provider.TurnRequest{TurnID: item.turnID, MessageID: item.messageID, Content: item.content.Clone(), Images: append([]provider.ImageInput(nil), item.images...), Context: item.context, Settings: cloneProviderSettings(item.settings)}, true
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
	queue.bytes -= item.content.SemanticBytes() + providerSettingsBytes(item.settings, item.presentation)
	if item.context != nil {
		queue.hasContext = false
	}
	return provider.TurnRequest{TurnID: item.turnID, MessageID: item.messageID, Content: item.content, Images: append([]provider.ImageInput(nil), item.images...), Context: item.context, Settings: item.settings}, true
}

// Items exposes only browser-safe queue values. Context bytes never enter the
// replay or protocol representation.
func (queue *Queue) Items() []protocol.QueueItem {
	if queue == nil {
		return []protocol.QueueItem{}
	}
	items := make([]protocol.QueueItem, len(queue.items))
	for index, item := range queue.items {
		content, err := messageContentFromProvider(item.content, item.descriptors)
		if err != nil {
			return []protocol.QueueItem{}
		}
		items[index] = protocol.QueueItem{TurnID: item.turnID, MessageID: item.messageID, Content: content, Images: ordinaryImageDescriptors(item.content, item.descriptors), Settings: presentedProtocolSettings(item.settings, item.presentation)}
	}
	return items
}

func referencesAreImmutable(before, after provider.MessageContent) bool {
	old := make(map[string]provider.ContextReference)
	for _, part := range before.Parts {
		if part.Reference != nil {
			reference := *part.Reference
			if reference.Visual != nil {
				reference.Visual.Ordinal = 0
			}
			old[reference.ID] = reference
		}
	}
	for _, part := range after.Parts {
		if part.Reference == nil {
			continue
		}
		reference := *part.Reference
		if reference.Visual != nil {
			reference.Visual.Ordinal = 0
		}
		previous, exists := old[reference.ID]
		if !exists || !reflect.DeepEqual(previous, reference) {
			return false
		}
	}
	return true
}

func reorderEditedImages(item queueItem, content provider.MessageContent) ([]provider.ImageInput, []protocol.ImageDescriptor, bool) {
	inputs := make(map[string]provider.ImageInput, len(item.images))
	descriptors := make(map[string]protocol.ImageDescriptor, len(item.descriptors))
	for index, input := range item.images {
		inputs[input.ID] = input
		descriptors[input.ID] = item.descriptors[index]
	}
	oldInline := make(map[string]struct{})
	for _, id := range item.content.InlineImageIDs() {
		oldInline[id] = struct{}{}
	}
	orderedIDs := append([]string(nil), content.InlineImageIDs()...)
	for _, input := range item.images {
		if _, inline := oldInline[input.ID]; !inline {
			orderedIDs = append(orderedIDs, input.ID)
		}
	}
	resultInputs := make([]provider.ImageInput, 0, len(orderedIDs))
	resultDescriptors := make([]protocol.ImageDescriptor, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		input, inputExists := inputs[id]
		descriptor, descriptorExists := descriptors[id]
		if !inputExists || !descriptorExists {
			return nil, nil, false
		}
		resultInputs = append(resultInputs, input)
		resultDescriptors = append(resultDescriptors, descriptor)
	}
	return resultInputs, resultDescriptors, true
}

func ordinaryImageDescriptors(content provider.MessageContent, descriptors []protocol.ImageDescriptor) []protocol.ImageDescriptor {
	inline := make(map[string]struct{})
	for _, id := range content.InlineImageIDs() {
		inline[id] = struct{}{}
	}
	result := make([]protocol.ImageDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if _, exists := inline[descriptor.ImageID]; !exists {
			result = append(result, descriptor)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (queue *Queue) imageMessageIDs() []string {
	if queue == nil {
		return nil
	}
	result := make([]string, 0, len(queue.items))
	for _, item := range queue.items {
		if len(item.images) != 0 {
			result = append(result, item.messageID)
		}
	}
	return result
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
