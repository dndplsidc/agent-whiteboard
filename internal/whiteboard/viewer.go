package whiteboard

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"

	"github.com/edocsss/agent-whiteboard/internal/common"
)

type ViewerConfig struct {
	CSS               []byte
	JS                []byte
	LocalAgentEnabled bool
}

type Viewer struct {
	css               []byte
	js                []byte
	localAgentEnabled bool
	csp               string
}

func NewViewer(config ViewerConfig) (*Viewer, error) {
	switch {
	case len(config.CSS) == 0:
		return nil, common.NewError(common.CodeInvalidRequest, "viewer CSS is required", nil)
	case len(config.JS) == 0:
		return nil, common.NewError(common.CodeInvalidRequest, "viewer JavaScript is required", nil)
	}

	jsDigest := sha256.Sum256(config.JS)
	connectSource := "'none'"
	if config.LocalAgentEnabled {
		connectSource = "http://127.0.0.1:* ws://127.0.0.1:*"
	}
	return &Viewer{
		css:               append([]byte(nil), config.CSS...),
		js:                append([]byte(nil), config.JS...),
		localAgentEnabled: config.LocalAgentEnabled,
		csp: "default-src 'none'; base-uri 'none'; connect-src " + connectSource +
			"; font-src 'none'; form-action 'none'; frame-ancestors 'none'; frame-src 'none'; img-src 'self' data:; manifest-src 'none'; media-src 'none'; object-src 'none'; script-src 'sha256-" +
			base64.StdEncoding.EncodeToString(jsDigest[:]) + "'; style-src 'unsafe-inline'; worker-src 'none'",
	}, nil
}

func (v *Viewer) ContentSecurityPolicy() string {
	return v.csp
}

func (v *Viewer) Render(w io.Writer, board Whiteboard) error {
	if err := writeViewerString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex, nofollow, noarchive"><title>Untitled whiteboard</title><style>`); err != nil {
		return err
	}
	if err := writeViewerBytes(w, v.css); err != nil {
		return err
	}
	if err := writeViewerString(w, `</style></head><body><noscript>This whiteboard requires JavaScript to render.</noscript><main id="agent-whiteboard-content"></main><script type="application/json" id="agent-whiteboard-source">`); err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	if v.localAgentEnabled {
		payload := struct {
			Markdown   string `json:"markdown"`
			Context    string `json:"context"`
			LocalAgent struct {
				Enabled bool `json:"enabled"`
			} `json:"local_agent"`
		}{Markdown: string(board.Source), Context: string(board.Context)}
		payload.LocalAgent.Enabled = true
		if err := encoder.Encode(payload); err != nil {
			return err
		}
	} else if err := encoder.Encode(struct {
		Markdown string `json:"markdown"`
	}{Markdown: string(board.Source)}); err != nil {
		return err
	}
	if err := writeViewerString(w, `</script><script>`); err != nil {
		return err
	}
	if err := writeViewerBytes(w, v.js); err != nil {
		return err
	}
	return writeViewerString(w, `</script></body></html>`)
}

func writeViewerString(w io.Writer, value string) error {
	written, err := io.WriteString(w, value)
	if err == nil && written != len(value) {
		return io.ErrShortWrite
	}
	return err
}

func writeViewerBytes(w io.Writer, value []byte) error {
	written, err := w.Write(value)
	if err == nil && written != len(value) {
		return io.ErrShortWrite
	}
	return err
}
