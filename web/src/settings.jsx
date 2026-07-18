import { useEffect, useState } from "preact/hooks";
import { ACCENT_RGB, ACCENTS, CLOCKS, MAX_NICK_SEP } from "./prefs.js";
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
export function Settings({ networks, rules, onRules, prefs, onPrefs, notifier, onPreviews, onClose }) {
	const [perm, setPerm] = useState(notifier.permission());
	const [enabled, setEnabled] = useState(notifier.enabled);
	const netNames = Object.keys(networks).sort((a, b) => a.localeCompare(b));

	// Server-side settings, loaded on open and applied on change.
	const [previewsOn, setPreviewsOn] = useState(null); // null while loading
	const [retention, setRetention] = useState(null); // { days, max } | null
	const [sessionDays, setSessionDays] = useState(null); // login cookie lifetime

	useEffect(() => {
		const onKey = (e) => e.key === "Escape" && onClose();
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, []);

	useEffect(() => {
		fetch("/api/config")
			.then((r) => (r.ok ? r.json() : null))
			.then((d) => {
				if (!d) return;
				setPreviewsOn(!!d.previews);
				setRetention({ days: d.retention_days | 0, max: d.retention_max_messages | 0 });
				setSessionDays(d.session_ttl_days | 0);
			})
			.catch(() => {});
	}, []);

	async function saveConfig(patch) {
		try {
			await fetch("/api/config", {
				method: "PUT",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify(patch),
			});
		} catch {
			/* leave optimistic UI; a reopen reloads the server truth */
		}
	}

	function saveRetention(patch) {
		const next = { ...retention, ...patch };
		setRetention(next);
		saveConfig({ retention_days: next.days, retention_max_messages: next.max });
	}
	function saveSessionDays(days) {
		setSessionDays(days);
		saveConfig({ session_ttl_days: days });
	}
	const retNum = (v) => Math.max(0, parseInt(v, 10) || 0);

	async function togglePreviews(on) {
		// Only reflect the new state once the server confirms the write.
		// A failed/non-2xx save must leave the toggle where it was, so
		// this session and other devices do not diverge (and previews are
		// not silently left enabled).
		try {
			const r = await fetch("/api/config", {
				method: "PUT",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ previews: on }),
			});
			if (!r.ok) return;
		} catch {
			return;
		}
		setPreviewsOn(on);
		onPreviews?.(on);
	}

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
							<span class="pref-name">Sidebar width</span>
							<Seg
								value={prefs.sidebarWidth}
								options={["compact", "comfortable", "wide"]}
								labels={["Compact", "Comfortable", "Wide"]}
								onPick={(sidebarWidth) => onPrefs({ ...prefs, sidebarWidth })}
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
						<div class="settings-label">Timestamps &amp; names</div>
						<div class="pref-row">
							<span class="pref-name">Clock</span>
							<Seg
								value={prefs.clock}
								options={CLOCKS}
								labels={["24-hour", "12-hour"]}
								onPick={(clock) => onPrefs({ ...prefs, clock })}
							/>
						</div>
						<div class="pref-row">
							<span class="pref-name">Seconds</span>
							<Seg
								value={prefs.seconds ? "on" : "off"}
								options={["off", "on"]}
								labels={["Hide", "Show"]}
								onPick={(v) => onPrefs({ ...prefs, seconds: v === "on" })}
							/>
						</div>
						<div class="pref-row">
							<span class="pref-name">AM/PM {prefs.clock === "24" && <span class="pref-hint">(12-hour only)</span>}</span>
							<Seg
								value={prefs.ampm ? "on" : "off"}
								options={["off", "on"]}
								labels={["Hide", "Show"]}
								onPick={(v) => onPrefs({ ...prefs, ampm: v === "on" })}
							/>
						</div>
						<div class="pref-row">
							<span class="pref-name">Nick separator</span>
							<input
								class="pref-input"
								value={prefs.nickSep}
								maxLength={MAX_NICK_SEP}
								placeholder="none"
								aria-label="Character shown after a nick (e.g. a colon)"
								onInput={(e) => onPrefs({ ...prefs, nickSep: e.currentTarget.value })}
							/>
						</div>
						<div class="settings-note">
							Shown after the nick before each message — e.g. a colon renders “AlteredParadox: hello”.
						</div>
						<div class="pref-row">
							<span class="pref-name">Highlight names in messages</span>
							<Seg
								value={prefs.highlightNames ? "on" : "off"}
								options={["off", "on"]}
								labels={["Off", "On"]}
								onPick={(v) => onPrefs({ ...prefs, highlightNames: v === "on" })}
							/>
						</div>
						<div class="settings-note">
							Colors and links nicknames mentioned inside message text (right-click for
							the user menu). The sender’s name is always highlighted.
						</div>
					</section>

					<section class="settings-section">
						<div class="settings-label">Custom CSS</div>
						<div class="settings-note">
							Applied live, on top of the theme. Synced across your devices.
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
						<div class="settings-label">Link previews</div>
						<div class="settings-note">
							Previews and image thumbnails are fetched by the server, each through
							its link's network proxy — a link in a proxied network is previewed
							over that proxy (no IP leak), one in a direct network goes direct.
							Off by default: an auto-fetched preview is a tracking beacon — whoever
							posts a link can tell when you open the buffer. Leave off for zero
							outbound fetches. Applies immediately.
						</div>
						{previewsOn !== null && (
							<div class="pref-row">
								<span class="pref-name">Show link previews</span>
								<Seg
									value={previewsOn ? "on" : "off"}
									options={["off", "on"]}
									labels={["Off", "On"]}
									onPick={(v) => togglePreviews(v === "on")}
								/>
							</div>
						)}
					</section>

					<section class="settings-section">
						<div class="settings-label">History retention</div>
						<div class="settings-note">
							Prune stored message history in the background. 0 = keep forever. Applies
							to the server database (older scrollback beyond the in-memory cache); a
							lower limit prunes promptly.
						</div>
						{retention !== null && (
							<>
								<div class="pref-row">
									<span class="pref-name">Delete after (days)</span>
									<input
										class="pref-input"
										type="number"
										min="0"
										value={retention.days}
										onChange={(e) => saveRetention({ days: retNum(e.currentTarget.value) })}
									/>
								</div>
								<div class="pref-row">
									<span class="pref-name">Max messages per buffer</span>
									<input
										class="pref-input"
										type="number"
										min="0"
										value={retention.max}
										onChange={(e) => saveRetention({ max: retNum(e.currentTarget.value) })}
									/>
								</div>
							</>
						)}
					</section>

					<section class="settings-section">
						<div class="settings-label">Session</div>
						<div class="settings-note">
							How long a login stays valid before you have to sign in again. Applies
							to new logins.
						</div>
						{sessionDays !== null && (
							<div class="pref-row">
								<span class="pref-name">Stay signed in (days)</span>
								<input
									class="pref-input"
									type="number"
									min="1"
									value={sessionDays}
									onChange={(e) => saveSessionDays(Math.max(1, parseInt(e.currentTarget.value, 10) || 1))}
								/>
							</div>
						)}
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
