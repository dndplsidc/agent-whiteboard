package localapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/agentattachment"
	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/edocsss/agent-whiteboard/internal/raster"
)

func (s *Server) imageResource(response http.ResponseWriter, request *http.Request) {
	origin, clientID, conversationID, ok := s.authorizeImage(response, request)
	if !ok {
		return
	}
	if nilInterface(s.images) {
		safeHTTPError(response, http.StatusServiceUnavailable, agentprotocol.ErrorBrokerUnavailable)
		return
	}
	allowOrigin(response.Header(), origin)

	if request.URL.Path == agentprotocol.ImagesPath {
		if request.Method != http.MethodPost {
			safeHTTPError(response, http.StatusMethodNotAllowed, agentprotocol.ErrorInvalidCommand)
			return
		}
		name := provider.Name(request.Header.Get(agentprotocol.ProviderHeader))
		if !name.Valid() || request.ContentLength > int64(agentprotocol.MaxImageBytes) {
			if request.ContentLength > int64(agentprotocol.MaxImageBytes) {
				safeHTTPError(response, http.StatusRequestEntityTooLarge, agentprotocol.ErrorImageTooLarge)
			} else {
				safeHTTPError(response, http.StatusBadRequest, agentprotocol.ErrorInvalidCommand)
			}
			return
		}
		staged, err := s.images.Stage(request.Context(), agentattachment.StageRequest{
			Origin: origin, Provider: name, ConversationID: conversationID, ClientID: clientID,
			Content: io.LimitReader(request.Body, int64(agentprotocol.MaxImageBytes)+1),
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
		safeHTTPError(response, http.StatusNotFound, agentprotocol.ErrorInvalidCommand)
		return
	}
	identity := agentattachment.ReadRequest{Origin: origin, ConversationID: conversationID, ClientID: clientID, ImageID: imageID}
	switch request.Method {
	case http.MethodGet:
		image, err := s.images.Read(request.Context(), identity)
		if err != nil {
			status, code := imageError(err)
			safeHTTPError(response, status, code)
			return
		}
		if !raster.SupportedMediaType(image.MediaType) || len(image.Content) == 0 || len(image.Content) > agentprotocol.MaxImageBytes {
			safeHTTPError(response, http.StatusInternalServerError, agentprotocol.ErrorImageStorageFailure)
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
		safeHTTPError(response, http.StatusMethodNotAllowed, agentprotocol.ErrorInvalidCommand)
	}
}

func (s *Server) authorizeImage(response http.ResponseWriter, request *http.Request) (string, string, string, bool) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return "", "", "", false
	}
	if values := request.Header.Values(agentprotocol.APIVersionHeader); len(values) != 1 || values[0] != agentprotocol.APIVersion {
		safeHTTPError(response, http.StatusUpgradeRequired, agentprotocol.ErrorIncompatibleAPI)
		return "", "", "", false
	}
	clientValues := request.Header.Values(agentprotocol.ClientIDHeader)
	conversationValues := request.Header.Values(agentprotocol.ConversationIDHeader)
	if len(clientValues) != 1 || len(conversationValues) != 1 || common.ValidateID(clientValues[0]) != nil || common.ValidateID(conversationValues[0]) != nil {
		safeHTTPError(response, http.StatusBadRequest, agentprotocol.ErrorInvalidCommand)
		return "", "", "", false
	}
	if !s.attachments.has(attachmentKey{origin: origin, clientID: clientValues[0], conversationID: conversationValues[0]}) {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return "", "", "", false
	}
	if request.Method == http.MethodPost {
		if values := request.Header.Values("Content-Type"); len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			safeHTTPError(response, http.StatusUnsupportedMediaType, agentprotocol.ErrorImageUnsupported)
			return "", "", "", false
		}
		if values := request.Header.Values(agentprotocol.ProviderHeader); len(values) != 1 {
			safeHTTPError(response, http.StatusBadRequest, agentprotocol.ErrorInvalidCommand)
			return "", "", "", false
		}
	} else if len(request.Header.Values(agentprotocol.ProviderHeader)) != 0 {
		safeHTTPError(response, http.StatusBadRequest, agentprotocol.ErrorInvalidCommand)
		return "", "", "", false
	}
	return origin, clientValues[0], conversationValues[0], true
}

func imageIDFromPath(path string) (string, bool) {
	prefix := agentprotocol.ImagesPath + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(path, prefix)
	return id, !strings.ContainsRune(id, '/') && common.ValidateID(id) == nil
}

func imageError(err error) (int, agentprotocol.BrowserErrorCode) {
	switch {
	case errors.Is(err, agentattachment.ErrUnsupported):
		return http.StatusUnsupportedMediaType, agentprotocol.ErrorImageUnsupported
	case errors.Is(err, agentattachment.ErrImageTooLarge):
		return http.StatusRequestEntityTooLarge, agentprotocol.ErrorImageTooLarge
	case errors.Is(err, agentattachment.ErrTurnLimit):
		return http.StatusRequestEntityTooLarge, agentprotocol.ErrorImageTurnLimit
	case errors.Is(err, agentattachment.ErrWorkspaceLimit):
		return http.StatusInsufficientStorage, agentprotocol.ErrorImageWorkspaceLimit
	case errors.Is(err, agentattachment.ErrMissing):
		return http.StatusNotFound, agentprotocol.ErrorImageMissing
	case errors.Is(err, agentattachment.ErrInvalid):
		return http.StatusBadRequest, agentprotocol.ErrorInvalidCommand
	default:
		return http.StatusInternalServerError, agentprotocol.ErrorImageStorageFailure
	}
}
