package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
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
		safeHTTPError(response, http.StatusForbidden, protocol.ErrorUntrustedOrigin)
		return
	}
	if s.isStopping() {
		safeHTTPError(response, http.StatusServiceUnavailable, protocol.ErrorBrokerShuttingDown)
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.RawPath != "" || request.URL.Fragment != "" {
		safeHTTPError(response, http.StatusNotFound, protocol.ErrorInvalidCommand)
		return
	}
	if request.Method == http.MethodOptions {
		s.preflight(response, request)
		return
	}

	switch request.URL.Path {
	case protocol.StatusPath:
		if request.Method != http.MethodGet {
			safeHTTPError(response, http.StatusMethodNotAllowed, protocol.ErrorInvalidCommand)
			return
		}
		s.status(response, request)
	case protocol.ConnectPath:
		switch request.Method {
		case http.MethodGet:
			s.websocket(response, request)
		case http.MethodPost:
			s.stream(response, request)
		default:
			safeHTTPError(response, http.StatusMethodNotAllowed, protocol.ErrorInvalidCommand)
		}
	case CommandsPath:
		if request.Method != http.MethodPost {
			safeHTTPError(response, http.StatusMethodNotAllowed, protocol.ErrorInvalidCommand)
			return
		}
		s.command(response, request)
	default:
		if request.URL.Path == protocol.ImagesPath || strings.HasPrefix(request.URL.Path, protocol.ImagesPath+"/") {
			s.imageResource(response, request)
			return
		}
		safeHTTPError(response, http.StatusNotFound, protocol.ErrorInvalidCommand)
	}
}

func (s *Server) status(response http.ResponseWriter, request *http.Request) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, protocol.ErrorUntrustedOrigin)
		return
	}
	allowOrigin(response.Header(), origin)
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(protocol.NewStatusResponse(s.trusted(request.Context(), origin)))
}

func validateMutationHeaders(request *http.Request) (int, protocol.BrowserErrorCode, bool) {
	if values := request.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		return http.StatusUnsupportedMediaType, protocol.ErrorInvalidCommand, false
	}
	if values := request.Header.Values(protocol.APIVersionHeader); len(values) != 1 || values[0] != protocol.APIVersion {
		return http.StatusUpgradeRequired, protocol.ErrorIncompatibleAPI, false
	}
	return 0, "", true
}

func decodeCommandBody(request *http.Request) (protocol.Command, error) {
	if request.ContentLength > protocol.MaxContextCommandBytes {
		return protocol.Command{}, protocol.ErrMessageTooLarge
	}
	reader := io.LimitReader(request.Body, int64(protocol.MaxContextCommandBytes)+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return protocol.Command{}, err
	}
	if len(body) > protocol.MaxContextCommandBytes {
		return protocol.Command{}, protocol.ErrMessageTooLarge
	}
	return protocol.DecodeCommand(body)
}

func safeHTTPError(response http.ResponseWriter, status int, code protocol.BrowserErrorCode) {
	if classified, ok := response.(interface {
		setBrowserErrorCode(protocol.BrowserErrorCode)
	}); ok {
		classified.setBrowserErrorCode(code)
	}
	setCommonHeaders(response.Header())
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(struct {
		Error protocol.BrowserError `json:"error"`
	}{protocol.NewBrowserError(code)})
}

func statusForDecodeError(err error) int {
	if errors.Is(err, protocol.ErrMessageTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func canonicalRoute(path string) string {
	switch path {
	case protocol.StatusPath, protocol.ConnectPath, CommandsPath:
		return path
	default:
		if path == protocol.ImagesPath || strings.HasPrefix(path, protocol.ImagesPath+"/") {
			return protocol.ImagesPath
		}
		return "unknown"
	}
}

type recordingResponseWriter struct {
	http.ResponseWriter
	status int
	code   protocol.BrowserErrorCode
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

func (w *recordingResponseWriter) setBrowserErrorCode(code protocol.BrowserErrorCode) {
	w.code = code
}
