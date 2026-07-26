package agentwb

import (
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/image"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
)

type Whiteboard = whiteboard.Whiteboard
type WhiteboardKind = whiteboard.Kind
type CreateWhiteboardInput = whiteboard.CreateInput
type UpdateWhiteboardInput = whiteboard.UpdateInput
type WhiteboardResult = whiteboard.Result

// UncertainCreateError reports that a failed create may still have committed.
// Create methods return the generated WhiteboardResult alongside this error so
// callers retain the capability ID and can check or remove the resource.
type UncertainCreateError = whiteboard.UncertainCreateError

const (
	KindMarkdown = whiteboard.KindMarkdown
	KindHTML     = whiteboard.KindHTML
)

type Image = image.Image
type ImageUpload = image.Upload
type CreateImagesInput = image.CreateInput
type UpdateImageInput = image.UpdateInput
type ImageResult = image.Result

type Clock = common.Clock
type IDGenerator = common.IDGenerator
