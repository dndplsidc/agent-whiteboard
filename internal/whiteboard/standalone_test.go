package whiteboard_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	httpx "github.com/edocsss/agent-whiteboard/internal/http"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

func TestStandaloneWrapperContainsOnlyStaticApplicationContentAndExactSandbox(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, whiteboard.RenderStandaloneWrapper(&output, testWhiteboardID))

	document, err := html.Parse(bytes.NewReader(output.Bytes()))
	require.NoError(t, err)
	require.Equal(t, 1, countElements(document, "iframe"))
	frames := findElements(document, "iframe", nil)
	require.Len(t, frames, 1)
	require.Equal(t, httpx.PublicHTML+testWhiteboardID+httpx.PublicHTMLContentSuffix, attribute(frames[0], "src"))
	require.Equal(t, "allow-scripts", attribute(frames[0], "sandbox"))
	require.Equal(t, "no-referrer", attribute(frames[0], "referrerpolicy"))
	require.True(t, hasBooleanAttribute(frames[0], "credentialless"))
	for _, forbidden := range []string{"allow-same-origin", "allow-forms", "allow-popups", "allow-downloads", "allow-top-navigation"} {
		require.NotContains(t, attribute(frames[0], "sandbox"), forbidden)
	}
	require.NotContains(t, output.String(), "SUBMITTED_SECRET")
	require.NotContains(t, output.String(), "<script")

	styles := findElements(document, "style", nil)
	require.Len(t, styles, 1)
	styleDigest := sha256.Sum256([]byte(textContent(styles[0])))
	require.Contains(t, whiteboard.StandaloneOuterContentSecurityPolicy,
		"style-src 'sha256-"+base64.StdEncoding.EncodeToString(styleDigest[:])+"'")
	require.NotContains(t, whiteboard.StandaloneOuterContentSecurityPolicy, "style-src 'unsafe-inline'")
}

func TestStandaloneWrapperRejectsInvalidIDWithoutWriting(t *testing.T) {
	var output bytes.Buffer
	err := whiteboard.RenderStandaloneWrapper(&output, "malformed")
	require.Error(t, err)
	require.Empty(t, output.String())
}

func hasBooleanAttribute(node *html.Node, name string) bool {
	for _, attr := range node.Attr {
		if attr.Key == name && strings.TrimSpace(attr.Val) == "" {
			return true
		}
	}
	return false
}
