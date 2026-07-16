import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";

// ContextMenu renders a right-click / long-press menu at (menu.x, menu.y),
// clamped to the viewport. `menu` is null (closed) or
// { x, y, title?, items: [{ label, danger?, onClick }] }. Closes on
// outside click, right-click, Escape, or an item choice.
export function ContextMenu({ menu, onClose }) {
	const ref = useRef(null);
	const [pos, setPos] = useState({ x: 0, y: 0 });

	// Clamp before paint so the menu never spills off-screen (no flicker).
	useLayoutEffect(() => {
		if (!menu || !ref.current) return;
		const r = ref.current.getBoundingClientRect();
		const vw = globalThis.innerWidth;
		const vh = globalThis.innerHeight;
		setPos({
			x: menu.x + r.width > vw ? Math.max(4, vw - r.width - 4) : menu.x,
			y: menu.y + r.height > vh ? Math.max(4, vh - r.height - 4) : menu.y,
		});
	}, [menu]);

	useEffect(() => {
		if (!menu) return;
		const onKey = (e) => e.key === "Escape" && onClose();
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, [menu]);

	if (!menu) return null;
	return (
		<div
			class="ctx-scrim"
			aria-hidden="true"
			onClick={(e) => e.target === e.currentTarget && onClose()}
			onContextMenu={(e) => {
				e.preventDefault();
				onClose();
			}}
		>
			<div class="ctx-menu" ref={ref} role="menu" style={{ left: pos.x, top: pos.y }}>
				{menu.title && <div class="ctx-title">{menu.title}</div>}
				{menu.items.map((it, i) => (
					it.divider ? <div class="ctx-div" key={"d" + i} /> :
					<button
						key={it.label}
						type="button"
						role="menuitem"
						class={"ctx-item" + (it.danger ? " danger" : "")}
						onClick={() => {
							onClose();
							it.onClick();
						}}
					>
						{it.label}
					</button>
				))}
			</div>
		</div>
	);
}

// menuTrigger wires an element so left-click, right-click, or
// Enter/Space all open a context menu via open(x, y). Left/right click
// open at the cursor; keyboard opens below the element. A tap on touch
// arrives as a click, so no separate long-press is needed.
export function menuTrigger(open) {
	const atCursor = (e) => {
		e.preventDefault();
		open(e.clientX, e.clientY);
	};
	return {
		role: "button",
		tabIndex: 0,
		onClick: atCursor,
		onContextMenu: atCursor,
		onKeyDown: (e) => {
			if (e.key === "Enter" || e.key === " ") {
				e.preventDefault();
				const r = e.currentTarget.getBoundingClientRect();
				open(r.left, r.bottom);
			}
		},
	};
}

// longPress returns touch handlers that fire open(x, y) after a ~500ms
// hold, for touch parity with right-click. It cancels on move or early
// release and marks the gesture so the element's onClick can ignore the
// tap that follows the press.
export function longPress(open, firedRef) {
	let timer = null;
	const cancel = () => {
		clearTimeout(timer);
		timer = null;
	};
	return {
		onTouchStart: (e) => {
			const t = e.touches[0];
			timer = setTimeout(() => {
				if (firedRef) firedRef.current = true;
				open(t.clientX, t.clientY);
			}, 500);
		},
		onTouchMove: cancel,
		onTouchEnd: cancel,
		onTouchCancel: cancel,
	};
}
