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

import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { test } from "node:test";
import { canonicalSASL } from "../src/sasl.js";

// A state blob holding every field at once — what the form state looks like
// after a user typed values under one mechanism and then switched to another
// (the form intentionally keeps typed values across toggles).
const full = {
	mechanism: "EXTERNAL",
	login: "alice",
	password: "hunter2",
	cert_file: "/etc/ssl/me.pem",
	key_file: "/etc/ssl/me.key",
};

test("canonicalSASL strips fields the selected mechanism cannot use", () => {
	const cases = [
		// EXTERNAL -> PLAIN/SCRAM: the cert paths must not ride along, or the
		// client cert is silently still presented on connect.
		["PLAIN", { mechanism: "PLAIN", login: "alice", password: "hunter2" }],
		["SCRAM-SHA-256", { mechanism: "SCRAM-SHA-256", login: "alice", password: "hunter2" }],
		// PLAIN/SCRAM -> EXTERNAL: neither the password nor the login stays —
		// login is the PLAIN/SCRAM authcid, and EXTERNAL's authzid is a
		// separate config field this form does not edit.
		["EXTERNAL", {
			mechanism: "EXTERNAL",
			cert_file: "/etc/ssl/me.pem",
			key_file: "/etc/ssl/me.key",
		}],
		// auto (stored mechanism "") may resolve server-side to EXTERNAL
		// (empty password) or SCRAM/PLAIN, so every field stays applicable.
		["auto", {
			login: "alice",
			password: "hunter2",
			cert_file: "/etc/ssl/me.pem",
			key_file: "/etc/ssl/me.key",
		}],
	];
	for (const [choice, want] of cases) eq(canonicalSASL(full, choice), want);
});

test("canonicalSASL none drops the whole block", () => {
	is(canonicalSASL(full, "none"), null);
	is(canonicalSASL(undefined, "none"), null);
});

test("canonicalSASL leaves unchanged-mechanism payloads unchanged", () => {
	const plain = { mechanism: "PLAIN", login: "a", password: "p" };
	eq(canonicalSASL(plain, "PLAIN"), plain);
	const scram = { mechanism: "SCRAM-SHA-256", login: "a", password: "p" };
	eq(canonicalSASL(scram, "SCRAM-SHA-256"), scram);
	const ext = { mechanism: "EXTERNAL", cert_file: "c", key_file: "k" };
	eq(canonicalSASL(ext, "EXTERNAL"), ext);
	const auto = { login: "a", password: "p" };
	eq(canonicalSASL(auto, "auto"), auto);
});

test("canonicalSASL drops empty optional fields", () => {
	// auto stores mechanism "" and an untouched form yields empty strings —
	// none of that clutter reaches the stored JSON (old saslOut behavior).
	eq(canonicalSASL({ mechanism: "", login: "", password: "" }, "auto"), {});
	// Mechanism picked but nothing typed yet: state has no sasl object.
	eq(canonicalSASL(undefined, "PLAIN"), { mechanism: "PLAIN" });
});

test("canonicalSASL does not mutate the form state it is given", () => {
	const state = { ...full };
	canonicalSASL(state, "PLAIN");
	eq(state, full);
});
