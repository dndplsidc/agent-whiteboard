package localapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
)

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setCommonHeaders(response.Header())
	if _, err := s.requestOrigin(request); err != nil {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}
	if s.isStopping() {
		safeHTTPError(response, http.StatusServiceUnavailable, agentprotocol.ErrorBrokerShuttingDown)
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.RawPath != "" || request.URL.Fragment != "" {
		safeHTTPError(response, http.StatusNotFound, agentprotocol.ErrorInvalidCommand)
		return
	}
	if request.Method == http.MethodOptions {
		s.preflight(response, request)
		return
	}

	switch request.URL.Path {
	case agentprotocol.StatusPath:
		if request.Method != http.MethodGet {
			safeHTTPError(response, http.StatusMethodNotAllowed, agentprotocol.ErrorInvalidCommand)
			return
		}
		s.status(response, request)
	case agentprotocol.ConnectPath:
		switch request.Method {
		case http.MethodGet:
			s.websocket(response, request)
		case http.MethodPost:
			s.stream(response, request)
		default:
			safeHTTPError(response, http.StatusMethodNotAllowed, agentprotocol.ErrorInvalidCommand)
		}
	case CommandsPath:
		if request.Method != http.MethodPost {
			safeHTTPError(response, http.StatusMethodNotAllowed, agentprotocol.ErrorInvalidCommand)
			return
		}
		s.command(response, request)
	default:
		safeHTTPError(response, http.StatusNotFound, agentprotocol.ErrorInvalidCommand)
	}
}

func (s *Server) status(response http.ResponseWriter, request *http.Request) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}
	allowOrigin(response.Header(), origin)
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(agentprotocol.NewStatusResponse(s.trusted(request.Context(), origin)))
}

func validateMutationHeaders(request *http.Request) (int, agentprotocol.BrowserErrorCode, bool) {
	if values := request.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		return http.StatusUnsupportedMediaType, agentprotocol.ErrorInvalidCommand, false
	}
	if values := request.Header.Values(agentprotocol.APIVersionHeader); len(values) != 1 || values[0] != agentprotocol.APIVersion {
		return http.StatusUpgradeRequired, agentprotocol.ErrorIncompatibleAPI, false
	}
	return 0, "", true
}

func decodeCommandBody(request *http.Request) (agentprotocol.Command, error) {
	if request.ContentLength > agentprotocol.MaxContextCommandBytes {
		return agentprotocol.Command{}, agentprotocol.ErrMessageTooLarge
	}
	reader := io.LimitReader(request.Body, int64(agentprotocol.MaxContextCommandBytes)+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return agentprotocol.Command{}, err
	}
	if len(body) > agentprotocol.MaxContextCommandBytes {
		return agentprotocol.Command{}, agentprotocol.ErrMessageTooLarge
	}
	return agentprotocol.DecodeCommand(body)
}

func safeHTTPError(response http.ResponseWriter, status int, code agentprotocol.BrowserErrorCode) {
	setCommonHeaders(response.Header())
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(struct {
		Error agentprotocol.BrowserError `json:"error"`
	}{agentprotocol.NewBrowserError(code)})
}

func statusForDecodeError(err error) int {
	if errors.Is(err, agentprotocol.ErrMessageTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
