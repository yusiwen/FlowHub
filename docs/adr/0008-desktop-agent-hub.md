# ADR 0008: A Tauri desktop shell for agent instances

- Status: accepted
- Date: 2026-10-19
- Issues: #38 (epic), #39-#46 (implementation slices)

## Context

The agent UIs this project works with are spread across ports and hosts: dsh web
on 3080 and 3081, opencode on 4096, and opencode on nuc11 and nuc12. Browser tabs
mix them with unrelated work, dsh web needs a per-process launch token, and there
is no shared registry of which instance lives where.

Two facts force the design:

1. dsh web authenticates a browser with `GET /?token=<launch token>` and answers
   `303` with an HttpOnly cookie bound to the listener's `host:port`. The launch
   token is random per process; the cookie lasts 30 days and survives a dsh
   restart because its signing secret is durable.
2. Agent pages are arbitrary remote origins. They must never reach the desktop
   app's IPC layer, and their content must not be embedded in a cross-origin
   iframe that the app cannot control.

## Decision

Add a macOS-first desktop app under `desktop/`, built with Tauri 2 and Rust, with
its own Node/Vite frontend. It is a second, independent build in the same
repository: no cgo, no FFI, no coupling to the Go service.

**One window, many webviews.** A plain Tauri `Window` hosts:

- one local `shell` webview (sidebar, tab bar, empty states), and
- one remote child webview per instance, created with
  `WebviewBuilder::new(label, WebviewUrl::External(url))` and attached with
  `Window::add_child`.

The tab manager hides inactive webviews instead of destroying them, so scroll and
input state survive a tab switch. The shell measures the `#stage` rectangle and
sends it to Rust; Rust applies it to every content webview.

**IPC isolation.** `src-tauri/capabilities/shell.json` names only
`webviews: ["shell"]`. It deliberately does not use `windows: ["main"]`, because
that would grant the capability to every webview in the window, including remote
pages. The `remote` field stays unset. A remote page therefore has no
`invoke`, no `window.__TAURI_INTERNALS__`, and no Tauri command.

**Authentication.** A dsh instance stores only its base URL. On a 401 (detected
by the missing `dsh-auth-` cookie and by the failed token redirect) the shell
asks for the URL printed by `dsh web`, builds `origin + "/?token=..."` with
exactly one token on the root path, navigates, and confirms success when the
`303` drops the token parameter. The token is never persisted; the cookie is the
session.

**Health.** The sidebar dot comes from a reachability probe in the Rust process,
not from the page: one `GET` per instance every 30 s, `200-399` = `ok`,
`401/403` = `auth`, any other status = `http`, a transport error = `unreachable`.
dsh rejects every credential-less request, so a loaded `dsh-web` tab lends the
probe its `dsh-auth-` cookie — read at probe time, never persisted, logged, or
exposed to the shell — and the report says only whether a credential was used.
Without a loaded tab there is no cookie to send, so the dot stays `auth` instead
of guessing `ok` (issue #65). A `401` for a probe that *did* carry the cookie has
proved the session dead, so the tab is put back into sign-in — page hidden, token
panel reopened — rather than left as a page that only answers `401`. The same
probe runs once when a tab's webview is created, so a dead session never reaches
the screen; activating such a tab asks again, so a cancelled panel is not a dead
end.

**State.** `instances.json` (mode `0600`) holds the registry; `session.json`
(mode `0644`) holds the open tabs and sidebar state. Both live under the app
config directory and are written atomically. No secret is written to either.

**macOS first.** Native menu items own the accelerators (`Cmd+T`, `Cmd+W`,
`Cmd+R`, `Cmd+B`, `Cmd+1..9`) because a focused content webview swallows
keyboard events. The Edit menu is present so clipboard shortcuts work inside a
webview. Other platforms are out of scope for now.

## Consequences

- The repository gains a Rust and Node toolchain, a macOS-only CI job, and a
  `desktop/` directory that the Go module ignores entirely.
- `tauri` is pinned to an exact minor version because the multi-webview API is
  behind the `unstable` Cargo feature. A Tauri upgrade is a deliberate change
  with a manual tab/layout regression pass.
- Remote pages cannot call the app. That is the security boundary; do not add a
  capability for `tab-*` labels, and do not set `remote`.
- A native child webview always paints above the shell webview. Any UI that must
  cover the page (login panel, confirmation) either hides the page first or is a
  separate window. The tab bar and sidebar live outside the page rectangle.
- The stage rectangle is in window content-view coordinates, not the shell's CSS
  viewport: WebKit insets the shell below the title bar, so Rust adds the
  difference between the shell height and `window.innerHeight` to the page's
  top. Removing that offset makes each tab cover the tab bar.
- The app is not part of the Go binary or its release artifacts. The only shared
  contract is the repository and the documentation.
- M2 polish (downloads, popups, tab hibernation) is tracked as follow-up issues
  after the usable M1 app lands. Packaging landed as #83: a build writes
  `FlowHub.app`, whose `Info.plist` is the application record macOS resolves the
  name and the icon from — a bare executable gets neither.

## Alternatives considered

- **One webview with iframes.** Rejected: cross-origin framing depends on the
  remote site's `frame-ancestors` policy, the app cannot set request headers or
  credentials per frame, and focus/keyboard behavior is worse.
- **Electron.** Rejected: a much larger runtime, and the app only needs a
  system webview and a few native menu items.
- **SwiftUI + WKWebView.** Viable for macOS only, but the requested stack is
  Rust/Tauri and the multi-webview management is the same work either way.
- **A separate repository now.** Rejected: this app shares the agent-instance
  domain and will later read FlowHub's runtime inventory; a monorepo keeps the
  ADR and the issue trail together. It can be extracted if it grows into a
  general browser.
