// Package web embeds the static frontend into the binary so the container
// ships a single file and there is no separate static-hosting step.
package web

import (
	"embed"
	"net/http"
)

//go:embed index.html app.js style.css
var files embed.FS

// Handler serves the frontend. index.html is served for "/" only; unknown
// paths get a 404 from FileServer, which is fine for an app with one page.
func Handler() http.Handler {
	return http.FileServer(http.FS(files))
}
