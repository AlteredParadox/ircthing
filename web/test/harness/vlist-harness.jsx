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

// Component-test harness for VirtualList: mounts the real component with
// 50k synthetic variable-height items and exposes hooks on window.__h for
// a browser driver (Playwright) to exercise append/prepend/scroll paths.
//
// Build (from web/):
//   node_modules/.bin/esbuild test/harness/vlist-harness.jsx --bundle \
//     --format=iife --jsx=automatic --jsx-import-source=preact \
//     --outfile=<dir>/vlist-harness.js
// and load it from an HTML page that gives .msgs a fixed height.
import { render } from "preact";
import { useState } from "preact/hooks";
import { VirtualList } from "../../src/vlist.jsx";
import { estimateMsgHeight } from "../../src/vmath.js";

let nextID = 1;
function mkMsg(len) {
	const id = nextID++;
	let text = `msg ${id} `;
	while (text.length < len) text += "lorem ipsum dolor ";
	return { id, raw: text };
}

const initial = [];
for (let i = 0; i < 50000; i++) initial.push(mkMsg(10 + ((i * 37) % 240)));

function App() {
	const [items, setItems] = useState(initial);
	const [layoutKey, setLayoutKey] = useState("default");
	window.__h = {
		itemCount: () => items.length,
		domRows: () => document.querySelectorAll("[data-vid]").length,
		append: (n) =>
			setItems((it) => [...it, ...Array.from({ length: n }, () => mkMsg(60))]),
		prepend: (n) =>
			setItems((it) => [...Array.from({ length: n }, () => mkMsg(60)), ...it]),
		filterEvery: (n) => setItems((it) => it.filter((_, i) => i % n !== 0)),
		invalidateLayout: () => setLayoutKey((k) => k + "!"),
		changeItem: (id, len) => setItems((it) => it.map((m) => m.id === id ? { ...m, raw: mkMsg(len).raw } : m)),
	};
	return (
		<VirtualList
			items={items}
			layoutKey={layoutKey}
			estimate={(m) => estimateMsgHeight(m.raw)}
			header={<div class="top-note">top of harness</div>}
			onNearTop={() => {
				window.__nearTops = (window.__nearTops || 0) + 1;
			}}
			onPinned={(p) => {
				window.__pinned = p;
			}}
			renderItem={(m) => <div class="row" data-msg={m.id}>{m.raw}</div>}
		/>
	);
}

window.__pinned = true;
render(<App />, document.getElementById("app"));
