import { useState } from "preact/hooks";

// NetworkForm: add/edit a network (The Lounge-style form). `initial` is
// the stored config object when editing (unshown fields like
// trusted_fingerprints are preserved by spreading it), null when adding.
// Saving reconnects the network; the server replies with an error string
// on invalid input, surfaced inline.

const SASL_CHOICES = ["none", "auto", "PLAIN", "SCRAM-SHA-256", "EXTERNAL"];

function saslChoice(cfg) {
	if (!cfg.sasl) return "none";
	return cfg.sasl.mechanism || "auto";
}

function Field({ label, children }) {
	return (
		<label class="nf-field">
			<span class="nf-label">{label}</span>
			{children}
		</label>
	);
}

export function NetworkForm({ initial, oldName, error, busy, onSave, onDelete, onClose }) {
	const [cfg, setCfg] = useState(() => ({
		tls: true,
		...initial,
	}));
	const [channels, setChannels] = useState((initial?.channels || []).join(" "));
	const [confirmDel, setConfirmDel] = useState(false);
	const set = (patch) => setCfg((c) => ({ ...c, ...patch }));
	const sasl = saslChoice(cfg);

	function pickSASL(choice) {
		if (choice === "none") set({ sasl: undefined });
		else {
			set({
				sasl: {
					...cfg.sasl,
					mechanism: choice === "auto" ? "" : choice,
				},
			});
		}
	}

	function submit(e) {
		e.preventDefault();
		const out = { ...cfg };
		out.channels = channels.split(/[\s,]+/).filter(Boolean);
		// Empty optional strings just clutter the stored JSON.
		for (const k of ["username", "realname", "pass", "proxy"]) {
			if (!out[k]) delete out[k];
		}
		if (!out.channels.length) delete out.channels;
		if (out.sasl) {
			const s2 = { ...out.sasl };
			for (const k of ["mechanism", "login", "password", "cert_file", "key_file"]) {
				if (!s2[k]) delete s2[k];
			}
			out.sasl = s2;
		} else {
			delete out.sasl;
		}
		onSave(out, oldName);
	}

	const valid = (cfg.addr || "").includes(":") && (cfg.nick || "").trim();
	return (
		<div class="search-scrim" aria-hidden="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
			<form class="settings-panel net-form" onSubmit={submit}>
				<div class="settings-head">
					<div class="settings-title">{oldName ? `Edit ${oldName}` : "Add network"}</div>
					<button type="button" class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="settings-body scroll">
					<section class="settings-section">
						<Field label="Name">
							<input class="rule-input" value={cfg.name || ""} onInput={(e) => set({ name: e.currentTarget.value })} placeholder="libera" />
						</Field>
						<Field label="Server">
							<input class="rule-input" value={cfg.addr || ""} onInput={(e) => set({ addr: e.currentTarget.value })} placeholder="irc.libera.chat:6697" />
						</Field>
						<div class="nf-check-row">
							<label class="nf-check">
								<input type="checkbox" checked={!!cfg.tls} onChange={(e) => set({ tls: e.currentTarget.checked })} />
								<span>Use TLS</span>
							</label>
							{!cfg.tls && (
								<label class="nf-check">
									<input type="checkbox" checked={!!cfg.allow_plaintext} onChange={(e) => set({ allow_plaintext: e.currentTarget.checked })} />
									<span>Allow plaintext (unencrypted)</span>
								</label>
							)}
						</div>
						<Field label="Nick">
							<input class="rule-input" value={cfg.nick || ""} onInput={(e) => set({ nick: e.currentTarget.value })} />
						</Field>
						<Field label="Username">
							<input class="rule-input" value={cfg.username || ""} onInput={(e) => set({ username: e.currentTarget.value })} placeholder="defaults to nick" />
						</Field>
						<Field label="Real name">
							<input class="rule-input" value={cfg.realname || ""} onInput={(e) => set({ realname: e.currentTarget.value })} />
						</Field>
						<Field label="Server password">
							<input class="rule-input" type="password" value={cfg.pass || ""} onInput={(e) => set({ pass: e.currentTarget.value })} />
						</Field>
						<Field label="Channels">
							<input class="rule-input" value={channels} onInput={(e) => setChannels(e.currentTarget.value)} placeholder="#go #linux" />
						</Field>
					</section>

					<section class="settings-section">
						<div class="settings-label">SASL</div>
						<Field label="Mechanism">
							<select class="rule-input" value={sasl} onChange={(e) => pickSASL(e.currentTarget.value)}>
								{SASL_CHOICES.map((c) => <option key={c} value={c}>{c}</option>)}
							</select>
						</Field>
						{sasl !== "none" && sasl !== "EXTERNAL" && (
							<>
								<Field label="Account">
									<input class="rule-input" value={cfg.sasl?.login || ""} onInput={(e) => set({ sasl: { ...cfg.sasl, login: e.currentTarget.value } })} />
								</Field>
								<Field label="Password">
									<input class="rule-input" type="password" value={cfg.sasl?.password || ""} onInput={(e) => set({ sasl: { ...cfg.sasl, password: e.currentTarget.value } })} />
								</Field>
							</>
						)}
						{sasl === "EXTERNAL" && (
							<>
								<Field label="Cert file (server path)">
									<input class="rule-input" value={cfg.sasl?.cert_file || ""} onInput={(e) => set({ sasl: { ...cfg.sasl, cert_file: e.currentTarget.value } })} />
								</Field>
								<Field label="Key file (server path)">
									<input class="rule-input" value={cfg.sasl?.key_file || ""} onInput={(e) => set({ sasl: { ...cfg.sasl, key_file: e.currentTarget.value } })} />
								</Field>
							</>
						)}
					</section>

					<section class="settings-section">
						<Field label="Proxy">
							<input class="rule-input" value={cfg.proxy || ""} onInput={(e) => set({ proxy: e.currentTarget.value })} placeholder="socks5://127.0.0.1:9050 (optional)" />
						</Field>
					</section>

					{error && <div class="cmd-error">{error}</div>}
					<div class="nf-actions">
						{oldName && (
							<button
								type="button"
								class={"nf-danger" + (confirmDel ? " confirm" : "")}
								onClick={() => (confirmDel ? onDelete(oldName) : setConfirmDel(true))}
							>
								{confirmDel ? "Really remove? This erases its scrollback" : "Remove network"}
							</button>
						)}
						<div class="nf-spacer" />
						<button type="submit" class="btn-accent" disabled={!valid || busy}>
							{oldName ? "Save & reconnect" : "Add & connect"}
						</button>
					</div>
				</div>
			</form>
		</div>
	);
}

