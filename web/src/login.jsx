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
				<label class="login-label" for="login-user">username</label>
				<input
					id="login-user" class="login-input" autocomplete="username" autofocus
					value={username} onInput={(e) => setUsername(e.currentTarget.value)}
				/>
				<label class="login-label" for="login-pass">password</label>
				<input
					id="login-pass" class="login-input" type="password" autocomplete="current-password"
					value={password} onInput={(e) => setPassword(e.currentTarget.value)}
				/>
				{error && <div class="login-error">{error}</div>}
				<button class="btn-accent login-btn" disabled={busy}>
					{busy ? "signing in…" : "sign in"}
				</button>
			</form>
		</div>
	);
}
