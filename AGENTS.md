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

A new agent file is picked up by a *new* directory instance, while the per-task
worktree the dispatcher creates is always fresh. That is not enough on its own:
the agent *registry* the server resolves an agent name against is built once per
server process, so a repaired profile keeps answering with its old settings — the
cached model included — until `opencode serve` restarts. FlowHub therefore pins the
model the runtime reported for that profile (ADR 0002), and `init` refuses a
profile whose model the provider no longer offers.

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
cmd/flowhub/        CLI entry: wiring, flags, HTTP server, /healthz, shutdown, activation prober
internal/config/    Environment parsing, validation, masked reporting
internal/logging/   Application logger: stderr tee + size-rotated log file
internal/event/     the neutral event IR (Kind, Subject, Attachment, Event) every source decodes into
internal/source/    the source seam: Request, Decoded, Source, ToolPolicy (data only)
internal/source/youtrack/ the YouTrack adapter: payload model, schema report, neutral-event decoding, tools, prompt
internal/webhook/   The delivery pipeline: entry locks, body limits, source decode, replay window, idempotency, audit, redaction
internal/projectmap/ the configuration file (v1/v2, sources, runtimes, (source, project) index, provenance) (~/.config/flowhub/config.json)
internal/agent/       the runtime seam: Runtime, Turn/Result, Phase, Downloads, model references
internal/agent/opencode/  the opencode implementation: client, arbiter, session ruleset, one-turn runner
internal/workspace/   the workspace seam: Workspace, Handle, Base, Validator
internal/workspace/localworktree/  one git worktree per task, baseline pinned through the origin
internal/rules/     trigger policy over the neutral event (ignore/analyze/plan/execute) and the reply basis
internal/dispatch/  the worker: intake loop, one task scheduler per runtime, routing, workspace hand-off, session, registry, audit
internal/provision/ data-plane host prep: `flowhub runtime init [--check]`, `doctor --push`, `uninstall` (capability report, embedded artifacts, manifest, runtime identity, heartbeat)
internal/runtimes/  control plane: runtime inventory, states, invites, secrets (hashes only), `/control/v1` admin API and the `invite`/`list`/`show`/`remove`/`rotate` CLI
internal/dedupe/    TTL idempotency cache
internal/store/     Audit record, non-blocking queue, JSONL audit, payload log
internal/metrics/   Counters behind /healthz
scripts/smoke.sh    End-to-end check driven by `make smoke`
```

The dispatcher imports `internal/agent` and `internal/workspace` but neither
implementation: `go list -deps ./internal/dispatch/` shows no
`internal/agent/opencode` and no `internal/workspace/localworktree` edge. Which
runtime and which provider exist is `main`'s decision, and `internal/dispatch`'s own
tests wire the real adapter against a fake server, which is what proves the seam
(ADR 0001 step 2).

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
   **An agent server's Basic Auth password follows the same rule (ADR 0004).** It
   comes from the environment (`FLOWHUB_OPENCODE_PASSWORD`, or
   `auth.password_env`), from a per-runtime `auth.password_file`, from the shared
   top-level `credentials_file`, or — only when the operator explicitly chooses it
   — inline as `auth.password`. A block names **exactly one** source: two is a
   load-time refusal, because the operator would otherwise rotate the one that is
   not in force. Precedence is per-runtime block, then shared file, then the
   process-wide environment pair, and the same `projectmap.ResolveCredential` is
   used by the runtime factory *and* the activation prober — two independent
   lookups is how a probe passes while the first turn fails. A file that holds a
   secret (`credentials_file`, `password_file`, or the configuration file when it
   carries an inline `password`) must be mode `0600`: group or other access refuses
   the start with the `chmod 600` fix in the message. Never print a password:
   `-print-config` names the source, and a test asserts the value stays out.
   **Every path that talks to an agent server resolves credentials by runtime name**,
   and that name comes from the declaration that owns the address
   (`defaultRuntimeNameFor`: the first declared block whose URL is
   `FLOWHUB_OPENCODE_URL`, else `default`) — the startup probe, the activation prober
   and the turn must present the same pair. Probing under the fallback name while the
   credentials live under a declared name is how a correct table was refused with a
   401 on 2026-10-08.
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
7. **Routing never guesses a repository.** The mapping from `(source, project)` to a
   local checkout lives in `~/.config/flowhub/config.json` (template:
   `config/config.example.json`); `FLOWHUB_CONFIG_FILE` overrides it
   (`FLOWHUB_PROJECTS_FILE` is the old name and still works) and a
   leading `~` is expanded. An unmapped project is refused, and the **router never
   derives a project from an issue ID**: splitting `TEST-12` on its last dash is a
   YouTrack convention, so the adapter that knows it fills the subject's project
   while the router stays free of conventions. The index is `(source, project)`, the
   file's version is explicit (a version 1 file is translated on read and reported
   as such), every unknown field is an error, and each effective value is printed
   with the level that supplied it. A `prompt_file` (source or project level) may only
   *add* instructions: the untrusted-input warning, the reply tool, the sign-off and
   the `<!-- flowhub-auto -->` marker are the adapter's contract and stay in code,
   because a text file that could delete the marker would break the loop prevention
   in rule 10. **The two levels reach the turn by different routes, and both must
   stay wired.** The source's file is read by the loader and handed to the adapter
   through `source.Factory(policy, instructions)`; the project's travels per turn in
   `rules.PromptContext.Instructions`. The source file was read and validated but
   never passed on until 2026-10-08 — a `prompt_file` that parses and then does
   nothing is exactly the silent no-op this project refuses, so a change to either
   route needs the prompt-level test that asserts the text appears. Startup validates
   the file, every path,
   git work tree and `origin` before any
   file is created, and refuses otherwise. Do not add a "use the only configured
   repository" fallback: silently editing the wrong repository is the worst
   failure this project can have. Startup also refuses to dispatch when an entry
   has no `default_branch` or no `worktrees` directory and no
   `FLOWHUB_WORKTREE_BASE` fallback, because such an entry can only fail one
   issue at a time.
8. **An unattended turn needs two permission layers.** `internal/agent/opencode` passes
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
   A turn that produces no assistant message at all while the session is idle is
   failed at `FLOWHUB_FIRST_RESPONSE` rather than held until the deadline — that is
   the signature of a prompt the agent server rejected, and a busy session with no
   message yet is not it. Permission replies are **one per round trip, re-listing in
   between**: opencode can hold two pending requests for one session and resolve one
   when the other is answered, so replying to a whole stale list got a 404
   (`PermissionNotFoundError`) and used to kill a turn that had already done the
   work. A 404 is treated as "already resolved" (skipped, never recorded as
   answered), and a request that is handled is never listed as pending again — a
   request the server keeps listing must not stop a finished turn from completing.
9. **One task, one worktree, one session.** `internal/registry` is the
   append-only record of that binding (`<DataDir>/registry.jsonl`), indexed by
   `(source, key)`: a task is identified by the adapter that produced it *and* its
   key, so two trackers may share a project key without sharing a session, and a row
   written before the source seam existed is read as `youtrack` (the field is written
   the next time the task is touched). `internal/workspace/localworktree` creates the
   checkout. The base commit is **pinned through
   the origin** once per task and recorded (`base_commit`), never re-resolved: two
   hosts whose clones were fetched at different times must start a project's tasks
   from the same commit. `Prepare` produces exactly that commit (fetching it only
   when the clone has never seen it) and attaches to an existing
   `flowhub/<task key>` branch only when it descends from the pin. A repository
   with no `origin` has no shared truth to pin: the local ref is used and the log
   says so. The dispatcher refuses to reuse a
   session when the routing table now points at a different repository than the
   task was created against, and it never falls back to the shared checkout.
   **The unit of serialization is the task, and the runtime is its capacity**: an
   intake loop decides which runtime takes a delivery and hands it to that runtime's
   scheduler, which groups the deliveries by task and serves up to
   `runtimes.<name>.max_concurrent` *distinct* tasks at once (default 1, at most 64,
   and never more than the host claimed at `init`). A task is never in two turns at
   once — a prompt sent to a busy session is silently swallowed, and a session belongs
   to one task for life — so a second delivery for a running task waits in its
   scheduler and runs when that turn ends; it is never dropped and never races the
   first. The routing decision is made once per task burst and remembered until that
   task's work drains, so two deliveries of one issue cannot reach two hosts. Do not
   replace the scheduler with a per-task mutex taken after dequeueing: that parks a
   worker on another task's turn (`internal/dispatch/schedule.go`, ADR 0003).
10. **The agent's reply must never start another turn.** Our own comments are
    recognised by content, because the agent posts as the same YouTrack user as
    the human. Three independent layers: the `<!-- flowhub-auto -->` marker in the
    reply, a probe against the task's recorded last reply (for when the marker is
    lost) — which is why only a turn that produced text replaces it — and an
    anchored trigger (`^\s*/opencode start\b`) so a reply that
    merely *mentions* the trigger cannot fire one. Do not loosen the anchoring,
    and do not add a natural-language trigger. The reply must also end with a
    blockquote that says opencode generated it and **what triggered the turn**,
    because a reader has to be able to tell an automated reply from a human one
    and judge whether the turn answered the right question. That basis comes from
    `Policy.Basis(decision, event)` — never from the model, which will invent a
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
12. **Installed files come from the binary, and the manifest is the contract.**
    `internal/provision` embeds the agent definition and the MCP snippet; nothing
    is fetched from the control plane, because the agent file is a system prompt
    and a compromised service must not be able to push prompts to every worker.
    Every write is recorded in `manifest.json` with its SHA-256, and a file a human
    changed is refused with a diff rather than overwritten — never silently
    replaced, and never deleted by `uninstall` unless `--force` says so. An
    artifact the manifest does not know is never touched. The agent's own
    configuration file is printed, not merged: it may hold comments, and a JSON
    round-trip would delete them.
13. **Dispatch must be stoppable without a restart.** `FLOWHUB_PAUSE_FILE`
    (default `<DataDir>/DISPATCH_OFF`) is checked before every delivery: while it
    exists, deliveries are audited and ignored. A kill switch that needs an API, a
    credential or a restart is not a kill switch.
14. **The core names no vendor, and each seam owns its own policy.** Three seams
    (ADR 0001): a source decodes an event and owns its trigger phrase, prompt, tool
    allowlist and download policy; a workspace provider produces and owns the task's
    directory, its baseline and the checks that depend on *its* filesystem; a
    runtime drives one turn and owns its session ruleset and shell policy.
    `internal/dispatch` imports the seam packages and never an implementation — do
    not add an `internal/agent/opencode` or `internal/workspace/localworktree`
    import to it, and do not move a policy into the dispatcher that belongs to one
    of them. The direction is what keeps a source unable to widen the shell policy
    and a runtime unable to decide where a task's code comes from. Two places name
    a product on purpose, because their job is that product's own host: `main`'s
    wiring, and `internal/provision` plus the activation prober, which install and
    verify opencode's files and read its agent registry and model catalogue.
15. **The environment file supplies defaults, and the environment always wins.**
    `~/.config/flowhub/.env` (or `FLOWHUB_ENV_FILE`; `-` disables it) is read by
    `internal/config/envfile.go` and applied **before the subcommand dispatch**, so
    `runtime init --check` sees what a start would see — that position is load-bearing
    and a later move would silently stop the subcommands from getting the file.
    Precedence is real environment → file → compiled default, and it must not be
    inverted: `FLOWHUB_ADDR=… ./bin/flowhub` has to mean what it says, and a file the
    operator forgot is not allowed to change it. `skipped_already_set` in the startup
    log and `env_file_skipped:` in `-print-config` exist to explain exactly that
    (see ADR 0005). The file holds the URL key and the token, so it carries the same
    `0600` floor as the credentials file in rule 3, a malformed line refuses with its
    line number, a duplicate key is refused rather than guessed, and **only variable
    names may reach a log or a report** — never a value.

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
3. **`source/youtrack.Schema` is the payload-analysis feature** — a flat sorted
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
7. **A task is bound to the runtime that prepared its worktree, for life.** The
   inventory lives behind the control API (`internal/runtimes`), the service is its
   only writer, and a mutation applies to the running process: `remove` refuses
   while non-terminal tasks are bound, and `--force` leaves those tasks to be
   refused one by one with the reason instead of silently re-homing them. The host
   reports what its profiles pin, and the dispatcher pins that model on the
   session, so what `init --check` verified is what the turn actually runs.
   `doctor --push` keeps that report current under the runtime secret, and it
   pushes only a report that passed: `last_seen` means "a host that was verified
   fit", so an expired credential surfaces while the operator is looking instead of
   at the first task. Activation checks more than liveness too: the profile the
   host claimed must exist on the advertised server (otherwise every turn runs
   under that server's default agent) and the model it reported must be one the
   server offers. **Which** runtime takes a new task is a routing decision: the
   project's `runtime`/`runtimes` set is an eligibility set, `runtime_policy`
   (`spread` by default) ranks it, every tie-break is deterministic, and the choice
   is logged with the numbers it came from. A bound task ignores all of that.

## Before you finish a change

```bash
make fmt-check && make lint && make test && make smoke
```

Report the exact commands you ran and their results. If a check could not be run
(for example a missing toolchain), say so explicitly instead of implying success.

The same targets run in CI (`.github/workflows/ci.yml`) on every push to `master` and
every pull request into it, so a local pass is the first gate rather than the only one.
Three jobs: `gate` runs the whole suite on Linux, building and testing with the Go
version `go.mod` declares and switching to `go1.27.1` for `staticcheck` alone (the
Makefile's pinned linter needs a newer toolchain than the module does); `cross-compile`
builds linux/amd64, linux/arm64 and darwin/arm64 and asserts each artifact's format; and
`test-darwin` runs `make test` on macOS, where path handling differs (`/tmp` is a symlink
to `/private/tmp`, which `localworktree` canonicalises). The action pins are kept current
by Dependabot (`.github/dependabot.yml`).
