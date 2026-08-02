package web

import (
	"embed"
	"io/fs"
	"mime"
)

// app.css is produced by the Tailwind v4 CLI during the Docker build, before
// `go build` runs. It is not checked in — see the Dockerfile's css step. The
// favicons are checked in (they came from favicon_io, the project's icon set).
//
//go:embed app.css apple-touch-icon.png favicon-32x32.png favicon-16x16.png favicon.ico site.webmanifest android-chrome-192x192.png android-chrome-512x512.png
var staticFS embed.FS

// Go's built-in table has no .webmanifest entry; without this the file server
// answers application/octet-stream.
func init() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// StaticFS returns the embedded assets, served under /static/.
func StaticFS() fs.FS { return staticFS }
