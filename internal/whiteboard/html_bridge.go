package whiteboard

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"golang.org/x/net/html"
)

var (
	bridgeScriptStart = []byte("<script>")
	bridgeScriptEnd   = []byte("</script>")
)

// injectHTMLBridge inserts trusted JavaScript after the exact source head start
// token. It returns a complete response buffer so callers can fail before
// writing a partial successful response.
func injectHTMLBridge(source, bridge []byte) ([]byte, error) {
	if len(bridge) == 0 || !utf8.Valid(bridge) || bytes.Contains(bytes.ToLower(bridge), []byte("</script")) {
		return nil, common.NewError(common.CodeInternal, "HTML bridge is unsafe", nil)
	}
	if !utf8.Valid(source) {
		return nil, common.NewError(common.CodeInternal, "could not locate HTML head for bridge injection", nil)
	}

	tokenizer := html.NewTokenizer(bytes.NewReader(source))
	offset := 0
	for {
		tokenType := tokenizer.Next()
		raw := tokenizer.Raw()
		offset += len(raw)
		switch tokenType {
		case html.StartTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "head") {
				result := make([]byte, 0, len(source)+len(bridgeScriptStart)+len(bridge)+len(bridgeScriptEnd))
				result = append(result, source[:offset]...)
				result = append(result, bridgeScriptStart...)
				result = append(result, bridge...)
				result = append(result, bridgeScriptEnd...)
				result = append(result, source[offset:]...)
				return result, nil
			}
		case html.ErrorToken:
			err := tokenizer.Err()
			if err != nil && err != io.EOF {
				return nil, common.NewError(common.CodeInternal, "could not locate HTML head for bridge injection", err)
			}
			return nil, common.NewError(common.CodeInternal, "could not locate HTML head for bridge injection", nil)
		}
	}
}
