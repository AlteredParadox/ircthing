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

// ChangePassword posts to /api/password: verify current, set new (8–72 bytes,
// confirmed). The server rotates the stored login hash and revokes other
// sessions.
function ChangePassword() {
	const [current, setCurrent] = useState("");
	const [next, setNext] = useState("");
	const [confirm, setConfirm] = useState("");
	const [msg, setMsg] = useState(null); // { ok, text } | null
	// Single-flight: without this, a double submit sends two requests — the
	// first rotates the password, the serialized second then fails against
	// the NEW hash and its later response overwrites the success message
	// with "current password is incorrect".
	const [busy, setBusy] = useState(false);

	async function submit(e) {
		e.preventDefault();
		if (busy) return;
		setMsg(null);
		// bcrypt (and the server) bound the password in BYTES, not JS code units.
		const bytes = new TextEncoder().encode(next).length;
		if (bytes < 8 || bytes > 72) {
			setMsg({ ok: false, text: "New password must be 8–72 bytes." });
			return;
		}
		if (next !== confirm) {
			setMsg({ ok: false, text: "New passwords do not match." });
			return;
		}
		setBusy(true);
		try {
			const r = await fetch("/api/password", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ current, new: next }),
			});
			if (r.ok) {
				setMsg({ ok: true, text: "Password changed." });
				setCurrent("");
				setNext("");
				setConfirm("");
			} else {
				setMsg({ ok: false, text: (await r.text()).trim() || "Change failed." });
			}
		} catch {
			setMsg({ ok: false, text: "Change failed." });
		} finally {
			setBusy(false);
		}
	}

	return (
		<form class="settings-section" onSubmit={submit}>
			<div class="settings-label">Change password</div>
			<div class="pref-row">
				<span class="pref-name">Current</span>
				<input class="pref-input" type="password" autocomplete="current-password" value={current} onInput={(e) => setCurrent(e.currentTarget.value)} />
			</div>
			<div class="pref-row">
				<span class="pref-name">New</span>
				<input class="pref-input" type="password" autocomplete="new-password" value={next} onInput={(e) => setNext(e.currentTarget.value)} />
			</div>
			<div class="pref-row">
				<span class="pref-name">Confirm</span>
				<input class="pref-input" type="password" autocomplete="new-password" value={confirm} onInput={(e) => setConfirm(e.currentTarget.value)} />
			</div>
			{msg && <div class={msg.ok ? "settings-note" : "cmd-error"}>{msg.text}</div>}
			<button class="btn-accent" type="submit" disabled={busy || !current || !next}>{busy ? "Changing…" : "Change password"}</button>
		</form>
	);
}

// saveConfig PUTs a settings patch and reports whether the server accepted
// it (2xx). Callers revert their optimistic UI on false so this session
// can't silently diverge from the persisted state (and other devices).
async function saveConfig(patch) {
	try {
		const r = await fetch("/api/config", {
			method: "PUT",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify(patch),
		});
		return r.ok;
	} catch {
		return false;
	}
}

// Mutation-ordering state for the server-side settings, at MODULE level
// deliberately: these used to be per-modal-instance refs, so closing and
// reopening Settings created an independent save queue while the old
// instance's requests were still in flight — a delayed older "previews on"
// PUT could land after a newer "previews off" PUT and its stale callback
// could still flip onPreviews, silently reversing the user's privacy choice.
// One page-wide queue and one generation counter per setting serialize and
// version every save regardless of how many times the modal remounts.
// saveQueue: the next PUT is not sent until the previous settles, so an older
// request can never land after (and overwrite) a newer one server-side.
// *Gen: a callback only applies its result if it is still the LATEST save of
// that setting. Retention tracks its two dimensions (days, max) SEPARATELY:
// with a shared counter, a later max save suppressed the rollback of an
// earlier failed days save, leaving the UI showing a policy the server never
// accepted. *Saved: the last value the server CONFIRMED — failed saves roll
// the display back to it, never to an unpersisted in-memory edit.
const saveQueue = { current: Promise.resolve() };
const retentionGens = { days: { current: 0 }, max: { current: 0 } };
const sessionGen = { current: 0 };
const previewsGen = { current: 0 };
const retentionSaved = { current: null };
const sessionSaved = { current: null };
const previewsSaved = { current: null };

