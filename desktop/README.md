# FlowHub (desktop)

A macOS-first Tauri app for opening the pages FlowHub works with in one window: a
sidebar registry split into three view scopes (upstream trackers, agent instances,
and the FlowHub service itself), one tab per page on the right.

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
| `src-tauri/src/hub.rs` | read-only `/healthz` client and the log tail behind the Hub dashboard |
| `src-tauri/src/prefs.rs` | local preferences: the confirmed log path per Hub |
| `src-tauri/src/menu.rs` | macOS menu, accelerators, and the window-close lifecycle |
| `src-tauri/capabilities/shell.json` | IPC grant for the `shell` webview only |
| `src-tauri/icons/icon.svg` | the icon master, and the generated PNG/ICNS/ICO set next to it |
| `scripts/build-icons.sh` | regenerates that set from the master |
| `src-tauri/tauri.conf.json` | Tauri configuration |

## Views

The sidebar has three scopes, switched at its top or with `Cmd+Shift+1/2/3`:
`Upstream` for tracker pages, `Instances` for agent pages, and `Hub` for the FlowHub
services the app reads. A scope is a filter over one registry — it does not own a
second store.

The tab strip stays global. Switching a scope activates the page that scope was last
on and changes only the list when it has none: replacing a page the user is reading
with an empty stage would also make the tab strip name a tab that is not on screen.
Activating a tab moves the selector to that tab's role, so the list beside the stage
is always the one that owns the visible page.

The scope's header shows the row count and its Add button. The form opened from a
scope offers only that scope's kinds, so editing cannot move a row between views.

## Upstream

YouTrack is the first upstream kind. A row stores the tracker's base URL and nothing
else: the tracker signs in inside its tab, and the resulting cookie lives in the
shared webview store like any other login. The token is never written to the
registry. The health probe is the credential-less one, so a green dot means
"reachable" — a login page answers `200` too — not "signed in".

## Hub

A Hub row is the base address of a FlowHub service. Its tab is native shell
content, not a webview, because the service has no page. The dashboard reads
`<base>/healthz` (a full `.../healthz` also works) every 5 s while the tab is
active and shows the version, uptime, address, dispatch queue and per-runtime
state, the runtime inventory, project mappings, counters and the three log paths.
A failed read keeps the last snapshot and shows its age; a `200` that is not the
FlowHub payload is reported as "not a FlowHub service", never as connected.

While a Hub tab is active every content webview is hidden: a native child webview
always paints above the shell, so the dashboard would otherwise be covered.
Switching back shows the page again without a reload.

The dashboard is read-only and unauthenticated. It does not call the control API
and stores no admin token. `/healthz` lives on the webhook listener, which nginx
never proxies, so a remote hub must expose that listener (or go through a tunnel).

The Logs card reads the service's `app_log`. A loopback Hub (`127.0.0.0/8`, `::1`,
`localhost`) is trusted to name its own file because it runs on this machine as
this user; any other address is only ever *shown* the reported path and needs a
path the user confirms in the dashboard. That choice is stored per instance in
`prefs.json` (`0644`, no secret). The reader returns the last 256 KiB / 200 lines,
drops a partial first line, and reports truncation; it never writes or follows.
Clearing the path falls back to the loopback rule.

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

A tab carries the role of its instance, which is what lets the shell move the sidebar
selector when the tab is activated. A Hub tab has no webview: it is loaded as soon as
the tab exists, and the shell renders the dashboard in the stage instead. A tab label is the page title followed by
`[<instance name>]`, or just
`[<instance name>]` before a title arrives. The tab is capped at 230 px and only
the title ellipsizes, so the instance suffix stays visible and two instances
with the same page title remain distinguishable. Hovering a tab shows the full,
untruncated page title, the instance name, and the URL.

## Shortcuts

The macOS menu owns the accelerators because a focused content webview receives
keyboard events before the shell:

