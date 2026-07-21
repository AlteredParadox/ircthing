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

import { deepStrictEqual as eq, rejects, strictEqual as is } from "node:assert";
import { test } from "node:test";
import { mediaKindOf } from "../src/irc.js";
import { mediaFileName, mintMediaToken, shouldRemint, streamSrc } from "../src/media.js";

test("mediaKindOf classifies by path extension, case-insensitive, ignoring query/fragment", () => {
	const cases = [
		// audio
		["https://x.example/song.mp3", "audio"],
		["https://x.example/a/b/Track.FLAC", "audio"],
		["https://x.example/pod.ogg?dl=1", "audio"],
		["https://x.example/voice.opus#t=30", "audio"],
		["https://x.example/x.m4a", "audio"],
		["https://x.example/x.wav", "audio"],
		["https://x.example/x.aac", "audio"],
		// video
		["https://x.example/clip.mp4", "video"],
		["https://x.example/clip.WebM?x=.mp3", "video"], // query must not reclassify
		["https://x.example/clip.m4v", "video"],
		["https://x.example/clip.ogv", "video"],
		// not media
		["https://x.example/photo.jpg", null],
		["https://x.example/page.html", null],
		["https://x.example/", null],
		["https://x.example/no-extension", null],
		["https://x.example/dir.mp3/file", null], // extension must be on the LAST segment
		["https://x.example/trailingdot.", null],
		["https://x.example/mp3", null], // no dot
		["not a url", null],
		["", null],
	];
	for (const [url, want] of cases) {
		is(mediaKindOf(url), want, url);
	}
});

test("mintMediaToken resolves a well-formed response", async () => {
	let got = null;
	const fetchFn = async (path, opts) => {
		got = { path, opts };
		return { ok: true, json: async () => ({ token: "abc", exp: 1234567 }) };
	};
	const d = await mintMediaToken("https://x.example/a.mp3", "libera", fetchFn);
	eq(d, { token: "abc", exp: 1234567 });
	is(got.path, "/api/media/token");
	is(got.opts.method, "POST");
	// The URL travels in the POST body (never a query string).
	eq(JSON.parse(got.opts.body), { url: "https://x.example/a.mp3", net: "libera" });
});

test("mintMediaToken normalizes a missing network to empty string", async () => {
	let body = null;
	const fetchFn = async (_p, opts) => {
		body = JSON.parse(opts.body);
		return { ok: true, json: async () => ({ token: "t", exp: 1 }) };
	};
	await mintMediaToken("https://x.example/a.mp3", undefined, fetchFn);
	is(body.net, "");
});

test("mintMediaToken rejects HTTP failure with the status attached", async () => {
	const fetchFn = async () => ({ ok: false, status: 403, json: async () => ({}) });
	await rejects(
		() => mintMediaToken("https://x.example/a.mp3", "", fetchFn),
		(err) => err.status === 403,
	);
});

test("mintMediaToken rejects malformed bodies", async () => {
	for (const bad of [null, {}, { token: 42, exp: 1 }, { token: "" }, { token: "t" }, { token: "t", exp: "soon" }]) {
		const fetchFn = async () => ({ ok: true, json: async () => bad });
		await rejects(() => mintMediaToken("https://x.example/a.mp3", "", fetchFn), undefined, JSON.stringify(bad));
	}
});

test("streamSrc URL-encodes the token into the stream path", () => {
	is(streamSrc("a+b/c"), "/api/media/stream?t=a%2Bb%2Fc");
});

test("shouldRemint only near/past expiry", () => {
	const expSec = 1_000_000; // exp in unix seconds
	const expMs = expSec * 1000;
	is(shouldRemint(expSec, expMs - 60_000), false, "mid-life error is a real failure");
	is(shouldRemint(expSec, expMs - 10_000), true, "within the skew window counts as expiry");
	is(shouldRemint(expSec, expMs + 1), true, "past expiry");
	is(shouldRemint(undefined, expMs), false, "no expiry known: never remint");
});

test("mediaFileName extracts the last path segment, decoded", () => {
	is(mediaFileName("https://x.example/music/My%20Song.mp3"), "My Song.mp3");
	is(mediaFileName("https://x.example/clip.mp4?dl=1"), "clip.mp4");
	is(mediaFileName("https://x.example/"), "x.example", "bare path falls back to host");
	is(mediaFileName("https://x.example/bad%zz.mp3"), "bad%zz.mp3", "malformed escape shown raw");
	is(mediaFileName("nonsense"), "nonsense", "unparseable input returned as-is");
});
