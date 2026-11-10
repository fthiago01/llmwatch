// Package web holds the static assets of the llmwatch web interface.
package web

import "embed"

// FS holds the embedded static files served by the HTTP server.
//
// The pages are self contained: HTML, CSS and JavaScript live in a single
// file per page, so the interface works without any network access.
//
// The progressive web app files (manifest, service worker and icons) are
// embedded as well and are served by the public routes of the HTTP server.
//
//go:embed *.html manifest.webmanifest sw.js icon.svg icon-192.png icon-512.png
var FS embed.FS
