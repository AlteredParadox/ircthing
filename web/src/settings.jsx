import { useEffect, useState } from "preact/hooks";
import { ACCENT_RGB, ACCENTS } from "./prefs.js";
import { uuid } from "./irc.js";

// NotifControl renders the notification section for the current
// permission state: unsupported, not yet granted, or toggleable.
function NotifControl({ perm, enabled, onEnable, onToggle }) {
	if (perm === "unsupported") {
		return <div class="settings-note">Not supported in this browser.</div>;
	}
	if (perm === "granted") {
		return (
			<label class="settings-toggle">
				<input
					type="checkbox"
					checked={enabled}
					onChange={(e) => onToggle(e.currentTarget.checked)}
				/>
				<span>Notify on highlights and private messages</span>
			</label>
		);
	}
	return (
		<button class="btn-accent" onClick={onEnable}>
			Enable desktop notifications
		</button>
	);
}

// Seg: a small segmented control — one button per option.
function Seg({ value, options, labels, onPick }) {
	return (
		<div class="seg">
			{options.map((o, i) => (
				<button
					key={o}
					class={o === value ? "on" : ""}
					onClick={() => onPick(o)}
				>{labels ? labels[i] : o}</button>
			))}
		</div>
	);
}

// Settings modal: appearance preferences, desktop-notification
// permission, and per-network highlight rules. Everything is edited live
// and persisted by the parent.
export function Settings({ networks, rules, onRules, prefs, onPrefs, notifier, onClose }) {
	const [perm, setPerm] = useState(notifier.permission());
	const [enabled, setEnabled] = useState(notifier.enabled);
	const netNames = Object.keys(networks).sort((a, b) => a.localeCompare(b));

	useEffect(() => {
		const onKey = (e) => e.key === "Escape" && onClose();
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, []);

	async function enableNotif() {
		await notifier.requestAndEnable();
		setPerm(notifier.permission());
		setEnabled(notifier.enabled);
	}
	function toggleNotif(on) {
		notifier.setEnabled(on);
		setEnabled(notifier.enabled);
	}

	const addRule = () => onRules([...rules, { id: uuid(), pattern: "", network: "" }]);
	const updateRule = (i, patch) => onRules(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
	const removeRule = (i) => onRules(rules.filter((_, j) => j !== i));

	return (
		<div class="search-scrim" aria-hidden="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
			<div class="settings-panel">
				<div class="settings-head">
					<div class="settings-title">Settings</div>
					<button class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="settings-body scroll">
					<section class="settings-section">
						<div class="settings-label">Appearance</div>
						<div class="pref-row">
							<span class="pref-name">Theme</span>
							<Seg
								value={prefs.theme}
								options={["system", "dark", "light"]}
								labels={["System", "Dark", "Light"]}
								onPick={(theme) => onPrefs({ ...prefs, theme })}
							/>
						</div>
						<div class="pref-row">
							<span class="pref-name">Accent</span>
							<div class="swatches">
								{ACCENTS.map((a) => (
									<button
										key={a}
										class={"swatch" + (a === prefs.accent ? " on" : "")}
										style={{ background: `rgb(${ACCENT_RGB[a]})` }}
										title={a}
										onClick={() => onPrefs({ ...prefs, accent: a })}
									/>
								))}
							</div>
						</div>
						<div class="pref-row">
							<span class="pref-name">Text size</span>
							<Seg
								value={prefs.textSize}
								options={["sm", "md", "lg", "xl"]}
								labels={["S", "M", "L", "XL"]}
								onPick={(textSize) => onPrefs({ ...prefs, textSize })}
							/>
						</div>
						<div class="pref-row">
							<span class="pref-name">Density</span>
							<Seg
								value={prefs.density}
								options={["compact", "cozy", "comfortable"]}
								labels={["Compact", "Cozy", "Comfortable"]}
								onPick={(density) => onPrefs({ ...prefs, density })}
							/>
						</div>
						<div class="pref-row">
							<span class="pref-name">Message font</span>
							<Seg
								value={prefs.msgFont}
								options={["sans", "mono"]}
								labels={["Sans", "Mono"]}
								onPick={(msgFont) => onPrefs({ ...prefs, msgFont })}
							/>
						</div>
						<div class="pref-row">
							<span class="pref-name">Joins &amp; parts</span>
							<Seg
								value={prefs.statusMsgs}
								options={["show", "collapse", "hide"]}
								labels={["Show", "Collapse", "Hide"]}
								onPick={(statusMsgs) => onPrefs({ ...prefs, statusMsgs })}
							/>
						</div>
					</section>

					<section class="settings-section">
						<div class="settings-label">Custom CSS</div>
						<div class="settings-note">
							Applied live, on top of the theme. Saved in this browser.
						</div>
						<textarea
							class="css-input"
							rows={4}
							spellcheck={false}
							placeholder={":root { --accent-rgb: 219 72 120; }"}
							value={prefs.css}
							onInput={(e) => onPrefs({ ...prefs, css: e.currentTarget.value })}
						/>
					</section>

					<section class="settings-section">
						<div class="settings-label">Desktop notifications</div>
						<NotifControl
							perm={perm} enabled={enabled}
							onEnable={enableNotif} onToggle={toggleNotif}
						/>
					</section>

					<section class="settings-section">
						<div class="settings-label">Highlight keywords</div>
						<div class="settings-note">
							Messages containing these words alert you like a mention. Scope to one
							network or all.
						</div>
						{rules.map((r, i) => (
							<div class="rule-row" key={r.id}>
								<input
									class="rule-input"
									value={r.pattern}
									placeholder="keyword"
									onInput={(e) => updateRule(i, { pattern: e.currentTarget.value })}
								/>
								<select
									class="rule-net"
									value={r.network}
									onChange={(e) => updateRule(i, { network: e.currentTarget.value })}
								>
									<option value="">All networks</option>
									{netNames.map((n) => (
										<option value={n} key={n}>{n}</option>
									))}
								</select>
								<button class="rule-remove" onClick={() => removeRule(i)} title="Remove">✕</button>
							</div>
						))}
						<button class="settings-add" onClick={addRule}>+ Add keyword</button>
					</section>
				</div>
			</div>
		</div>
	);
}
