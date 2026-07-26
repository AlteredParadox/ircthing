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

// Shared plumbing for the browser tests: one Chromium for the whole run, and
// the geometry probes the assertions are written against.

import { chromium } from "playwright";
import { fileURLToPath } from "node:url";
import path from "node:path";

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const PAGE_URL = "file://" + path.join(HERE, ".build", "chat-harness.html");

let browser = null;

export async function launch() {
	if (!browser) browser = await chromium.launch();
	return browser;
}

export async function shutdown() {
	if (browser) await browser.close();
	browser = null;
}

// open mounts the harness at a given viewport. Page errors are promoted to
// rejections: a crashed render otherwise looks like an empty message list,
// which several assertions would pass on.
export async function open(browserInst, { width, height, look }, opts = {}) {
	const page = await browserInst.newPage({
		viewport: { width, height },
		hasTouch: opts.touch === true,
	});
	const errors = [];
	page.on("pageerror", (e) => errors.push(e.message));
	await page.goto(PAGE_URL);
	if (look) {
		await page.evaluate(([f, s, d]) => window.__h.setLook(f, s, d), look);
	}
	await settle(page);
	if (errors.length) throw new Error("page errors: " + errors.join("; "));
	page.__errors = errors;
	return page;
}

// settle waits for the render, the ResizeObserver measurement pass it
// schedules, and the frame that paints the corrected layout. Two rAFs plus a
// short tick: VirtualList measures in an effect and re-renders, so a single
// frame observes the pre-correction geometry.
export async function settle(page) {
	for (let i = 0; i < 3; i++) {
		await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
	}
	await page.waitForTimeout(50);
}

// probe returns the geometry the assertions are written against, measured
// against the real stylesheet in one round trip.
export function probe(page) {
	return page.evaluate(() => {
		const sc = document.querySelector(".msgs");
		const rows = [...sc.querySelectorAll("[data-vid]")];
		const scr = sc.getBoundingClientRect();
		const last = rows.at(-1)?.getBoundingClientRect();
		return {
			rows: rows.length,
			msgRows: sc.querySelectorAll(".msg-row").length,
			sysRows: sc.querySelectorAll(".sys-row").length,
			// Horizontal overflow: the message list must never become a
			// sideways scroller, at any width or preference combination.
			scrollWidth: sc.scrollWidth,
			clientWidth: sc.clientWidth,
			// Vertical coverage: how far the last rendered row falls short of
			// the bottom of the viewport. Positive = visible blank strip.
			shortfall: last ? Math.round(scr.bottom - last.bottom) : scr.height,
			atEnd: sc.scrollTop + sc.clientHeight >= sc.scrollHeight - 2,
			scrollTop: Math.round(sc.scrollTop),
			scrollHeight: Math.round(sc.scrollHeight),
			clientHeight: Math.round(sc.clientHeight),
		};
	});
}

// rowTop returns a rendered row's position relative to the scroller viewport,
// for anchoring assertions. Null when that row is not currently rendered.
export function rowTop(page, vid) {
	return page.evaluate((id) => {
		const sc = document.querySelector(".msgs");
		const el = sc.querySelector(`[data-vid="${id}"]`);
		if (!el) return null;
		return Math.round(el.getBoundingClientRect().top - sc.getBoundingClientRect().top);
	}, vid);
}

// midRowID returns the id of a row near the middle of the viewport — the one
// a reader is looking at, and the one a prepend must not move.
export function midRowID(page) {
	return page.evaluate(() => {
		const sc = document.querySelector(".msgs");
		const mid = sc.getBoundingClientRect().top + sc.clientHeight / 2;
		for (const el of sc.querySelectorAll("[data-vid]")) {
			const r = el.getBoundingClientRect();
			if (r.bottom >= mid) return el.dataset.vid;
		}
		return null;
	});
}

// touchHold / touchRelease latch and release VirtualList's touch gesture.
//
// This is not decoration: while the gesture is latched, the list DEFERS every
// row measurement (vlist.jsx — a scrollTop write cancels an iOS pan, so
// nothing may move under the finger). That deferral is precisely what makes
// the height ESTIMATES load-bearing, and a bad estimator only shows up then.
// Without a finger down, the ResizeObserver corrects every row before a test
// can observe anything, and a badly broken estimator still passes.
//
// The listeners are plain addEventListener, so a synthetic (untrusted) event
// is enough — no CDP needed.
export function touchHold(page, clientY) {
	return page.evaluate((y) => {
		const sc = document.querySelector(".msgs");
		const r = sc.getBoundingClientRect();
		const t = new Touch({ identifier: 1, target: sc, clientX: r.left + r.width / 2, clientY: r.top + y });
		sc.dispatchEvent(new TouchEvent("touchstart", {
			bubbles: true, cancelable: true, touches: [t], targetTouches: [t], changedTouches: [t],
		}));
	}, clientY);
}

export function touchRelease(page) {
	return page.evaluate(() => {
		const sc = document.querySelector(".msgs");
		sc.dispatchEvent(new TouchEvent("touchend", {
			bubbles: true, cancelable: true, touches: [], targetTouches: [], changedTouches: [],
		}));
	});
}

// panBack simulates the reported gesture: land at the live tail, put a finger
// down, and drag back through history without lifting it. Returns, for each
// frame, how many px of viewport sit BELOW the last rendered row — anything
// positive is blank screen the user can see.
//
// The whole loop runs inside ONE page.evaluate on rAF. Driving it from Node
// would put a CDP round trip between frames, which is not what a finger does
// and lets the list catch up between samples.
export function panBack(page, { steps = 40, px = 300 } = {}) {
	return page.evaluate(async ({ steps, px }) => {
		// rAF resolves the promise directly — wrapping it in `() => r()` only
		// discards the timestamp nobody reads, at the cost of a fifth level of
		// nesting.
		const frame = () => new Promise((r) => requestAnimationFrame(r));
		const dispatchTouch = (el, type) => {
			const t = new Touch({ identifier: 1, target: el, clientX: 200, clientY: 400 });
			const empty = type === "touchend";
			el.dispatchEvent(new TouchEvent(type, {
				bubbles: true, cancelable: true,
				touches: empty ? [] : [t], targetTouches: empty ? [] : [t], changedTouches: [t],
			}));
		};

		const sc = document.querySelector(".msgs");
		const touch = (type) => dispatchTouch(sc, type);

		sc.scrollTop = sc.scrollHeight;
		await frame(); await frame();
		touch("touchstart");

		const gaps = [];
		for (let i = 0; i < steps; i++) {
			sc.scrollTop = Math.max(0, sc.scrollTop - px);
			await frame(); await frame();
			const rows = sc.querySelectorAll("[data-vid]");
			if (!rows.length) continue;
			const last = rows[rows.length - 1].getBoundingClientRect();
			gaps.push(Math.round(sc.getBoundingClientRect().bottom - last.bottom));
		}
		// Lifting the finger flushes every deferred measurement at once and
		// the list compensates scrollTop by their summed error. That single
		// write is the visible "jerk" — a good estimator keeps it small.
		const before = sc.scrollTop;
		touch("touchend");
		await new Promise((r) => setTimeout(r, 400));
		return {
			gaps,
			viewportH: Math.round(sc.getBoundingClientRect().height),
			settleJump: Math.round(sc.scrollTop - before),
		};
	}, { steps, px });
}

export async function scrollTo(page, top) {
	await page.evaluate((t) => { document.querySelector(".msgs").scrollTop = t; }, top);
	await settle(page);
}
