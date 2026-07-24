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

// Package ircthing embeds repo-root legal texts into the binary, so every
// distributed artifact carries its own license and the third-party notices
// for everything statically linked into it (served at /third-party-licenses;
// see scripts/gen-third-party-licenses.sh).
package ircthing

// Blank import for its side effect: registers the //go:embed directives
// below (the embed package need not be referenced by name).
import _ "embed"

// License is the project's own license (GNU AGPL-3.0-or-later).
//
//go:embed LICENSE
var License []byte

// ThirdPartyLicenses reproduces the license/copyright notices of all bundled
// third-party code: Go modules linked into the binary and npm packages in the
// embedded web bundle. Regenerate with scripts/gen-third-party-licenses.sh
// after any dependency change.
//
//go:embed THIRD_PARTY_LICENSES.md
var ThirdPartyLicenses []byte
