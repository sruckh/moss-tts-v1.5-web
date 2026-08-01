package web

import (
	"embed"
	"io/fs"
)

// app.css is produced by the Tailwind v4 CLI during the Docker build, before
// `go build` runs. It is not checked in — see the Dockerfile's css step.
//
//go:embed app.css
var staticFS embed.FS

// StaticFS returns the embedded assets, served under /static/.
func StaticFS() fs.FS { return staticFS }
