# Agent Hub (desktop)

A macOS-first Tauri app for opening agent UIs (dsh web, opencode, and other
pages) in one window: a sidebar registry on the left, one native WebView per
instance in a tab on the right.

The Go service and this app share a repository, not a build: the app lives in
`desktop/` and has its own Rust and Node toolchain. No cgo, no FFI.

## Prerequisites

- macOS with Xcode command line tools
- Rust stable (`rustup`)
- Node 24 and pnpm

## Commands

```bash
make desktop-install   # pnpm install
make desktop-dev       # Vite + Tauri dev window
make desktop-check     # tsc --noEmit, cargo fmt, clippy, cargo test
make desktop-build     # release bundle (after the packaging slice lands)
```

## Layout

| Path | Responsibility |
| --- | --- |
| `src/` | shell UI, running only in the local webview |
| `src-tauri/src/main.rs` | window and webview wiring |
| `src-tauri/capabilities/shell.json` | IPC grant for the `shell` webview only |
| `src-tauri/tauri.conf.json` | Tauri configuration |

## Registry

The sidebar is backed by `instances.json` in the app config directory
(`~/Library/Application Support/dev.flowhub.agenthub/` on macOS). The file is
versioned, written atomically with mode `0600`, and holds no secret: a dsh launch
token is never persisted, only exchanged for a cookie in the webview.

## Security invariants

- Only the `shell` webview is named in a capability; a remote instance page has
  no Tauri IPC.
- Content webviews load `http`/`https` only; navigation to `file:` or `data:` is
  refused in the tab manager.
- No token or password is written to `instances.json` or the logs.

See `docs/adr/0008-desktop-agent-hub.md` for the decision record.