- `Cmd+T` open the first row of the current view without a tab (or cycle that view's tabs)
- `Cmd+Shift+1/2/3` switch to Upstream / Instances / Hub
- `Cmd+W` close the active tab; with no tabs, close the window
- `Cmd+R` reload the active tab, or refetch the Hub dashboard
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
process, so it is not subject to page CORS.

dsh answers `401` to every request that does not carry its
`dsh-auth-<authority hash>` cookie, so a credential-less probe would report
"sign-in required" for a tab that is signed in and working. A loaded `dsh-web`
tab therefore lends its cookie to the probe: the value is read from the tab's
webview at probe time, used for that one request, and never logged, persisted,
or returned to the shell. The report records only whether a credential was used
(`none` or `cookie`), which is what lets the tooltip separate the yellow states:

- `cookie` + `200` — green, "reachable, signed in".
- `cookie` + `401` — yellow, "session expired". The server rejected the cookie
  the tab still holds, so the tab goes back to sign-in: the page is hidden and
  the token panel reopens. That happens as soon as the verdict arrives for a
  loaded tab, when the tab's webview is created (it is verified with its own
  cookie before its page counts as signed in), and on activation — which also
  makes cancelling the panel recoverable, since activating a tab that waits for
  a token asks again.
- `none` + `401` — yellow, "sign-in required". No webview has been loaded for
  that instance in this app run, so there is nothing to send; opening the tab
  runs the sign-in panel.
- No `dsh-web` tab loaded at all — the dot stays yellow and never claims `ok`
  without evidence.

YouTrack and `opencode`/`other` instances keep the credential-less probe; `200` is
green. A YouTrack login page also answers `200`/`302`, so the dot means reachable,
not signed in. A `flowhub` row probes `<base>/healthz`; `200` is green.

## Registry

The sidebar is backed by `instances.json` in the app config directory
(`~/Library/Application Support/cn.yusiwen.flowhub/` on macOS, named after the
bundle identifier). The file is versioned, written atomically with mode `0600`,
and holds no secret: a dsh launch token is never persisted, only exchanged for a
cookie in the webview. It holds all three roles; `kind` decides the view —
`youtrack` is upstream, `flowhub` is the hub, `dsh-web`/`opencode`/`other` are
downstream — and the registry version stays 1 because the role is derived rather
than stored.

While the app was called "Agent Hub" it used the `dev.flowhub.agenthub`
directory. On the first start after the rename, `instances.json` and
`session.json` are copied from there when the new directory has none; the legacy
directory is left untouched, so nothing is lost either way.

## App icon

The mark is one hub wired to three agents, with the live agent in mint. It is drawn
as a single vector master, `src-tauri/icons/icon.svg`: a 1024 px canvas, an 824 px
body with continuous (superellipse) corners, the macOS icon grid, and no baked-in
drop shadow outside the body.

Every raster is generated from that master:

```bash
bash scripts/build-icons.sh          # BROWSER=/path/to/chrome to override detection
```

The script renders each size natively in a headless Chromium-family browser
(transparent background, one launch per size) and then writes `icon.png` (1024),
`32x32.png`, `64x64.png`, `128x128.png`, `128x128@2x.png` (256), the ten-slot
`icon.icns` through `iconutil`, and `icon.ico` through ImageMagick when it is
installed. `sips` and `iconutil` ship with macOS; no other tool is required.

`bundle.icon` lists the generated files, which is what makes `pnpm tauri dev` set
the Dock tile from `icon.icns` — Tauri's codegen reads that list in development.
`bundle.active` stays `false` until the packaging slice lands.

## Security invariants

- Only the `shell` webview is named in a capability; a remote instance page has
  no Tauri IPC.
- Content webviews load `http`/`https` only; navigation to `file:` or `data:` is
  refused in the tab manager.
- No token or password is written to `instances.json` or the logs; a YouTrack
  sign-in is a webview cookie, not a registry field.
- The Hub dashboard is read-only: it calls `/healthz` and nothing else, and the app
  stores no control API token.
- The path a *remote* Hub reports for its log is never opened; only a path the user
  confirmed locally is read. A configured URL must not become an arbitrary-file-read
  primitive.

See `docs/adr/0008-desktop-agent-hub.md` for the decision record.
