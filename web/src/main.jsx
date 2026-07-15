import { render } from "preact";
import "./style.css";

function App() {
	return (
		<main class="placeholder">
			<h1>ircthing</h1>
			<p>Scaffolding placeholder — no client here yet.</p>
		</main>
	);
}

render(<App />, document.getElementById("app"));