// authEpoch scopes all queued settings work to one login session. Being
// module-global, the queue survives logout — without the epoch, a mutation
// queued before signing out would execute against the NEXT session's cookie,
// and a delayed previews callback could re-pin stale UI state after the
// login-phase reset. Work enqueued under an old epoch becomes a no-op.
const authEpoch = { current: 0 };

// enqueue serializes work on the save queue, dropping it (and anything it
// would have applied) if a logout/login boundary was crossed since it was
// queued. work receives a stale() probe and MUST recheck it after every
// await: the pre-call check alone can't stop a request already in flight
// when the epoch bumps, and its post-await callbacks would apply results
// (or re-pin preview state) into the NEXT session's UI.
function enqueue(work) {
	const epoch = authEpoch.current;
	const stale = () => epoch !== authEpoch.current;
	saveQueue.current = saveQueue.current.then(() => {
		if (stale()) return undefined; // crossed a logout: drop
		return work(stale);
	});
}

// resetSettingsSession invalidates every queued settings mutation and pending
// callback, and clears the confirmed-value baselines. Called by the app shell
// when the authenticated phase ends (logout, session expiry). Replacing the
// queue head DETACHES the new session from the old chain: a hung old request
// can no longer block the next session's saves (its own continuations still
// no-op via the epoch).
export function resetSettingsSession() {
	authEpoch.current++;
	saveQueue.current = Promise.resolve();
	retentionSaved.current = null;
	sessionSaved.current = null;
	previewsSaved.current = null;
}

