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

// canonicalSASL is the submit-time authority on a network's stored SASL
// block. The form keeps typed values across mechanism toggles (flipping away
// and back must not lose a password or cert path — same rule as the egress
// blocks in netform's submit()), which means hidden inputs can hold stale
// state; this strips whatever the SELECTED mechanism cannot use so it never
// reaches storage:
//
//   - PLAIN / SCRAM-SHA-256: cert_file/key_file are dropped — cert paths
//     typed for EXTERNAL must not silently keep a client cert presented on
//     connect. (This governs only the form's payload; a config file pairing
//     a client cert with PLAIN — CertFP — remains supported.)
//   - EXTERNAL: password is dropped (authentication is the TLS client
//     cert); login is KEPT — it is the optional authzid, legal with
//     EXTERNAL.
//   - "auto" (stored as mechanism ""): the server resolves it to EXTERNAL
//     when the password is empty, else SCRAM/PLAIN (internal/irc/sasl.go
//     newMech), so every field is potentially applicable and all are kept.
//
// `choice` is the form's selection ("none", "auto", or an explicit
// mechanism). Returns null for "none" (no SASL block at all). Empty fields
// are always dropped so they don't clutter the stored JSON.
export function canonicalSASL(sasl, choice) {
	if (choice === "none") return null;
	const clean = { ...sasl, mechanism: choice === "auto" ? "" : choice };
	if (choice === "EXTERNAL") {
		delete clean.password;
	} else if (choice !== "auto") {
		delete clean.cert_file;
		delete clean.key_file;
	}
	for (const k of ["mechanism", "login", "password", "cert_file", "key_file"]) {
		if (!clean[k]) delete clean[k];
	}
	return clean;
}
