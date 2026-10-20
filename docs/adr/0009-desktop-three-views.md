# ADR 0009: Three views over one target registry

- Status: accepted
- Date: 2026-10-20
- Issues: #74 (epic), #75-#77 (implementation slices)

## Context

The desktop app models one role: downstream agent instances (dsh web, opencode).
FlowHub has three — upstream trackers (YouTrack), the agents it dispatches to, and
the hub itself — and the app can see only the middle one.

Two constraints shape the answer:

1. A native child webview always paints above the shell. A page the shell renders
   itself (the Hub dashboard) can only appear when every content webview is hidden,
   so it has to be a first-class tab state rather than an overlay.
2. The Hub has no page, only an API. `GET /healthz` on the main listener is the
   operational view; the control API is a separate, opt-in listener that needs an
   admin token and can invite, remove and rotate runtimes.

A third fact is a security boundary, not a convenience: `/healthz` reports the
service's `app_log` path. Reading a path a remote endpoint chose would turn "add a
Hub address" into an arbitrary-file-read primitive.

## Decision

**One registry, role derived from the kind.** `InstanceKind` gains `youtrack`
(upstream) and `flowhub` (the hub); the agent kinds stay downstream. `role()` maps
kind to role, so a row cannot claim one role and behave like another.
`instances.json` keeps version 1 and gains no field; existing rows are downstream by
construction. Three sidebar scopes — `Upstream`, `Instances`, `Hub` — filter the
same list, and each scope's Add form offers only its own kinds, so editing cannot
move a row across views.

**One global tab strip.** Tabs stay the only thing that describes the stage. A scope
switch activates that scope's last page and changes only the list when the scope has
none — blanking the stage would hide the page being read while the strip still named
it active. Activating a tab moves the scope to the tab's role. The switch is a
segmented control at the top of the sidebar (`Cmd+Shift+1/2/3`), not a rail: three
labels fit in the existing 300 px and the layout keeps two columns.

**The Hub is a target with native content.** A `flowhub` row has a real instance id,
so session restore, `Cmd+W`, `Cmd+1..9` and the tab order need no special case. Its
tab creates no webview; while it is active the tab manager hides every content
webview and focuses the shell. The dashboard is read-only over `/healthz`: version,
uptime, address, dispatch, runtimes, projects, queues, counters and paths, polled
every 5 s only while the tab is active, keeping the last snapshot and its age when a
poll fails. No admin token is stored and the control API is not called.

**Logs obey the address's trust domain.** A loopback Hub (`127.0.0.0/8`, `::1`,
`localhost`) runs on this machine as this user, so its `app_log` may be read. Any
other address may only use a path the user confirmed in the dashboard, persisted in
a small `prefs.json` keyed by instance id; the reported path is displayed, never
opened. The reader tails the last 256 KiB, reports truncation, and never writes or
follows.

**YouTrack stores no credential.** The tracker signs in inside its tab; the session
cookie lives in the shared webview store like any other login. The health probe only
claims "reachable", never "signed in", until an authenticated probe is designed.

## Consequences

- The three views share one registry file, one reorder operation and one health map;
  a scope is a filter, not a store.
- A downgrade to a build without `youtrack`/`flowhub` refuses the registry at load
  (serde rejects the unknown kind). At 0.1 that is acceptable; a shipped app would
  need a forward-compatible read.
- The Hub dashboard depends on `/healthz` being reachable. nginx never proxies it, so
  a remote hub must expose the main listener (or go through a tunnel); the control
  API's `/control/v1/health` is too thin to substitute.
- The log path preference is local to this machine and this instance id; moving the
  app to another machine means confirming the path again. That is intended — a path
  is only trusted after a human chose it.
- The tab manager recognizes native content by kind, so adding another native page
  (settings, an inventory view) is a kind plus a shell renderer, not a new window.

## Alternatives considered

- **Separate `upstreams.json` / `hubs.json` stores.** Rejected for now: they duplicate
  CRUD, ordering, validation and events, and the difference is a filter. The tab
  manager consumes a page target (`id`, `name`, `url`, `auth`), so a later split does
  not touch it.
- **Per-view tab strips.** Rejected: the app's value is jumping between a tracker page
  and an agent page; hiding one set behind a mode costs a second click and loses
  sight of what is open.
- **An activity rail.** Rejected: 48 px of permanent width and icon-only labels for
  three items, when the sidebar already has room.
- **The control API for the Hub view.** Rejected in v1: it is off by default, needs a
  secret the app would have to store, and its actions are dangerous. The dashboard
  only reads.
- **A Hub overlay/mode instead of a tab.** Rejected: the tab strip would claim a page
  is active while the stage showed the dashboard, and every tab action would need an
  exception.
- **Reading `app_log` from any Hub address.** Rejected: a configured URL must not be
  able to make the app read and display a local file.
