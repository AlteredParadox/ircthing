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

// readTimestamp computes the read-marker timestamp for a buffer's visible
// list: the max time over SERVER-stamped events only.
//
// Max, not last-arrived: live events append in arrival order, so a backdated
// relay/bridge line lands at the tail with an older time; marking there would
// rewind the marker and re-badge genuinely-newer messages as unread.
//
// Excluding `local: true` events: synthetic info cards (whois / server_info)
// are stamped with the BROWSER clock for display ordering. A browser clock a
// few minutes ahead would otherwise push the marker into the future (the
// server clamp is now+5min), and since unread/mention gate on
// `ev.time > marker`, real messages arriving inside the skew window would
// show no badge on ANY device. Local rows must never advance the marker.
export function readTimestamp(list) {
	return list.reduce((mx, m) => (m.local ? mx : Math.max(mx, m.time)), 0);
}
