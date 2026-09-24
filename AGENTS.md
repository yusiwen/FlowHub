# FlowHub — DevOps/agent orchestration hub in Go

## Language Policy
All code comments and documentation must be written in **English**, unless explicitly asked to use Chinese.

## What this project is

FlowHub turns DevOps events into work for a headless `opencode` instance. The
first implemented source is YouTrack; Gitea and Drone come later.

**Current state: the YouTrack receiver and the opencode dispatcher are both
implemented.** The receiver records and audits every delivery; the dispatcher is
opt-in (`FLOWHUB_DISPATCH=1`) and turns an accepted delivery into one opencode
turn inside a per-task git worktree.

The three documents in the repository root are the specification — read the
relevant section before changing behaviour:

| Document | Covers |
| --- | --- |
| `youtrack-webhook-and-flowhub-security.md` | YouTrack app behaviour, real payload quirks, layered security (§8, §9, §12) |
| `opencode-devops-orchestration-design.md` | Overall architecture, session ownership, event-driven model |
| `opencode-headless-automation-and-permissions.md` | opencode HTTP API and the permission loop the dispatcher drives |
| `docs/adr/` | Decisions taken after the design documents, with their context and consequences. Read the relevant ADR before restructuring a seam it covers |

`CODEBASE.md` is the file-level map; keep it in step with structural changes.

## Quick Start

```bash
make build          # build to bin/flowhub
make secrets        # print a fresh FLOWHUB_HOOK_KEY / FLOWHUB_TOKEN pair
make test           # unit tests
make test-race      # race detector (needs cgo; the Nix shell leaves it on)
make lint           # go vet
make smoke          # end-to-end: real process, real HTTP, asserts the logs
```

Run the receiver locally:

```bash
export FLOWHUB_HOOK_KEY=$(openssl rand -hex 32)
export FLOWHUB_TOKEN=$(openssl rand -hex 32)
export FLOWHUB_ALLOWED_SOURCES=127.0.0.1
make run
```

Turn the dispatcher on as well (`opencode serve` must already be listening; the
agent definition lives in the opencode configuration, not here — on this host
`~/.config/opencode/agents/devops.md`):

```bash
FLOWHUB_DISPATCH=1 make run
```

A new agent file is only picked up by a *new* directory instance: opencode caches
the agent list per instance, so an already-used directory needs a server restart,
while the per-task worktree the dispatcher creates is always fresh.

Then post to `http://127.0.0.1:8080/hooks/youtrack/$FLOWHUB_HOOK_KEY` with the
header `X-YouTrack-Token: $FLOWHUB_TOKEN`. See `README.md`.

## Toolchain

The Nix dev shell (`flake.nix`, activated by `.envrc` or `nix develop`) provides
Go, gopls, gofumpt, git and openssl. Outside Nix, this machine has a Go 1.27
toolchain at `/opt/go/bin/go`, which is **not on the default PATH**.

Note for agents: `nix` on this host lives at `/nix/var/nix/profiles/default/bin`
and is not on the sandbox PATH. Redirect the XDG directories and pass the
experimental-features flag when invoking it:

```bash
export PATH=/nix/var/nix/profiles/default/bin:$PATH
export XDG_CONFIG_HOME=/tmp/dsh-nix-xdg/config XDG_DATA_HOME=/tmp/dsh-nix-xdg/data \
       XDG_STATE_HOME=/tmp/dsh-nix-xdg/state XDG_CACHE_HOME=/tmp/dsh-nix-xdg/cache \
       NIX_PROFILES=/tmp/dsh-nix-xdg/profiles
nix --extra-experimental-features 'nix-command flakes' develop --command bash -c 'go test ./...'
```

Nix flakes in a git repository only see files in the git index. New files must be
`git add`-ed before `nix develop` / `nix flake check` can see them.

## Project Structure

```
cmd/flowhub/        CLI entry: wiring, flags, HTTP server, /healthz, shutdown
internal/config/    Environment parsing, validation, masked reporting
internal/logging/   Application logger: stderr tee + size-rotated log file
internal/webhook/   Lenient payload model, payload schema report, delivery pipeline
internal/projectmap/ YouTrack project -> repository routing table (~/.config/flowhub/config.json)
internal/opencode/  opencode client, permission arbiter, one-turn runner (make test-live)
internal/rules/     trigger policy (ignore/analyze/plan/execute) and the per-turn prompt
internal/dispatch/  the worker: queue, routing, worktree, session, arbiter, registry
internal/provision/ data-plane host prep: `flowhub runtime init --check` (capability report, writes nothing)
internal/dedupe/    TTL idempotency cache
internal/store/     Audit record, non-blocking queue, JSONL audit, payload log
internal/metrics/   Counters behind /healthz
scripts/smoke.sh    End-to-end check driven by `make smoke`
```

