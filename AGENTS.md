# FlowHub — DevOps/agent orchestration hub in Go

## Language Policy
All code comments and documentation must be written in **English**, unless explicitly asked to use Chinese.

## What this project is

FlowHub turns DevOps events into work for a headless `opencode` instance. The
first implemented source is YouTrack; Gitea and Drone come later.

**Current state: phase 1 complete.** The YouTrack webhook receiver exists, is
tested, and only records deliveries. It does **not** call opencode yet, by design.

The three documents in the repository root are the specification — read the
relevant section before changing behaviour:

| Document | Covers |
| --- | --- |
| `youtrack-webhook-and-flowhub-security.md` | YouTrack app behaviour, real payload quirks, layered security (§8, §9, §12) |
| `opencode-devops-orchestration-design.md` | Overall architecture, session ownership, event-driven model |
| `opencode-headless-automation-and-permissions.md` | opencode HTTP API and the permission loop used in phase 2 |

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
   `<masked len=… sha256:…>`. The URL key is redacted from path and query. The
   service answers the "is the app sending the literal `secret`?" question
   through `MatchesConfigured` / `LooksLikeLiteralSecret` fingerprints instead.
4. **Parsing is lenient.** Nothing but `event` is required, unknown fields are
   ignored, and `changedFields[].value/oldValue` stay `json.RawMessage`: the
   released app, its `main` branch and the official docs disagree.
5. **Phase 1 does not talk to opencode.** Adding an outbound call to this path
   would violate both the latency budget and the phase-1 security model.
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
   failure this project can have. This layer only validates and reports today —
   dispatching to opencode comes next.
8. **An unattended turn needs two permission layers.** `internal/opencode` passes
   a session ruleset at session creation (deny `edit`, `external_directory`,
   `webfetch`, `websearch`; `bash: ask`) and then answers the `ask` requests with
   the arbiter. The arbiter is an allowlist over shell segments, and it must keep
   rejecting absolute paths and `~`: the ruleset's `external_directory` does not
   cover a command run inside a shell. Never reply `always` by default, never
   widen the allowlist to "make the agent work", and remember that allowed build
   and test commands are arbitrary code execution — the per-task worktree is the
   real containment. Completion detection needs all three signals (new completed
   assistant message, status not busy, no pending permission): status lags, and a
   pending permission keeps a session busy.

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
   phase-1 goal is observing real traffic. A SQLite store can replace it behind
   the same `store.Recorder` interface.

## Before you finish a change

```bash
make fmt-check && make lint && make test && make smoke
```

Report the exact commands you ran and their results. If a check could not be run
(for example a missing toolchain), say so explicitly instead of implying success.
