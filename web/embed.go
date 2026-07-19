// ircthing — a self-hosted, always-connected web IRC client.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Package web embeds the built frontend assets (esbuild output in dist/).
package web

import "embed"

// Dist holds the built frontend. Run `make frontend` (or `make build`)
// to populate web/dist before building the binary; a .gitkeep placeholder
// keeps the embed valid in a fresh checkout.
//
//go:embed all:dist
var Dist embed.FS