## The delivery pipeline

```
POST /hooks/youtrack/<url key>
  -> lock 1 URL key -> lock 2 header token -> lock 3 source IP
  -> body cap -> content type -> JSON -> known event -> replay window
  -> idempotency -> audit queue (background) -> 202, empty body
```

Everything after the entry locks is derived from measurements in the design
documents. The rules below are load-bearing; do not relax them casually.

1. **Never block the request path.** The published Webhook Triggers app posts
   synchronously with a 5s timeout and no retry, so a slow handler is felt by the
   person editing the issue. No I/O on this path except reading the body; audit
   writes go through `store.Async` and are dropped (with a counter) if the queue
   is full.
2. **Every rejection looks like success.** `202` with an empty body and no
   explanatory header, so a scanner learns nothing. The reason goes to the audit
   log and `/healthz` counters only. The two deliberate exceptions are `405` for
   a wrong method and `413` for an oversized body.
3. **Secrets are never written to disk.** The token header, `Authorization`,
   `Proxy-Authorization`, `Cookie` and `Set-Cookie` values are replaced by
   `<masked len=… sha256:…>`. The URL key is redacted **by position** — the
   segment after the hook base in a path, the value of `k` in a query — never by
   matching the configured value. Matching the configured value leaked: the
   deliveries that reach the audit log are the ones whose key did not match, so a
   receiver restarted with a fresh random key (the documented
   `openssl rand` recipe) wrote the app's live key to disk. Measured 2026-09-21,
   fixed the same day; the redaction runs even when lock 1 is disabled. The
   service answers the "is the app sending the literal `secret`?" question
   through `MatchesConfigured` / `LooksLikeLiteralSecret` fingerprints instead.
4. **Parsing is lenient.** Nothing but `event` is required, unknown fields are
   ignored, and `changedFields[].value/oldValue` stay `json.RawMessage`: the
   released app, its `main` branch and the official docs disagree.
5. **The request path never waits for opencode.** The dispatcher runs on its own
   goroutine and is handed a delivery only after the `202` is on the wire, and
   only when the delivery was accepted. Adding an outbound call to this path
   would violate both the latency budget and the security model.
6. **A wildcard listen is refused unless acknowledged.** `0.0.0.0:8080`, `:8080`
   and `[::]:8080` are the same bind to the kernel; `config.Problems()` blocks
   startup for them unless `FLOWHUB_ALLOW_WILDCARD_LISTEN=1` is set, which exists
   for container images (where the wildcard is the only option, and isolation
   comes from publishing the port to loopback). Do not "fix" a refusal by
   relaxing the check: either bind a specific address or set the flag for a real
   container. `Problems()` is deliberately separate from `validate()` so
   `-print-config` keeps working while a start is refused.
7. **Routing never guesses a repository.** The mapping from `project.key` to a
   local checkout lives in `~/.config/flowhub/config.json` (template:
   `config/config.example.json`); `FLOWHUB_PROJECTS_FILE` overrides it and a
   leading `~` is expanded. An unmapped project is refused, and a project key
   that *is* present but unmapped must **not** fall back to the issue-ID
   prefix. Startup validates every path, git work tree and `origin` before any
   file is created, and refuses otherwise. Do not add a "use the only configured
   repository" fallback: silently editing the wrong repository is the worst
   failure this project can have. Startup also refuses to dispatch when an entry
   has no `default_branch` or no `worktrees` directory and no
   `FLOWHUB_WORKTREE_BASE` fallback, because such an entry can only fail one
   issue at a time.
8. **An unattended turn needs two permission layers.** `internal/opencode` passes
   a session ruleset at session creation — a short allowlist over a catch-all
   `ask`, which must come **first** because the ruleset is an array evaluated
   last-match-wins — and then answers the `ask` requests with the arbiter. `edit`
   is `ask`, never `allow`, so the phase decides. The arbiter is an allowlist over
   shell segments, and it must keep rejecting absolute paths and `~`: the
   ruleset's `external_directory` does not cover a command run inside a shell.
   Never reply `always` by default, never widen the allowlist to "make the agent
   work", and remember that allowed build and test commands are arbitrary code
   execution — the per-task worktree is the real containment. Completion detection
   needs all three signals (new completed assistant message, status not busy, no
   pending permission): status lags, and a pending permission keeps a session busy.
