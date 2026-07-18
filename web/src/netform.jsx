import { useState } from "preact/hooks";
import { proxyCredsExposed } from "./irc.js";

// NetworkForm: add/edit a network (The Lounge-style form). `initial` is
// the stored config object when editing (spread so any field this form does
// not surface is preserved), null when adding. Saving reconnects the network;
// the server replies with an error string on invalid input (e.g. a malformed
// pinned fingerprint), surfaced inline.

const SASL_CHOICES = ["none", "auto", "PLAIN", "SCRAM-SHA-256", "EXTERNAL"];

function saslChoice(cfg) {
	if (!cfg.sasl) return "none";
	return cfg.sasl.mechanism || "auto";
}

// Egress: direct, a SOCKS5/HTTP proxy, or an in-process WireGuard tunnel.
// proxy and wireguard are mutually exclusive (the server rejects both).
const EGRESS_CHOICES = [
	["direct", "Direct"],
	["proxy", "Proxy (SOCKS5 / HTTP)"],
	["wireguard", "WireGuard tunnel"],
];

function egressChoice(cfg) {
	if (cfg.wireguard) return "wireguard";
	if (cfg.proxy) return "proxy";
	return "direct";
}

// wireguardOut normalizes the typed WG block for storage: the empty optional
// preshared key is dropped, mtu kept only when it parses as a positive
// number. Spreading undefined is a no-op, so a never-touched block yields {}
// and the required-field check (wgReady) still gates save.
function wireguardOut(wg) {
	const clean = { ...wg };
	if (!clean.preshared_key) delete clean.preshared_key;
	const mtu = Number.parseInt(clean.mtu, 10);
	if (mtu > 0) clean.mtu = mtu;
	else delete clean.mtu;
	return clean;
}

