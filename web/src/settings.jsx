import { useEffect, useState } from "preact/hooks";

// Settings modal: desktop-notification permission and per-network
// highlight rules. Rules are edited live and persisted by the parent.
export function Settings({ networks, rules, onRules, notifier, onClose }) {
	const [perm, setPerm] = useState(notifier.permission());
	const [enabled, setEnabled] = useState(notifier.enabled);
	const netNames = Object.keys(networks).sort();

	useEffect(() => {
		const onKey = (e) => e.key === "Escape" && onClose();
		window.addEventListener("keydown", onKey);
		return () => window.removeEventListener("keydown", onKey);
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

	const addRule = () => onRules([...rules, { pattern: "", network: "" }]);
	const updateRule = (i, patch) => onRules(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
	const removeRule = (i) => onRules(rules.filter((_, j) => j !== i));

	return (
		<div class="search-scrim" onClick={onClose}>
			<div class="settings-panel" onClick={(e) => e.stopPropagation()}>
				<div class="settings-head">
					<div class="settings-title">Settings</div>
					<button class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="settings-body scroll">
					<section class="settings-section">
						<div class="settings-label">Desktop notifications</div>
						{perm === "unsupported" ? (
							<div class="settings-note">Not supported in this browser.</div>
						) : perm !== "granted" ? (
							<button class="btn-accent" onClick={enableNotif}>
								Enable desktop notifications
							</button>
						) : (
							<label class="settings-toggle">
								<input
									type="checkbox"
									checked={enabled}
									onChange={(e) => toggleNotif(e.currentTarget.checked)}
								/>
								<span>Notify on highlights and private messages</span>
							</label>
						)}
					</section>

					<section class="settings-section">
						<div class="settings-label">Highlight keywords</div>
						<div class="settings-note">
							Messages containing these words alert you like a mention. Scope to one
							network or all.
						</div>
						{rules.map((r, i) => (
							<div class="rule-row" key={i}>
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
