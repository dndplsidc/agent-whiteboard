package whiteboard

import (
	"io"

	"github.com/edocsss/agent-whiteboard/internal/common"
	httpx "github.com/edocsss/agent-whiteboard/internal/http"
)

const (
	RestrictivePermissionsPolicy = "accelerometer=(), autoplay=(), camera=(), display-capture=(), encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), hid=(), idle-detection=(), magnetometer=(), microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-create=(), publickey-credentials-get=(), screen-wake-lock=(), serial=(), usb=(), web-share=(), xr-spatial-tracking=()"

	StandaloneOuterContentSecurityPolicy = "default-src 'none'; base-uri 'none'; connect-src 'none'; font-src 'none'; form-action 'none'; frame-ancestors 'none'; frame-src 'self'; img-src 'none'; manifest-src 'none'; media-src 'none'; object-src 'none'; script-src 'none'; style-src 'sha256-Tn/hKQI0ISMV0qjQCZd0Gif536vvizgJ1ukIP+PYoJ8='; worker-src 'none'"
	StandaloneInnerContentSecurityPolicy = "sandbox allow-scripts; default-src 'none'; base-uri 'none'; connect-src 'none'; font-src 'none'; form-action 'none'; frame-ancestors 'self'; frame-src 'none'; img-src data: blob:; manifest-src 'none'; media-src data: blob:; object-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; worker-src 'none'"
)

func RenderStandaloneWrapper(w io.Writer, id string) error {
	if err := common.ValidateID(id); err != nil {
		return err
	}
	if err := writeViewerString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex, nofollow, noarchive"><title>Standalone whiteboard</title><style>html,body,iframe{box-sizing:border-box;width:100%;height:100%;margin:0;border:0}body{overflow:hidden}</style></head><body><iframe title="Standalone whiteboard content" src="`); err != nil {
		return err
	}
	if err := writeViewerString(w, httpx.PublicHTML+id+httpx.PublicHTMLContentSuffix); err != nil {
		return err
	}
	return writeViewerString(w, `" sandbox="allow-scripts" referrerpolicy="no-referrer" credentialless></iframe></body></html>`)
}
