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
	window.__h = {
		itemCount: () => items.length,
		domRows: () => document.querySelectorAll("[data-vid]").length,
		append: (n) =>
			setItems((it) => [...it, ...Array.from({ length: n }, () => mkMsg(60))]),
		prepend: (n) =>
			setItems((it) => [...Array.from({ length: n }, () => mkMsg(60)), ...it]),
	};
	return (
		<VirtualList
			items={items}
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
