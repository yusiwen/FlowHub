import { defineConfig } from "vite";

// The shell is served by Vite in development and bundled into the app for a
// release build. `strictPort` keeps Tauri's devUrl from silently moving.
export default defineConfig({
  clearScreen: false,
  server: { port: 1420, strictPort: true },
  build: { target: "safari15", minify: "esbuild", sourcemap: false },
});
