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

import { useState } from "preact/hooks";

export function Login({ onLogin }) {
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);

	async function submit(e) {
		e.preventDefault();
		setBusy(true);
		setError("");
		try {
			const resp = await fetch("/api/login", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ username, password }),
			});
			if (resp.status === 204) {
				onLogin();
				return;
			}
			setError(resp.status === 401 ? "invalid credentials" : `login failed (${resp.status})`);
		} catch {
			setError("server unreachable");
		} finally {
			setBusy(false);
		}
	}

	return (
		<div class="login-wrap">
			<form class="login-card" onSubmit={submit}>
				<div class="login-head">
					<div class="logo">λ</div>
					<div class="login-title">ircthing</div>
				</div>
				<label class="login-label" htmlFor="login-user">username</label>
				<input
					id="login-user" class="login-input" autocomplete="username" autofocus
					value={username} onInput={(e) => setUsername(e.currentTarget.value)}
				/>
				<label class="login-label" htmlFor="login-pass">password</label>
				<input
					id="login-pass" class="login-input" type="password" autocomplete="current-password"
					value={password} onInput={(e) => setPassword(e.currentTarget.value)}
				/>
				{error && <div class="login-error">{error}</div>}
				<button type="submit" class="btn-accent login-btn" disabled={busy}>
					{busy ? "signing in…" : "sign in"}
				</button>
			</form>
		</div>
	);
}
