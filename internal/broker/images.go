package broker

import (
	"context"
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/agentattachment"
	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

func (actor *conversation) claimTurnImages(command agentprotocol.Command, payload agentprotocol.SubmitPayload) ([]provider.ImageInput, []agentprotocol.ImageDescriptor, agentprotocol.BrowserErrorCode) {
	inline := payload.Content.InlineImages()
	images := append(append([]agentprotocol.ImageReference(nil), inline...), payload.Images...)
	if len(images) == 0 {
		return nil, nil, ""
	}
	if !actor.session.capabilities.Images {
		return nil, nil, agentprotocol.ErrorImageInputUnsupported
	}
	if common.IsNil(actor.attachments) || actor.mapping.Current == nil {
		return nil, nil, agentprotocol.ErrorImageStorageFailure
	}
	claimed, err := actor.attachments.Claim(actor.lifecycleCtx, agentattachment.ClaimRequest{
		Origin:         actor.identity.Origin,
		Provider:       actor.identity.Provider,
		ConversationID: actor.mapping.Current.ConversationID,
		ClientID:       command.ClientID,
		TurnID:         payload.TurnID,
		MessageID:      payload.MessageID,
		Images:         images,
	})
	if err != nil {
		return nil, nil, mapAttachmentError(err)
	}
	return claimed.Inputs, claimed.Descriptors, ""
}

func (actor *conversation) messageImages(messageID string) ([]agentprotocol.ImageDescriptor, error) {
	if common.IsNil(actor.attachments) || actor.mapping.Current == nil {
		return []agentprotocol.ImageDescriptor{}, nil
	}
	return actor.attachments.ImagesForMessage(actor.lifecycleCtx, actor.mapping.Current.ConversationID, messageID)
}

func (actor *conversation) releaseMessageImages(messageID string) error {
	if common.IsNil(actor.attachments) || actor.mapping.Current == nil {
		return nil
	}
	return actor.attachments.ReleaseMessage(context.WithoutCancel(actor.lifecycleCtx), actor.mapping.Current.ConversationID, messageID)
}

func removeImageWorkspace(ctx context.Context, attachments AttachmentStore, state StateStore, conversationID string) error {
	if !common.IsNil(attachments) {
		return attachments.RemoveWorkspace(ctx, conversationID)
	}
	return state.RemoveWorkspace(conversationID)
}

func mapAttachmentError(err error) agentprotocol.BrowserErrorCode {
	switch {
	case errors.Is(err, agentattachment.ErrUnsupported):
		return agentprotocol.ErrorImageUnsupported
	case errors.Is(err, agentattachment.ErrImageTooLarge):
		return agentprotocol.ErrorImageTooLarge
	case errors.Is(err, agentattachment.ErrTurnLimit):
		return agentprotocol.ErrorImageTurnLimit
	case errors.Is(err, agentattachment.ErrWorkspaceLimit):
		return agentprotocol.ErrorImageWorkspaceLimit
	case errors.Is(err, agentattachment.ErrMissing):
		return agentprotocol.ErrorImageMissing
	case errors.Is(err, agentattachment.ErrInvalid):
		return agentprotocol.ErrorInvalidCommand
	default:
		return agentprotocol.ErrorImageStorageFailure
	}
}
