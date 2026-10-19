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
| `src-tauri/src/tabs.rs` | tab manager: one child webview per instance, stage bounds, activate/close |
| `src-tauri/src/menu.rs` | macOS menu, accelerators, and the window-close lifecycle |
| `src-tauri/capabilities/shell.json` | IPC grant for the `shell` webview only |
| `src-tauri/tauri.conf.json` | Tauri configuration |

## Tabs

Clicking an instance row opens its page in a tab. Each tab is a separate native
child WebView labelled `tab-<uuid>`; switching tabs hides the inactive webview
instead of destroying it, so scroll and input state survive. The shell measures
the `#stage` rectangle and Rust applies it to every content webview. A content
webview matches no capability, so the page has no Tauri IPC.

## Shortcuts

The macOS menu owns the accelerators because a focused content webview receives
keyboard events before the shell:

- `Cmd+T` open the first instance without a tab (or cycle when every instance is open)
- `Cmd+W` close the active tab; with no tabs, close the window
- `Cmd+R` reload the active tab
- `Cmd+B` toggle the sidebar
- `Cmd+1..9` activate a tab by position; `Ctrl+Tab` / `Ctrl+Shift+Tab` cycle
- `Cmd+Shift+O` open the active page in the system browser
- `Cmd+Shift+W` close the window; the Edit menu provides the standard clipboard items

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
