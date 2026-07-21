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

// WebSocket client for the versioned envelope protocol: request/response
// matching by seq, server pushes by type, automatic reconnect with
// backoff. Unknown envelope types are ignored (protocol rule).

const V = 1;

// A connection only counts as "stable" — resetting reconnect backoff and
// emitting _stable — once it has stayed open this long or delivered its
// first server frame, whichever comes first. Resetting on the bare open
// handshake let an accept-then-immediate-drop loop (reverse proxy with a
// tiny read timeout, crash-looping backend) reconnect at ~1-1.5s forever.
const STABLE_MS = 5000;

export class Socket {
	constructor(url) {
		this.url = url;
		this.ws = null;
		this.seq = 0;
		this.pending = new Map(); // seq -> {resolve, reject, timer}
		this.handlers = new Map(); // type -> [fn]
		this.backoff = 1000;
		this.closed = false;
		this.stable = false; // current connection survived STABLE_MS or got a frame
	}

	// markStable: the current connection is trustworthy — reset reconnect
	// backoff to base and tell the app (which gates its own failure counter
	// on it). Idempotent per connection; onclose re-arms it.
	markStable() {
		if (this.stable || this.closed) return;
		this.stable = true;
		clearTimeout(this.stableTimer);
		this.backoff = 1000;
		this.emit("_stable");
	}

	on(type, fn) {
		if (!this.handlers.has(type)) this.handlers.set(type, []);
		this.handlers.get(type).push(fn);
	}

	off(type, fn) {
		const arr = this.handlers.get(type);
		const i = arr ? arr.indexOf(fn) : -1;
		if (i !== -1) arr.splice(i, 1);
	}

	emit(type, data) {
		for (const fn of this.handlers.get(type) || []) fn(data);
	}

	rejectPending(message = "disconnected") {
		for (const [, p] of this.pending) {
			clearTimeout(p.timer);
			p.reject(new Error(message));
		}
		this.pending.clear();
	}

	// dispatch routes one decoded envelope. A seq-tagged envelope is a
	// request reply, never a push: settle it if still pending, otherwise it
	// arrived after the request timed out — drop it rather than emit() it,
	// where a late reply that shares a type with a push (e.g. get_prefs vs
	// the prefs push) would clobber unsynced local state. Seq-less
	// envelopes (seq 0/absent) are server pushes.
	dispatch(env) {
		// close() is synchronous from the app's point of view. A browser may
		// still deliver frames queued before the closing handshake completes;
		// they belong to the retired auth/socket generation and must be inert.
		if (this.closed) return;
		// A well-formed, version-matching envelope is the "delivered frame"
		// proof of a real backend. It is deliberately signalled HERE, not in
		// onmessage: a garbage frame (proxy error page, wrong upstream) must
		// not reset reconnect backoff — and JSON that parses but isn't our
		// envelope shape (null, a bare {v:1}, a nonsense seq) is still
		// garbage. A quiet-but-working link stabilizes via the STABLE_MS
		// timer instead.
		if (!env || typeof env !== "object" || env.v !== V) return;
		if (typeof env.type !== "string" || !env.type) return;
		if (env.seq != null && env.seq !== 0 &&
			!(Number.isSafeInteger(env.seq) && env.seq > 0)) return;
		this.markStable();
		if (env.seq) {
			const p = this.pending.get(env.seq);
			if (!p) return;
			this.pending.delete(env.seq);
			clearTimeout(p.timer);
			if (env.type === "error") p.reject(Object.assign(new Error(env.data?.message || "error"), { code: env.data?.code }));
			else p.resolve(env.data);
			return;
		}
		this.emit(env.type, env.data);
	}

	connect() {
		if (this.closed) return; // don't revive a socket closed during backoff
		this.stable = false; // each transport must re-prove itself
		const ws = new WebSocket(this.url);
		this.ws = ws;
		ws.onopen = () => {
			if (this.closed || this.ws !== ws) return;
			// Backoff does NOT reset here — only once the connection proves
			// stable (see STABLE_MS / markStable), so a handshake-succeeds-
			// then-dies loop keeps doubling toward the cap instead of
			// hammering the server and wiping scrollback every ~1s.
			clearTimeout(this.stableTimer);
			this.stableTimer = setTimeout(() => this.markStable(), STABLE_MS);
			this.emit("_open");
		};
		ws.onmessage = (e) => {
			if (this.closed || this.ws !== ws) return;
			// Stability (backoff reset) is signalled by dispatch() once the
			// frame parses as a version-matching envelope — an unparseable
			// frame is not proof of a live backend.
			let env;
			try {
				env = JSON.parse(e.data);
			} catch {
				return;
			}
			this.dispatch(env);
		};
		ws.onclose = () => {
			// connect() can replace a transport before an older browser close
			// callback is delivered. That retired callback must not reject the new
			// transport's requests or schedule a second reconnect loop.
			if (this.ws !== ws) return;
			clearTimeout(this.stableTimer);
			this.stable = false;
			this.ws = null;
			this.rejectPending();
			if (this.closed) return;
			this.emit("_close");
			// Jitter from crypto — quality is irrelevant for reconnect
			// spacing, but it keeps security linters quiet without a config
			// exception.
			const wait = this.backoff + (crypto.getRandomValues(new Uint32Array(1))[0] % 500);
			this.backoff = Math.min(this.backoff * 2, 15000);
			this.reconnectTimer = setTimeout(() => this.connect(), wait);
		};
	}

	request(type, data, timeoutMs = 10000) {
		return new Promise((resolve, reject) => {
			if (this.closed || this.ws?.readyState !== WebSocket.OPEN) {
				reject(new Error("not connected"));
				return;
			}
			const seq = ++this.seq;
			const timer = setTimeout(() => {
				this.pending.delete(seq);
				reject(new Error("timeout"));
			}, timeoutMs);
			this.pending.set(seq, { resolve, reject, timer });
			this.ws.send(JSON.stringify({ v: V, type, seq, data }));
		});
	}

	// notify sends a fire-and-forget envelope (no seq, no response).
	// Silently dropped while disconnected — fine for ephemeral state
	// like typing.
	notify(type, data) {
		if (!this.closed && this.ws?.readyState === WebSocket.OPEN) {
			this.ws.send(JSON.stringify({ v: V, type, data }));
		}
	}

	close() {
		if (this.closed) return;
		this.closed = true;
		// Cancel any reconnect scheduled by a prior onclose — otherwise it
		// fires after close() and revives a zombie socket that double-
		// processes every push once the app reconnects.
		clearTimeout(this.reconnectTimer);
		clearTimeout(this.stableTimer);
		// Settle callers NOW. Waiting for the browser's asynchronous close event
		// leaves old request continuations alive across a rapid logout/re-login.
		this.rejectPending();
		this.ws?.close();
	}
}
