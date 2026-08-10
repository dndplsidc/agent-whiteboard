package localapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
)

var mutationHeaders = []string{"content-type", strings.ToLower(agentprotocol.APIVersionHeader)}
var imageReadHeaders = []string{
	strings.ToLower(agentprotocol.APIVersionHeader),
	strings.ToLower(agentprotocol.ClientIDHeader),
	strings.ToLower(agentprotocol.ConversationIDHeader),
}
var imageUploadHeaders = append(append([]string{}, imageReadHeaders...), "content-type", strings.ToLower(agentprotocol.ProviderHeader), strings.ToLower(agentprotocol.ImagePurposeHeader))

func (s *Server) preflight(response http.ResponseWriter, request *http.Request) {
	origin, err := s.requestOrigin(request)
	if err != nil {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}
	methodValues := request.Header.Values("Access-Control-Request-Method")
	if len(methodValues) != 1 || methodValues[0] == "" {
		safeHTTPError(response, http.StatusBadRequest, agentprotocol.ErrorInvalidCommand)
		return
	}

	allowedMethod := ""
	requiredHeaders := []string(nil)
	authorized := false
	switch request.URL.Path {
	case agentprotocol.StatusPath:
		allowedMethod = http.MethodGet
		authorized = true
	case agentprotocol.ConnectPath:
		allowedMethod = http.MethodPost
		requiredHeaders = mutationHeaders
		authorized = s.trusted(request.Context(), origin)
	case CommandsPath:
		allowedMethod = http.MethodPost
		requiredHeaders = mutationHeaders
		authorized = s.attachments.hasOrigin(origin) || s.trusted(request.Context(), origin)
	default:
		if request.URL.Path == agentprotocol.ImagesPath {
			allowedMethod = http.MethodPost
			requiredHeaders = imageUploadHeaders
			authorized = s.attachments.hasOrigin(origin) || s.trusted(request.Context(), origin)
		} else if _, ok := imageIDFromPath(request.URL.Path); ok && (methodValues[0] == http.MethodGet || methodValues[0] == http.MethodDelete) {
			allowedMethod = methodValues[0]
			requiredHeaders = imageReadHeaders
			authorized = s.attachments.hasOrigin(origin) || s.trusted(request.Context(), origin)
		} else {
			safeHTTPError(response, http.StatusNotFound, agentprotocol.ErrorInvalidCommand)
			return
		}
	}
	if methodValues[0] != allowedMethod || !sameHeaderSet(request.Header.Values("Access-Control-Request-Headers"), requiredHeaders) {
		safeHTTPError(response, http.StatusBadRequest, agentprotocol.ErrorInvalidCommand)
		return
	}
	if !authorized {
		safeHTTPError(response, http.StatusForbidden, agentprotocol.ErrorUntrustedOrigin)
		return
	}

	setCommonHeaders(response.Header())
	allowOrigin(response.Header(), origin)
	response.Header().Set("Access-Control-Allow-Methods", allowedMethod)
	if len(requiredHeaders) != 0 {
		response.Header().Set("Access-Control-Allow-Headers", strings.Join(requiredHeaders, ", "))
	}
	appendVary(response.Header(), "Access-Control-Request-Method")
	appendVary(response.Header(), "Access-Control-Request-Headers")
	if values := request.Header.Values("Access-Control-Request-Private-Network"); len(values) == 1 && values[0] == "true" {
		response.Header().Set("Access-Control-Allow-Private-Network", "true")
		appendVary(response.Header(), "Access-Control-Request-Private-Network")
	}
	response.WriteHeader(http.StatusNoContent)
}

func sameHeaderSet(values []string, expected []string) bool {
	var actual []string
	for _, line := range values {
		for _, item := range strings.Split(line, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				return false
			}
			actual = append(actual, strings.ToLower(item))
		}
	}
	if len(actual) != len(expected) {
		return false
	}
	sort.Strings(actual)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	for index := range actual {
		if actual[index] != want[index] || (index > 0 && actual[index] == actual[index-1]) {
			return false
		}
	}
	return true
}