// Settings modal: appearance preferences, desktop-notification
// permission, and per-network highlight rules. Everything is edited live
// and persisted by the parent.
export function Settings({ networks, rules, onRules, prefs, onPrefs, notifier, onPreviews, onClose, onLogout }) {
	const [perm, setPerm] = useState(notifier.permission());
	const [enabled, setEnabled] = useState(notifier.enabled);
	const [logoutErr, setLogoutErr] = useState(false);
	const netNames = Object.keys(networks).sort((a, b) => a.localeCompare(b));

	// Server-side settings, loaded on open and applied on change. The save
	// queue, generation guards, and confirmed-value baselines live at module
	// level (see above) so they survive the modal closing and reopening.
	const [previewsOn, setPreviewsOn] = useState(null); // null while loading
	const [retention, setRetention] = useState(null); // { days, max } | null
	const [sessionDays, setSessionDays] = useState(null); // login cookie lifetime

	useEffect(() => {
		const onKey = (e) => e.key === "Escape" && onClose();
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, []);

	useEffect(() => {
		// Load THROUGH the save queue, not alongside it: an unversioned GET racing
		// an in-flight save (e.g. the modal reopened while a previous instance's
		// PUT is still settling) would return the pre-save value and overwrite the
		// confirmed baselines with a stale snapshot — a later edit would then
		// silently undo the newer value. Enqueuing the fetch serializes it after
		// every pending save; the per-field generation guards additionally skip
		// applying any field the user re-edited while the GET itself was running.
		let alive = true;
		enqueue(async (stale) => {
			const genD = retentionGens.days.current;
			const genM = retentionGens.max.current;
			const genS = sessionGen.current;
			const genP = previewsGen.current;
			let d = null;
			try {
				const r = await fetch("/api/config");
				d = r.ok ? await r.json() : null;
			} catch {
				d = null;
			}
			if (!d || stale()) return; // logged out while the GET ran: drop it
			// Not `|0`: that wraps values above 2^31-1 negative, corrupting
			// the display and making every later retention save a 400.
			const num = (v) => Math.max(0, Math.floor(Number(v) || 0));
			if (previewsGen.current === genP) {
				previewsSaved.current = !!d.previews; // seed the confirmed baseline
				if (alive) setPreviewsOn(!!d.previews);
			}
			if (retentionGens.days.current === genD && retentionGens.max.current === genM) {
				const ret = { days: num(d.retention_days), max: num(d.retention_max_messages) };
				retentionSaved.current = ret;
				if (alive) setRetention(ret);
			}
			if (sessionGen.current === genS) {
				const sess = num(d.session_ttl_days);
				sessionSaved.current = sess;
				if (alive) setSessionDays(sess);
			}
		});
		return () => {
			alive = false;
		};
	}, []);

	// Saves are serialized through the module-level saveQueue and versioned by
	// the module-level generation guards — see the block above saveConfig.
	function saveRetention(patch) {
		const dim = "days" in patch ? "days" : "max";
		const next = { ...retention, ...patch };
		setRetention(next);
		const gen = ++retentionGens[dim].current;
		// Send ONLY the edited dimension. The API is a partial patch (pointer
		// fields, read-modify-write under a server-side lock), so including the
		// untouched field from this client's possibly-stale snapshot would
		// overwrite a newer value another device (or a racing save) just wrote —
		// premature deletion or indefinite retention, silently.
		const body = dim === "days" ? { retention_days: patch.days } : { retention_max_messages: patch.max };
		enqueue(async (stale) => {
			const ok = await saveConfig(body);
			if (stale()) return; // logged out while the PUT ran: don't touch UI or baselines
			// Confirm only what this PUT actually carried: merge the patch into
			// the baseline rather than adopting the whole local snapshot.
			if (ok) retentionSaved.current = { ...(retentionSaved.current || next), ...patch };
			else if (gen === retentionGens[dim].current && retentionSaved.current) {
				// Roll back ONLY the failed dimension: the generations are
				// per-dimension, so a later (successful) save of the OTHER
				// dimension can't suppress this rollback and leave the UI
				// showing a policy the server never accepted.
				setRetention((cur) => ({ ...cur, [dim]: retentionSaved.current[dim] }));
			}
		});
	}
	function saveSessionDays(days) {
		setSessionDays(days);
		const gen = ++sessionGen.current;
		enqueue(async (stale) => {
			const ok = await saveConfig({ session_ttl_days: days });
			if (stale()) return;
			if (ok) sessionSaved.current = days;
			else if (gen === sessionGen.current && sessionSaved.current != null) setSessionDays(sessionSaved.current);
		});
	}
	const retNum = (v) => Math.max(0, Number.parseInt(v, 10) || 0);

	function togglePreviews(on) {
		// Only reflect the new state once the server confirms the write — a
		// failed save leaves the toggle where it was, so this session and other
		// devices do not diverge. Serialized through saveQueue with a generation
		// guard (same as saveRetention): two rapid toggles would otherwise race
		// their PUTs, and the response completing LAST — not the one carrying the
		// user's final choice — would set the visible state.
		const gen = ++previewsGen.current;
		enqueue(async (stale) => {
			const ok = await saveConfig({ previews: on });
			if (stale()) return; // logged out mid-flight: a late callback must not re-pin previews
			if (ok) {
				previewsSaved.current = on; // record every server-confirmed value
				if (gen === previewsGen.current) {
					setPreviewsOn(on);
					onPreviews?.(on);
				}
			} else if (gen === previewsGen.current && previewsSaved.current != null) {
				// Latest save failed: reconcile the UI to the last value the
				// server actually holds (an earlier queued save may have
				// succeeded), not to the failed toggle's target.
				setPreviewsOn(previewsSaved.current);
				onPreviews?.(previewsSaved.current);
			}
		});
	}

	// Sign out deliberately invalidates the CURRENT session server-side
	// (password rotation also revokes it, rotating onto a fresh cookie).
	// Only leave the app once the server confirms the revocation; a network
	// failure must not show the login screen over a still-valid session.
	async function logout() {
		setLogoutErr(false);
		try {
			const r = await fetch("/api/logout", { method: "POST" });
			if (!r.ok) throw new Error();
			onLogout?.();
		} catch {
			setLogoutErr(true);
		}
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
									onChange={(e) => saveSessionDays(Math.max(1, Number.parseInt(e.currentTarget.value, 10) || 1))}
								/>
							</div>
						)}
						<div class="settings-note">
							Signing out invalidates this device’s login immediately (other
							devices stay signed in).
						</div>
						{logoutErr && <div class="cmd-error">Sign out failed — try again.</div>}
						<button class="btn-accent" onClick={logout}>Sign out</button>
					</section>

					<ChangePassword />

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

					<section class="settings-section">
						<div class="settings-label">About</div>
						<div class="settings-note">
							ircthing is free software, licensed under the{" "}
							<a href="/license" target="_blank" rel="noopener noreferrer">
								GNU AGPL v3 or later
							</a>. Get the{" "}
							<a href="/source" target="_blank" rel="noopener noreferrer">
								source code
							</a>{" "}
							for this exact build. Bundled{" "}
							<a href="/third-party-licenses" target="_blank" rel="noopener noreferrer">
								third-party licenses
							</a>.
						</div>
					</section>
				</div>
			</div>
		</div>
	);
}
