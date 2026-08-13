package broker

import (
	"context"
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agent/attachment"
	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

func (actor *conversation) claimTurnImages(command protocol.Command, payload protocol.SubmitPayload) ([]provider.ImageInput, []protocol.ImageDescriptor, protocol.BrowserErrorCode) {
	inline := payload.Content.InlineImages()
	images := append(append([]protocol.ImageReference(nil), inline...), payload.Images...)
	if len(images) == 0 {
		return nil, nil, ""
	}
	if !actor.draftSupportsImages(providerSettings(payload.Settings)) {
		return nil, nil, protocol.ErrorImageInputUnsupported
	}
	if common.IsNil(actor.attachments) || actor.mapping.Current == nil {
		return nil, nil, protocol.ErrorImageStorageFailure
	}
	claimed, err := actor.attachments.Claim(actor.lifecycleCtx, attachment.ClaimRequest{
		Origin:         actor.identity.Origin,
		Provider:       actor.identity.Provider,
		ConversationID: actor.mapping.Current.ConversationID,
		ClientID:       command.ClientID,
		TurnID:         payload.TurnID,
		MessageID:      payload.MessageID,
		Images:         images,
		InlineImages:   len(inline),
	})
	if err != nil {
		return nil, nil, mapAttachmentError(err)
	}
	return claimed.Inputs, claimed.Descriptors, ""
}

func (actor *conversation) messageImages(messageID string) ([]protocol.ImageDescriptor, error) {
	if common.IsNil(actor.attachments) || actor.mapping.Current == nil {
		return []protocol.ImageDescriptor{}, nil
	}
	return actor.attachments.ImagesForMessage(actor.lifecycleCtx, actor.mapping.Current.ConversationID, messageID)
}

func (actor *conversation) releaseMessageImages(messageID string) error {
	if common.IsNil(actor.attachments) || actor.mapping.Current == nil {
		return nil
	}
	return actor.attachments.ReleaseMessage(context.WithoutCancel(actor.lifecycleCtx), actor.mapping.Current.ConversationID, messageID)
}

func (actor *conversation) releaseMessageImageSubset(messageID string, imageIDs []string) error {
	if common.IsNil(actor.attachments) || actor.mapping.Current == nil || len(imageIDs) == 0 {
		return nil
	}
	releaser, ok := actor.attachments.(interface {
		ReleaseImages(context.Context, string, string, []string) error
	})
	if !ok {
		return errors.New("attachment store cannot release individual images")
	}
	return releaser.ReleaseImages(context.WithoutCancel(actor.lifecycleCtx), actor.mapping.Current.ConversationID, messageID, imageIDs)
}

func removeImageWorkspace(ctx context.Context, attachments AttachmentStore, state StateStore, conversationID string) error {
	if !common.IsNil(attachments) {
		return attachments.RemoveWorkspace(ctx, conversationID)
	}
	return state.RemoveWorkspace(conversationID)
}

func mapAttachmentError(err error) protocol.BrowserErrorCode {
	switch {
	case errors.Is(err, attachment.ErrUnsupported):
		return protocol.ErrorImageUnsupported
	case errors.Is(err, attachment.ErrImageTooLarge):
		return protocol.ErrorImageTooLarge
	case errors.Is(err, attachment.ErrTurnLimit):
		return protocol.ErrorImageTurnLimit
	case errors.Is(err, attachment.ErrWorkspaceLimit):
		return protocol.ErrorImageWorkspaceLimit
	case errors.Is(err, attachment.ErrMissing):
		return protocol.ErrorImageMissing
	case errors.Is(err, attachment.ErrInvalid):
		return protocol.ErrorInvalidCommand
	default:
		return protocol.ErrorImageStorageFailure
	}
}
