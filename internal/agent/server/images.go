package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/agent/attachment"
	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/edocsss/agent-whiteboard/internal/common"
)

func (s *Server) imageResource(response http.ResponseWriter, request *http.Request) {
	origin, clientID, conversationID, ok := s.authorizeImage(response, request)
	if !ok {
		return
	}
	if nilInterface(s.images) {
		safeHTTPError(response, http.StatusServiceUnavailable, protocol.ErrorBrokerUnavailable)
		return
	}
	allowOrigin(response.Header(), origin)

	if request.URL.Path == protocol.ImagesPath {
		if request.Method != http.MethodPost {
			safeHTTPError(response, http.StatusMethodNotAllowed, protocol.ErrorInvalidCommand)
			return
		}
		name := provider.Name(request.Header.Get(protocol.ProviderHeader))
		purpose := attachment.ImagePurpose(request.Header.Get(protocol.ImagePurposeHeader))
		if purpose == "" {
			purpose = attachment.PurposeAttachment
		}
		if !name.Valid() || (purpose != attachment.PurposeAttachment && purpose != attachment.PurposeInlineReference) || request.ContentLength > int64(protocol.MaxImageBytes) {
			if request.ContentLength > int64(protocol.MaxImageBytes) {
				safeHTTPError(response, http.StatusRequestEntityTooLarge, protocol.ErrorImageTooLarge)
			} else {
				safeHTTPError(response, http.StatusBadRequest, protocol.ErrorInvalidCommand)
			}
			return
		}
		staged, err := s.images.Stage(request.Context(), attachment.StageRequest{
			Origin: origin, Provider: name, ConversationID: conversationID, ClientID: clientID, Purpose: purpose,
			Content: io.LimitReader(request.Body, int64(protocol.MaxImageBytes)+1),
		})
		if err != nil {
			status, code := imageError(err)
			safeHTTPError(response, status, code)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(staged)
		return
	}

	imageID, valid := imageIDFromPath(request.URL.Path)
	if !valid {
		safeHTTPError(response, http.StatusNotFound, protocol.ErrorInvalidCommand)
		return
	}
	identity := attachment.ReadRequest{Origin: origin, ConversationID: conversationID, ClientID: clientID, ImageID: imageID}
	switch request.Method {
	case http.MethodGet:
		image, err := s.images.Read(request.Context(), identity)
		if err != nil {
			status, code := imageError(err)
			safeHTTPError(response, status, code)
			return
		}
		if !common.SupportedRasterMediaType(image.MediaType) || len(image.Content) == 0 || len(image.Content) > protocol.MaxImageBytes {
			safeHTTPError(response, http.StatusInternalServerError, protocol.ErrorImageStorageFailure)
			return
		}
		response.Header().Set("Content-Type", image.MediaType)
		response.Header().Set("Content-Length", strconv.Itoa(len(image.Content)))
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(image.Content)
	case http.MethodDelete:
		if err := s.images.DeleteStaged(request.Context(), identity); err != nil {
			status, code := imageError(err)
			safeHTTPError(response, status, code)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	default:
		safeHTTPError(response, http.StatusMethodNotAllowed, protocol.ErrorInvalidCommand)
	}
}

func (s *Server) authorizeImage(response http.ResponseWriter, request *http.Request) (string, string, string, bool) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, protocol.ErrorUntrustedOrigin)
		return "", "", "", false
	}
	if values := request.Header.Values(protocol.APIVersionHeader); len(values) != 1 || values[0] != protocol.APIVersion {
		safeHTTPError(response, http.StatusUpgradeRequired, protocol.ErrorIncompatibleAPI)
		return "", "", "", false
	}
	clientValues := request.Header.Values(protocol.ClientIDHeader)
	conversationValues := request.Header.Values(protocol.ConversationIDHeader)
	if len(clientValues) != 1 || len(conversationValues) != 1 || common.ValidateID(clientValues[0]) != nil || common.ValidateID(conversationValues[0]) != nil {
		safeHTTPError(response, http.StatusBadRequest, protocol.ErrorInvalidCommand)
		return "", "", "", false
	}
	if !s.attachments.has(attachmentKey{origin: origin, clientID: clientValues[0], conversationID: conversationValues[0]}) {
		safeHTTPError(response, http.StatusForbidden, protocol.ErrorUntrustedOrigin)
		return "", "", "", false
	}
	if request.Method == http.MethodPost {
		if values := request.Header.Values("Content-Type"); len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			safeHTTPError(response, http.StatusUnsupportedMediaType, protocol.ErrorImageUnsupported)
			return "", "", "", false
		}
		if values := request.Header.Values(protocol.ProviderHeader); len(values) != 1 {
			safeHTTPError(response, http.StatusBadRequest, protocol.ErrorInvalidCommand)
			return "", "", "", false
		}
		if values := request.Header.Values(protocol.ImagePurposeHeader); len(values) > 1 {
			safeHTTPError(response, http.StatusBadRequest, protocol.ErrorInvalidCommand)
			return "", "", "", false
		}
	} else if len(request.Header.Values(protocol.ProviderHeader)) != 0 || len(request.Header.Values(protocol.ImagePurposeHeader)) != 0 {
		safeHTTPError(response, http.StatusBadRequest, protocol.ErrorInvalidCommand)
		return "", "", "", false
	}
	return origin, clientValues[0], conversationValues[0], true
}

func imageIDFromPath(path string) (string, bool) {
	prefix := protocol.ImagesPath + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(path, prefix)
	return id, !strings.ContainsRune(id, '/') && common.ValidateID(id) == nil
}

func imageError(err error) (int, protocol.BrowserErrorCode) {
	switch {
	case errors.Is(err, attachment.ErrUnsupported):
		return http.StatusUnsupportedMediaType, protocol.ErrorImageUnsupported
	case errors.Is(err, attachment.ErrImageTooLarge):
		return http.StatusRequestEntityTooLarge, protocol.ErrorImageTooLarge
	case errors.Is(err, attachment.ErrTurnLimit):
		return http.StatusRequestEntityTooLarge, protocol.ErrorImageTurnLimit
	case errors.Is(err, attachment.ErrWorkspaceLimit):
		return http.StatusInsufficientStorage, protocol.ErrorImageWorkspaceLimit
	case errors.Is(err, attachment.ErrMissing):
		return http.StatusNotFound, protocol.ErrorImageMissing
	case errors.Is(err, attachment.ErrInvalid):
		return http.StatusBadRequest, protocol.ErrorInvalidCommand
	default:
		return http.StatusInternalServerError, protocol.ErrorImageStorageFailure
	}
}
