package whiteboard

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInjectHTMLBridgePreservesSourceOutsideEarlyScript(t *testing.T) {
	source := []byte("<!DOCTYPE HTML>\n<HTML><HeAd data-x='1'>\n<script>publisher()</script></HeAd><body>exact & bytes</body></HTML>")
	bridge := []byte("(()=>{globalThis.bridge=true})()")

	got, err := injectHTMLBridge(source, bridge)
	require.NoError(t, err)
	injected := append(append([]byte("<script>"), bridge...), []byte("</script>")...)
	require.Equal(t, append([]byte("<!DOCTYPE HTML>\n<HTML><HeAd data-x='1'>"), append(injected, []byte("\n<script>publisher()</script></HeAd><body>exact & bytes</body></HTML>")...)...), got)
	require.Equal(t, source, bytes.Replace(got, injected, nil, 1))
}

func TestInjectHTMLBridgeFailsClosedForUnsafeBridgeOrMissingHead(t *testing.T) {
	for _, test := range []struct {
		name   string
		source []byte
		bridge []byte
	}{
		{name: "missing head", source: []byte("<!doctype html><html><body>x</body></html>"), bridge: []byte("safe()")},
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
