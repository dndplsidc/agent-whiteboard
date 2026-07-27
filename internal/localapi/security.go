package localapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/edocsss/agent-whiteboard/internal/config"
)

var (
	errBadHost         = errors.New("invalid host")
	errBadOrigin       = errors.New("invalid origin")
	errUntrustedOrigin = errors.New("untrusted origin")
)

func (s *Server) requestOrigin(request *http.Request) (string, error) {
	if request.Host != s.host {
		return "", errBadHost
	}
	values := request.Header.Values("Origin")
	if len(values) != 1 || values[0] == "" || values[0] == "null" {
		return "", errBadOrigin
	}
	canonical, err := config.CanonicalOrigin(values[0])
	if err != nil || canonical != values[0] {
		return "", errBadOrigin
	}
	return canonical, nil
}

func (s *Server) trusted(ctx context.Context, origin string) bool {
	origins, err := s.trust.TrustedOrigins(ctx)
	if err != nil {
		return false
	}
	_, ok := origins[origin]
	return ok
}

func setCommonHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
}

func allowOrigin(header http.Header, origin string) {
	header.Set("Access-Control-Allow-Origin", origin)
	appendVary(header, "Origin")
}

func appendVary(header http.Header, value string) {
	for _, existing := range header.Values("Vary") {
		if existing == value {
			return
		}
	}
	header.Add("Vary", value)
}