// ChannelPrompt: the "Join a channel…" mini-dialog for a network. The
// input is NOT prefilled with "#": pasting "#chan" after a prefilled
// "#" silently made "##chan" — a different (and valid) channel. A
// missing chantype prefix is added on submit instead.
export function ChannelPrompt({ network, chantypes, error, onJoin, onClose }) {
	const [name, setName] = useState("");
	function submit(e) {
		e.preventDefault();
		let n = name.trim();
		if (!n) return;
		if (!(chantypes || "#").includes(n[0])) n = "#" + n;
		if (n.length > 1) onJoin(network, n);
	}
	return (
		<div class="search-scrim" aria-hidden="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
			<form
				class="settings-panel chan-prompt"
				onSubmit={submit}
			>
				<div class="settings-head">
					<div class="settings-title">Join a channel on {network}</div>
					<button type="button" class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="settings-body">
					<input
						class="rule-input"
						value={name}
						onInput={(e) => setName(e.currentTarget.value)}
						placeholder="#channel"
						autofocus
					/>
					{error && <div class="cmd-error">{error}</div>}
					<div class="nf-actions">
						<div class="nf-spacer" />
						<button type="submit" class="btn-accent" disabled={!name.trim()}>Join</button>
					</div>
				</div>
			</form>
		</div>
	);
}