// saslOut drops empty optional SASL fields so they don't clutter the stored
// JSON.
function saslOut(sasl) {
	const clean = { ...sasl };
	for (const k of ["mechanism", "login", "password", "cert_file", "key_file"]) {
		if (!clean[k]) delete clean[k];
	}
	return clean;
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
	const [fingerprints, setFingerprints] = useState((initial?.trusted_fingerprints || []).join("\n"));
	const [confirmDel, setConfirmDel] = useState(false);
	const [egress, setEgress] = useState(() => egressChoice(initial || {}));
	const set = (patch) => setCfg((c) => ({ ...c, ...patch }));
	const setWG = (patch) => setCfg((c) => ({ ...c, wireguard: { ...c.wireguard, ...patch } }));
	const sasl = saslChoice(cfg);

	// pickEgress switches egress mode. It does NOT clear the other block, so a
	// typed proxy URL or WireGuard config survives toggling away and back; submit()
	// is the single authority that keeps only the selected mode's block. It just
	// seeds an empty wireguard object so the WG fields render.
	function pickEgress(mode) {
		setEgress(mode);
		if (mode === "wireguard" && !cfg.wireguard) set({ wireguard: {} });
	}

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
		out.trusted_fingerprints = fingerprints.split(/[\s,]+/).filter(Boolean);
		// Egress is exactly one of direct / proxy / wireguard. The form keeps
		// typed proxy/WG values across toggles, so submit is the SOLE authority on
		// what gets stored: keep only the selected mode's block, drop the others.
		if (egress === "wireguard") out.wireguard = wireguardOut(out.wireguard);
		else delete out.wireguard;
		if (egress !== "proxy") delete out.proxy;
		// Empty optional strings just clutter the stored JSON.
		for (const k of ["username", "realname", "pass", "proxy"]) {
			if (!out[k]) delete out[k];
		}
		if (!out.channels.length) delete out.channels;
		if (!out.trusted_fingerprints.length) delete out.trusted_fingerprints;
		if (out.sasl) out.sasl = saslOut(out.sasl);
		else delete out.sasl;
		onSave(out, oldName);
	}

	const wg = cfg.wireguard || {};
	const wgReady = egress !== "wireguard" ||
		(wg.private_key && wg.peer_public_key && wg.endpoint && wg.address && wg.dns);
	const valid = (cfg.addr || "").includes(":") && (cfg.nick || "").trim() && wgReady;
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
						{cfg.tls && (
							<Field label="Pinned cert fingerprints">
								<textarea
									class="rule-input"
									rows={2}
									spellcheck={false}
									value={fingerprints}
									onInput={(e) => setFingerprints(e.currentTarget.value)}
									placeholder="SHA-256 hex, one per line (optional; for self-signed servers)"
								/>
							</Field>
						)}
						<Field label="Nick">
							<input class="rule-input" value={cfg.nick || ""} onInput={(e) => set({ nick: e.currentTarget.value })} />
						</Field>
						<Field label="Username">
							<input class="rule-input" autocomplete="off" value={cfg.username || ""} onInput={(e) => set({ username: e.currentTarget.value })} placeholder="defaults to nick" />
						</Field>
						<Field label="Real name">
							<input class="rule-input" value={cfg.realname || ""} onInput={(e) => set({ realname: e.currentTarget.value })} />
						</Field>
						<Field label="Server password">
							<input class="rule-input" type="password" autocomplete="new-password" value={cfg.pass || ""} onInput={(e) => set({ pass: e.currentTarget.value })} />
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
									<input class="rule-input" autocomplete="off" value={cfg.sasl?.login || ""} onInput={(e) => set({ sasl: { ...cfg.sasl, login: e.currentTarget.value } })} />
								</Field>
								<Field label="Password">
									<input class="rule-input" type="password" autocomplete="new-password" value={cfg.sasl?.password || ""} onInput={(e) => set({ sasl: { ...cfg.sasl, password: e.currentTarget.value } })} />
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
						<div class="settings-label">Egress</div>
						<Field label="Route through">
							<select class="rule-input" value={egress} onChange={(e) => pickEgress(e.currentTarget.value)}>
								{EGRESS_CHOICES.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
							</select>
						</Field>
						{egress === "proxy" && (
							<>
								<Field label="Proxy URL">
									<input type="password" autocomplete="off" class="rule-input" value={cfg.proxy || ""} onInput={(e) => set({ proxy: e.currentTarget.value })} placeholder="socks5://127.0.0.1:9050" />
								</Field>
								{proxyCredsExposed(cfg.proxy) && (
									<div class="nf-warn">
										⚠ This proxy sends a username/password to a non-loopback host. SOCKS5
										and HTTP proxy auth are transmitted <b>unencrypted</b>, so the
										credentials travel in the clear unless the connection to the proxy is
										itself protected (a VPN or SSH tunnel).{" "}
										{cfg.tls
											? "Your IRC traffic still runs TLS inside the tunnel, so only the proxy login is exposed."
											: "This network is plaintext (no TLS), so the proxy also sees your IRC traffic itself — enable TLS."}
									</div>
								)}
							</>
						)}
						{egress === "wireguard" && (
							<>
								<Field label="Private key">
									<input class="rule-input" type="password" autocomplete="new-password" value={wg.private_key || ""} onInput={(e) => setWG({ private_key: e.currentTarget.value })} placeholder="base64 (wg genkey)" />
								</Field>
								<Field label="Peer public key">
									<input class="rule-input" autocomplete="off" value={wg.peer_public_key || ""} onInput={(e) => setWG({ peer_public_key: e.currentTarget.value })} placeholder="base64" />
								</Field>
								<Field label="Preshared key">
									<input class="rule-input" type="password" autocomplete="new-password" value={wg.preshared_key || ""} onInput={(e) => setWG({ preshared_key: e.currentTarget.value })} placeholder="base64 (optional)" />
								</Field>
								<Field label="Endpoint">
									<input class="rule-input" autocomplete="off" value={wg.endpoint || ""} onInput={(e) => setWG({ endpoint: e.currentTarget.value })} placeholder="peer.example:51820" />
								</Field>
								<Field label="Tunnel address">
									<input class="rule-input" autocomplete="off" value={wg.address || ""} onInput={(e) => setWG({ address: e.currentTarget.value })} placeholder="10.64.0.2" />
								</Field>
								<Field label="Tunnel DNS">
									<input class="rule-input" autocomplete="off" value={wg.dns || ""} onInput={(e) => setWG({ dns: e.currentTarget.value })} placeholder="10.64.0.1 (or 10.64.0.1:5353)" />
								</Field>
								<Field label="MTU">
									<input class="rule-input" type="number" value={wg.mtu || ""} onInput={(e) => setWG({ mtu: e.currentTarget.value })} placeholder="1420 (optional)" />
								</Field>
								<div class="nf-note">
									Egresses this network through an in-process userspace WireGuard
									tunnel. Its Noise handshake authenticates without a cleartext proxy
									login, and target DNS resolves through the tunnel.
								</div>
							</>
						)}
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
