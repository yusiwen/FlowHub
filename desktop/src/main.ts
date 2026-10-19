import "./styles/base.css";

const app = document.querySelector<HTMLDivElement>("#app");
if (app === null) throw new Error("shell: missing #app");

app.innerHTML = `
  <main class="shell">
    <h1>Agent Hub</h1>
    <p class="subtitle">Desktop shell ready.</p>
  </main>
`;
