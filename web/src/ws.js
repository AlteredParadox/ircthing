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

	emit(type, data) {
		for (const fn of this.handlers.get(type) || []) fn(data);
	}

	connect() {
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
			if (env.v !== V) return;
			if (env.seq && this.pending.has(env.seq)) {
				const p = this.pending.get(env.seq);
				this.pending.delete(env.seq);
				clearTimeout(p.timer);
				if (env.type === "error") p.reject(Object.assign(new Error(env.data?.message || "error"), { code: env.data?.code }));
				else p.resolve(env.data);
				return;
			}
			this.emit(env.type, env.data);
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
			setTimeout(() => this.connect(), wait);
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
		this.ws?.close();
	}
}
