package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
)

// app.css is produced by the Tailwind v4 CLI during the Docker build, before
// `go build` runs. It is not checked in — see the Dockerfile's css step. The
// favicons are checked in (they came from favicon_io, the project's icon set).
//
//go:embed app.css apple-touch-icon.png favicon-32x32.png favicon-16x16.png favicon.ico site.webmanifest android-chrome-192x192.png android-chrome-512x512.png
var staticFS embed.FS

// cssVersion fingerprints the embedded stylesheet. The app sits behind
// Cloudflare, which caches /static aggressively and ignores content changes
// at an unchanged URL — a fresh deploy can otherwise serve a stale app.css
// indefinitely (this has shipped fixed-but-invisible CSS twice). Rendering
// the link as /static/app.css?v=<hash> makes every build a new URL, so caches
// can hold the file forever and still pick up each deploy.
var cssVersion = func() string {
	data, err := staticFS.ReadFile("app.css")
	if err != nil {
		return "dev"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}()

// CSSVersion is the per-build fingerprint appended to the stylesheet URL.
func CSSVersion() string { return cssVersion }

// Go's built-in table has no .webmanifest entry; without this the file server
// answers application/octet-stream.
func init() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// StaticFS returns the embedded assets, served under /static/.
func StaticFS() fs.FS { return staticFS }
