// Package web embeds the built frontend assets (esbuild output in dist/).
package web

import "embed"

// Dist holds the built frontend. Run `make frontend` (or `make build`)
// to populate web/dist before building the binary; a .gitkeep placeholder
// keeps the embed valid in a fresh checkout.
//
//go:embed all:dist
var Dist embed.FS
