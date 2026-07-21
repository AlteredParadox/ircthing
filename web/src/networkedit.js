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

function editError(code, message) {
	return Object.assign(new Error(message), { code });
}

// editableNetwork validates the exact get_network response before handing its
// config to the form. Recovery rows deliberately omit config, so their flags
// must win over the generic response-shape check.
export function editableNetwork(data, expectedName) {
	const network = data?.network;
	if (!network || typeof network !== "object" || Array.isArray(network)) {
		throw editError("unavailable", "network definition is unavailable");
	}
	if (network.invalid_name) {
		throw editError("invalid_name", "stored network name is invalid");
	}
	if (network.name !== expectedName) {
		throw editError("unavailable", "network lookup returned the wrong definition");
	}
	if (network.oversized) {
		throw editError("oversized", "network definition is oversized");
	}
	if (network.invalid) {
		throw editError("invalid", "network definition is invalid");
	}
	if (!network.config || typeof network.config !== "object" || Array.isArray(network.config)) {
		throw editError("unavailable", "network definition is unavailable");
	}
	return network;
}

// networkEditError turns exact-lookup and recovery errors into actionable UI
// text. The menu remains usable, so every uneditable legacy row can still be
// removed and recreated without direct database access.
export function networkEditError(name, error) {
	switch (error?.code) {
		case "oversized":
			return `${name}: definition exceeds the 64 KiB safety limit; ` +
				"repair it in the database, or use Remove network… and recreate it.";
		case "invalid_name":
			return `${name}: stored network name is invalid; ` +
				"use Remove network… and recreate it with a valid name.";
		case "invalid":
		case "unavailable":
			return `${name}: stored definition is invalid; ` +
				"repair it in the database, or use Remove network… and recreate it.";
		case "unknown_network":
		case "not_found":
			return `${name}: network no longer exists.`;
		default:
			return error?.message || "loading network failed";
	}
}
