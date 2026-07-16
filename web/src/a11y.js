// pressable makes a non-native interactive element (a clickable div row)
// accessible: button role, tab focus, and Enter/Space activation — spread
// it where a real <button> would break the row layout.
export function pressable(onActivate) {
	return {
		role: "button",
		tabIndex: 0,
		onClick: onActivate,
		onKeyDown: (e) => {
			if (e.key === "Enter" || e.key === " ") {
				e.preventDefault();
				onActivate(e);
			}
		},
	};
}
