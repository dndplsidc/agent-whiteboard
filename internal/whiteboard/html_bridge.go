package whiteboard

import (
	"bytes"
	"errors"
	"unicode/utf8"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
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

	offset, err := htmlHeadStartOffset(source)
	if errors.Is(err, errPublisherContentBeforeHead) {
		return bytes.Clone(source), nil
	}
	if err != nil {
		return nil, common.NewError(common.CodeInternal, "could not locate safe HTML head for bridge injection", err)
	}
	result := make([]byte, 0, len(source)+len(bridgeScriptStart)+len(bridge)+len(bridgeScriptEnd))
	result = append(result, source[:offset]...)
	result = append(result, bridgeScriptStart...)
	result = append(result, bridge...)
	result = append(result, bridgeScriptEnd...)
	result = append(result, source[offset:]...)
	return result, nil
}
