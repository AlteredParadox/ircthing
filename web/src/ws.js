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

export class Socket {
	constructor(url) {
		this.url = url;
		this.ws = null;
		this.seq = 0;
		this.pending = new Map(); // seq -> {resolve, reject, timer}
		this.handlers = new Map(); // type -> [fn]
		this.backoff = 1000;
		this.closed = false;
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

	// dispatch routes one decoded envelope. A seq-tagged envelope is a
	// request reply, never a push: settle it if still pending, otherwise it
	// arrived after the request timed out — drop it rather than emit() it,
	// where a late reply that shares a type with a push (e.g. get_prefs vs
	// the prefs push) would clobber unsynced local state. Seq-less
	// envelopes (seq 0/absent) are server pushes.
	dispatch(env) {
		if (env.v !== V) return;
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
		this.ws = new WebSocket(this.url);
		this.ws.onopen = () => {
			this.backoff = 1000;
			this.emit("_open");
		};
		this.ws.onmessage = (e) => {
			let env;
			try {
				env = JSON.parse(e.data);
			} catch {
				return;
			}
			this.dispatch(env);
		};
		this.ws.onclose = () => {
			this.emit("_close");
			for (const [, p] of this.pending) {
				clearTimeout(p.timer);
				p.reject(new Error("disconnected"));
			}
			this.pending.clear();
			if (this.closed) return;
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
			if (this.ws?.readyState !== WebSocket.OPEN) {
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
		if (this.ws?.readyState === WebSocket.OPEN) {
			this.ws.send(JSON.stringify({ v: V, type, data }));
		}
	}

	close() {
		this.closed = true;
		// Cancel any reconnect scheduled by a prior onclose — otherwise it
		// fires after close() and revives a zombie socket that double-
		// processes every push once the app reconnects.
		clearTimeout(this.reconnectTimer);
		this.ws?.close();
	}
}
