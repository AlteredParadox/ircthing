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

// Layout regressions for the virtualized message list, driven against a real
// Chromium and the real stylesheet.
//
// Named `.browser.js`, NOT `.test.js`: `node --test` discovers `**/*.test.js`
// anywhere under the cwd, so that name would pull this into `make
// frontend-test` — the fast DOM-less unit run, which has no browser and must
// stay runnable on a fresh clone. `make browser-test` runs this file by path.
//
// These exist because three shipped bugs (all fixed in #19) were invisible to
// the DOM-less unit suite AND to code review, and reached a phone before
// anything caught them. Each test below is the SYMPTOM of one of them, not a
// restatement of its fix — a different wrong implementation should still fail
// these:
//
//   1. rows estimated from the raw IRC line came out far too tall, so the
//      rendered window stopped short of the viewport and the lower half of
//      the screen went blank while scrolling  -> "viewport stays covered"
//   2. a history page committed mid-gesture wrote scrollTop and yanked the
//      content under the finger                -> "prepend keeps the anchor"
//   3. unbreakable hostmasks on presence rows made the list a sideways
//      scroller at phone widths                -> "never scrolls sideways"
//
// What these CANNOT cover, and why the phone is still the last word: Chromium
// has no touch momentum, so the iOS behaviour that motivated fix 2 — a
// scrollTop write cancelling an in-flight pan — has no analogue here. This
// suite pins the geometry; it does not pin the feel.

import { after, before, describe, test } from "node:test";
import { ok } from "node:assert";
import { launch, midRowID, open, panBack, probe, rowTop, scrollTo, settle, shutdown, touchHold, touchRelease } from "./support.js";

// 360px is the documented minimum supported width (CLAUDE.md); 402 is the
// iPhone that produced the original report. Mono at xl is the widest the
// glyph advance ever gets, which is the worst case for both wrapping and the
// height estimate.
const VIEWPORTS = [
	{ name: "360x780 sans/md", width: 360, height: 780, look: ["sans", "md", "cozy"] },
	{ name: "402x874 mono/md", width: 402, height: 874, look: ["mono", "md", "cozy"] },
	{ name: "402x874 mono/xl", width: 402, height: 874, look: ["mono", "xl", "comfortable"] },
	{ name: "1280x900 sans/sm", width: 1280, height: 900, look: ["sans", "sm", "compact"] },
];

let browser;
before(async () => { browser = await launch(); });
after(shutdown);

describe("message list layout", () => {
	for (const vp of VIEWPORTS) {
		test(`${vp.name}: never scrolls sideways`, async () => {
			const page = await open(browser, vp);
			try {
				// The harness turns statusHost on, so presence rows carry the
				// full ident@host — the shapes with no break opportunity.
				const p = await probe(page);
				ok(p.sysRows > 0, "expected presence rows to be rendered");
				ok(
					p.scrollWidth <= p.clientWidth,
					`horizontal overflow: scrollWidth ${p.scrollWidth} > clientWidth ${p.clientWidth}`,
				);
			} finally {
				await page.close();
			}
		});

		test(`${vp.name}: panning back through history never blanks the viewport`, async () => {
			const page = await open(browser, vp, { touch: true });
			try {
				const { gaps, viewportH, settleJump } = await panBack(page);
				ok(gaps.length > 20, `only ${gaps.length} sampled frames`);
				const blank = gaps.filter((g) => g > 4);
				const worst = Math.max(0, ...gaps);
				ok(
					blank.length === 0,
					`${blank.length}/${gaps.length} frames left blank screen below the last row; ` +
					`worst ${worst}px of ${viewportH} (${Math.round((100 * worst) / viewportH)}%)`,
				);
				// The accumulated estimate error is paid back in one scrollTop write
				// when the finger lifts. Large error = a visible jerk, which is the
				// other half of the original report.
				ok(
					Math.abs(settleJump) <= 40,
					`scroll jumped ${settleJump}px when the gesture settled`,
				);
			} finally {
				await page.close();
			}
		});

	}

	test("prepend keeps the anchor row where the reader left it", async () => {
		const page = await open(browser, VIEWPORTS[1]);
		try {
			await scrollTo(page, 400);
			const anchor = await midRowID(page);
			ok(anchor, "expected a row mid-viewport to anchor on");
			const before_ = await rowTop(page, anchor);

			await page.evaluate(() => window.__h.prepend(120));
			await settle(page);

			const after_ = await rowTop(page, anchor);
			ok(after_ !== null, "anchor row stopped being rendered after the prepend");
			// The whole point of the scrollTop compensation: inserting older
			// history above must not move what is on screen.
			ok(
				Math.abs(after_ - before_) <= 2,
				`anchor moved ${after_ - before_}px (was ${before_}, now ${after_})`,
			);
		} finally {
			await page.close();
		}
	});

	test("history landing mid-gesture does not move the scroll position", async () => {
		// On iOS any scrollTop write cancels an in-flight pan AND its momentum,
		// so a history page that arrives while the finger is down must not be
		// allowed to reposition anything. Committing it (and compensating) is
		// deferred until the gesture settles.
		const page = await open(browser, VIEWPORTS[1], { touch: true });
		try {
			await scrollTo(page, 0);
			await touchHold(page, 200);
			const before_ = (await probe(page)).scrollTop;
			await page.evaluate(() => window.__h.prepend(120));
			await settle(page);
			const after_ = (await probe(page)).scrollTop;
			ok(
				after_ === before_,
				`scroll moved ${after_ - before_}px under the finger (was ${before_}, now ${after_})`,
			);
		} finally {
			await touchRelease(page).catch(() => {});
			await page.close();
		}
	});

	test("switching message font reflows without leaving a blank strip", async () => {
		// A pref change invalidates every measured row height at once, which
		// is the other path that used to strand the window mid-viewport.
		const page = await open(browser, VIEWPORTS[1]);
		try {
			await scrollTo(page, 600);
			await page.evaluate(() => window.__h.setLook("mono", "xl", "comfortable"));
			await settle(page);

			const p = await probe(page);
			ok(p.scrollWidth <= p.clientWidth, `horizontal overflow after reflow: ${p.scrollWidth} > ${p.clientWidth}`);
			ok(p.atEnd || p.shortfall <= 4, `blank strip after reflow: ${p.shortfall}px`);
		} finally {
			await page.close();
		}
	});
});
