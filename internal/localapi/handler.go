package localapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
)

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if !nilInterface(s.recorder) {
		recorded := &recordingResponseWriter{ResponseWriter: response}
		defer func() {
			status := recorded.status
			if status == 0 {
				status = http.StatusOK
			}
			s.recorder.Record(RequestRecord{Route: canonicalRoute(request.URL.Path), Method: request.Method, Status: status, Code: recorded.code})
		}()
		response = recorded
	}
	s.serveHTTP(response, request)
}

func (s *Server) serveHTTP(response http.ResponseWriter, request *http.Request) {
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
		if request.URL.Path == agentprotocol.ImagesPath || strings.HasPrefix(request.URL.Path, agentprotocol.ImagesPath+"/") {
			s.imageResource(response, request)
			return
		}
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
	if classified, ok := response.(interface {
		setBrowserErrorCode(agentprotocol.BrowserErrorCode)
	}); ok {
		classified.setBrowserErrorCode(code)
	}
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

func canonicalRoute(path string) string {
	switch path {
	case agentprotocol.StatusPath, agentprotocol.ConnectPath, CommandsPath:
		return path
	default:
		if path == agentprotocol.ImagesPath || strings.HasPrefix(path, agentprotocol.ImagesPath+"/") {
			return agentprotocol.ImagesPath
		}
		return "unknown"
	}
}

type recordingResponseWriter struct {
	http.ResponseWriter
	status int
	code   agentprotocol.BrowserErrorCode
}

func (w *recordingResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *recordingResponseWriter) Flush() { _ = w.FlushError() }

func (w *recordingResponseWriter) FlushError() error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *recordingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil && w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return connection, buffered, err
}

func (w *recordingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recordingResponseWriter) setBrowserErrorCode(code agentprotocol.BrowserErrorCode) {
	w.code = code
}