9. **One task, one worktree, one session.** `internal/registry` is the
   append-only record of that binding (`<DataDir>/registry.jsonl`) and
   `internal/worktree` creates the checkout. The dispatcher refuses to reuse a
   session when the routing table now points at a different repository than the
   task was created against, and it never falls back to the shared checkout.
   Only one worker runs turns: a prompt sent to a busy session is silently
   swallowed, so more workers need per-task locking first.
10. **The agent's reply must never start another turn.** Our own comments are
    recognised by content, because the agent posts as the same YouTrack user as
    the human. Three independent layers: the `<!-- flowhub-auto -->` marker in the
    reply, a probe against the task's recorded last reply (for when the marker is
    lost), and an anchored trigger (`^\s*/opencode start\b`) so a reply that
    merely *mentions* the trigger cannot fire one. Do not loosen the anchoring,
    and do not add a natural-language trigger. The reply must also end with a
    blockquote that says opencode generated it and **what triggered the turn**,
    because a reader has to be able to tell an automated reply from a human one
    and judge whether the turn answered the right question. That basis comes from
    `Policy.Basis(decision, delivery)` — never from the model, which will invent a
    plausible reason when asked to explain itself. The marker stays the last line,
    inside the blockquote.
11. **A subcommand runs before the receiver configuration is loaded.**
    `flowhub runtime init` executes on a machine that has none of the receiver's
    environment variables, so `main` dispatches a subcommand before `config.Load`
    and does not print the receiver's banner or load the routing table. Its exit
    status is a gate: 0 ready, 2 not ready, 1 usage. A capability report never
    carries a secret value — only whether a variable is present — because it is a
    document that gets copied into tickets; and a check that could not run (a
    permission failure) must never be reported as a negative finding.
12. **Dispatch must be stoppable without a restart.** `FLOWHUB_PAUSE_FILE`
    (default `<DataDir>/DISPATCH_OFF`) is checked before every delivery: while it
    exists, deliveries are audited and ignored. A kill switch that needs an API, a
    credential or a restart is not a kill switch.

## Code Conventions

- **Language**: Go, standard library only — no third-party dependencies so far.
  Keep it that way unless there is a strong reason.
- **Imports**: stdlib first, then internal packages, grouped by a blank line.
- **Error handling**: `fmt.Errorf("context: %w", err)`, lowercase messages.
- **Comments**: English, explaining *why* (the constraint or measurement that
  forced the decision), not restating the code.
- **Logging**: `log/slog` with structured attributes; never log a secret or a
  full URL containing the key.
- **Testing**: `_test.go` next to the source; table-driven where it helps. New
  pipeline stages need tests for the accept path *and* each rejection path, and
  tests that assert secrets do not leak into records.
- **Rejection reasons** are stable identifiers: adding one means adding it to
  `webhook.AllReasons`, documenting it in `README.md`, and keeping `smoke.sh` in
  step.

## Key Design Decisions

1. **Method-less mux patterns** — a wrong method must reach the handler so it is
   audited as `method_not_allowed` instead of being swallowed by `ServeMux`.
2. **Two log sinks, one queue** — `store.NewMulti` writes each delivery to the
   machine-readable JSONL audit and to the human-readable payload log on the same
   goroutine, so ordering matches and the request path stays I/O free.
3. **`webhook.Schema` is the payload-analysis feature** — a flat sorted
   `path: type` report with array elements merged, which is how the real payload
   is compared against the documented one (§5.2/§5.4 of the security document).
4. **Idempotency is local** — the app sends no delivery id and never retries, so
   the key is `sha256(event, issue id, timestamp, body)` held for a TTL.
5. **JSONL audit over SQLite** — dependency-free, greppable, replayable, and the
   first goal is observing real traffic. A SQLite store can replace it behind the
   same `store.Recorder` interface. The task registry made the same choice for the
   same reason.
6. **The dispatcher is opt-in and fail-closed.** `FLOWHUB_DISPATCH` defaults to
   false, and turning it on adds startup checks instead of removing them: the
   opencode health probe, the routing-table checks above, and a loud warning when
   the pause file is already present. Anything that would let the agent run
   unattended in a repository nobody chose is a bug.

## Before you finish a change

```bash
make fmt-check && make lint && make test && make smoke
```

Report the exact commands you ran and their results. If a check could not be run
(for example a missing toolchain), say so explicitly instead of implying success.
