package whiteboard

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInjectHTMLBridgePreservesSourceOutsideEarlyScript(t *testing.T) {
	source := []byte(" \n<!--before-->\n\xef\xbb\xbf \n<!DOCTYPE HTML>\n<HTML><HeAd data-x='1'>\n<script>publisher()</script></HeAd><body>exact & bytes</body></HTML>")
	bridge := []byte("(()=>{globalThis.bridge=true})()")

	got, err := injectHTMLBridge(source, bridge)
	require.NoError(t, err)
	injected := append(append([]byte("<script>"), bridge...), []byte("</script>")...)
	require.Equal(t, append([]byte(" \n<!--before-->\n\xef\xbb\xbf \n<!DOCTYPE HTML>\n<HTML><HeAd data-x='1'>"), append(injected, []byte("\n<script>publisher()</script></HeAd><body>exact & bytes</body></HTML>")...)...), got)
	require.Equal(t, source, bytes.Replace(got, injected, nil, 1))
}

func TestInjectHTMLBridgeFallsBackAtomicallyForPublisherContentBeforeExplicitHead(t *testing.T) {
	allowedPrefix := []byte("<!DOCTYPE html>\n<!--exact comment-->\n<HTML data-x='1'>\n  ")
	bridge := []byte("safe()")
	for _, prefix := range []string{
		"<script>publisher()</script>",
		"<meta charset='utf-8'>",
		"<body>publisher</body>",
		"publisher text",
		"</HTML>",
		"<?publisher execute?>",
	} {
		source := append(append(append([]byte{}, allowedPrefix...), []byte(prefix)...), []byte("<HeAd></HeAd><body>exact</body></HTML>")...)
		got, err := injectHTMLBridge(source, bridge)
		require.NoError(t, err, prefix)
		require.Equal(t, source, got, prefix)
		require.NotContains(t, string(got), "<script>safe()</script>", prefix)
	}

	missingHead := []byte("<!doctype html><html><body>legacy</body></html>")
	got, err := injectHTMLBridge(missingHead, bridge)
	require.NoError(t, err)
	require.Equal(t, missingHead, got)

	source := append(append([]byte{}, allowedPrefix...), []byte("<HeAd></HeAd><body>exact</body></HTML>")...)
	got, err = injectHTMLBridge(source, bridge)
	require.NoError(t, err)
	injected := []byte("<script>safe()</script>")
	require.Equal(t, source, bytes.Replace(got, injected, nil, 1))
}

func TestInjectHTMLBridgeFailsClosedForUnsafeBridge(t *testing.T) {
	for _, test := range []struct {
		name   string
		source []byte
		bridge []byte
	}{
		{name: "empty bridge", source: []byte("<!doctype html><html><head></head><body></body></html>")},
		{name: "script terminator", source: []byte("<!doctype html><html><head></head><body></body></html>"), bridge: []byte(`const x="</ScRiPt>"`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := injectHTMLBridge(test.source, test.bridge)
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}
