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

The same checks run in `.github/workflows/desktop.yml` on an arm64 macOS runner
for `desktop/**` changes. The workflow is path-filtered, so it is not a required
check for a Go-only pull request.

## Layout

| Path | Responsibility |
| --- | --- |
| `src/` | shell UI, running only in the local webview |
| `src-tauri/src/main.rs` | window and webview wiring |
| `src-tauri/src/tabs.rs` | tab manager: one child webview per instance, stage bounds, activate/close, lazy restore |
| `src-tauri/src/session.rs` | the versioned `session.json`: open tab ids, active tab, sidebar visibility |
| `src-tauri/src/auth.rs` | dsh launch-token parsing, the token URL, and the authority-hashed cookie name |
| `src-tauri/src/health.rs` | reachability probes and the `ok` / `auth` / `http` / `unreachable` classes |
| `src-tauri/src/menu.rs` | macOS menu, accelerators, and the window-close lifecycle |
| `src-tauri/capabilities/shell.json` | IPC grant for the `shell` webview only |
| `src-tauri/tauri.conf.json` | Tauri configuration |

## Tabs

Clicking an instance row opens its page in a tab. Each tab is a separate native
child WebView labelled `tab-<uuid>`; switching tabs hides the inactive webview
instead of destroying it, so scroll and input state survive. The shell measures
the `#stage` rectangle and Rust applies it to every content webview. A content
webview matches no capability, so the page has no Tauri IPC.

The window's content view spans the full window, while WebKit insets the shell's
CSS viewport below the title bar. The shell therefore also reports
`window.innerHeight`; Rust adds `shell height - viewport height` to the stage's
`y` before positioning a child webview. Without that inset the page starts 32
points too high and covers the bottom of the tab bar.

A tab label is the page title followed by `[<instance name>]`, or just
`[<instance name>]` before a title arrives. The tab is capped at 230 px and only
the title ellipsizes, so the instance suffix stays visible and two instances
with the same page title remain distinguishable. Hovering a tab shows the full,
untruncated page title, the instance name, and the URL.

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

## dsh sign-in

A `dsh-web` tab opens its base URL. If the `dsh-auth-<authority hash>` cookie is
missing, the page is hidden and the shell asks for the URL printed by `dsh web`
(or the bare token). The app builds `GET /?token=...`, waits for the `303` to
drop the token, and confirms the cookie is present. A rejected token keeps the
panel open with an error and lets the user retry.

The token is never written to `instances.json` or the logs; only the cookie is a
session. The cookie name hashes `host:port`, so cookies for two dsh ports on the
same host cannot be confused.

## Session restore

`session.json` (mode `0644`, written atomically) holds the open instance ids, the
active id and the sidebar visibility. On startup the tabs are rebuilt as
placeholders: the tab bar renders immediately, only the active tab's webview is
created, and every other webview is created on first activation. Ids whose
instance no longer exists are pruned; a corrupt or unknown-version file starts
with an empty session and is rewritten instead of blocking the app.

## Health

Every 30 s (and on demand from the UI) each instance gets one `GET` with a 3 s
timeout and redirects disabled. `200-399` is `ok`, `401`/`403` is `auth`
(reachable but sign-in required), any other HTTP status is `http`, and a
timeout or connection error is `unreachable`. The sidebar dot is green, yellow,
orange or red; its tooltip includes the latency. The probe runs in the Rust
process, so it is not subject to page CORS or a missing webview cookie.

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
