package whiteboard

import (
	"bytes"
	"embed"
)

//go:embed assets/dist/viewer.min.js assets/dist/viewer.min.css assets/manifest.json assets/licenses/THIRD_PARTY_NOTICES.txt
var files embed.FS

var (
	viewerJS          = mustReadBrowserAsset("assets/dist/viewer.min.js")
	viewerCSS         = mustReadBrowserAsset("assets/dist/viewer.min.css")
	manifest          = mustReadBrowserAsset("assets/manifest.json")
	thirdPartyNotices = mustReadBrowserAsset("assets/licenses/THIRD_PARTY_NOTICES.txt")
)

// ViewerJS returns a fresh copy of the bundled browser renderer.
func ViewerJS() []byte {
	return bytes.Clone(viewerJS)
}

// ViewerCSS returns a fresh copy of the bundled viewer stylesheet.
func ViewerCSS() []byte {
	return bytes.Clone(viewerCSS)
}

// Manifest returns a fresh copy of the generated asset manifest.
func Manifest() []byte {
	return bytes.Clone(manifest)
}

// ThirdPartyNotices returns a fresh copy of the browser dependencies' notices.
func ThirdPartyNotices() []byte {
	return bytes.Clone(thirdPartyNotices)
}

func mustReadBrowserAsset(name string) []byte {
	content, err := files.ReadFile(name)
	if err != nil {
		panic("read embedded browser asset " + name + ": " + err.Error())
	}
	return content
}
