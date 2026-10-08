# CODEBASE — FlowHub

Map for future sessions. Read this first, then the three design documents in the
repository root.

## What this project is

FlowHub is a Go service that turns DevOps events into work for a headless
`opencode` instance. Target event sources are YouTrack, Gitea and Drone; the
first implemented source is YouTrack.

Current state: the YouTrack webhook receiver and the opencode dispatcher are
both implemented and tested. The receiver always records; the dispatcher is
opt-in (`FLOWHUB_DISPATCH=1`) and turns an accepted delivery into one opencode
turn inside a per-task git worktree. A second host can be prepared and enrolled as
a named runtime (`flowhub runtime init` / `invite`, an inventory behind a control
API), and a task is bound to the runtime that prepared its worktree for life.

YouTrack is no longer hard-wired into the core: the receiver and the dispatcher
both consume `internal/event` and `internal/source`, and `internal/source/youtrack`
is the only package that knows what a YouTrack payload looks like or which tools
the agent may call (ADR 0001 step 1).

## Where things live

| Path | Responsibility |
| --- | --- |
| `cmd/flowhub/main.go` | Wiring, flags (`-version`, `-print-config`), HTTP server, `/healthz`, graceful shutdown, dedupe sweeper, and the activation prober that decides whether a host claiming a runtime name can actually run the work |
| `internal/config/config.go` | Environment parsing, validation, `Warnings()`, masked `Report()` |
| `internal/logging/` | Application logger: stderr tee plus a size-rotated log file (`flowhub.log`) |
| `internal/event/` | The neutral event IR (`event.go`): `Kind`, `Subject`, `Attachment`, `Event`, `Validate`. Data only — no I/O, no vendor names — so routing, the trigger policy, the audit decision and the reply check never learn which tracker a delivery came from (ADR 0001 step 1) |
| `internal/source/` | The event-source seam (`source.go`): `Request` (one delivery as the receiver saw it), `Decoded` (the event plus the source's own audit facts), `Source` (name, parse, policy, tools, prompt), and the data-only `ToolPolicy`/`DownloadPolicy`. A source cannot reach around the receiver's entry locks or the dispatcher's shell policy |
| `internal/source/youtrack/` | The YouTrack adapter: `payload.go` (lenient payload model, timestamp and issue-id parsing), `schema.go` (`Schema()`: flat sorted `path: type` report), `source.go` (payload → `event.Event`, including the workflow-state normalisation and the `Source` methods), `tools.go` (allowed MCP tools, reply tools, download host and prefix), `prompt.go` (ground rules, phase instructions, the reply sign-off contract) |
| `internal/webhook/handler.go` | The delivery pipeline: three locks, body limits, source decode, replay window, idempotency, audit, 202. It owns the transport and the redaction, not the payload |
| `internal/dedupe/dedupe.go` | TTL idempotency cache (`FLOWHUB_DEDUPE_TTL`) |
| `internal/store/store.go` | `Record` (the audit schema) and the `Recorder` interface |
| `internal/store/async.go` | Non-blocking audit queue; drops and counts instead of slowing the response |
| `internal/store/multi.go` | Fans one record out to several recorders |
| `internal/store/jsonl.go` | Daily rotating append-only JSONL writer (`webhook-YYYY-MM-DD.jsonl`, `0600`) |
| `internal/store/detail.go` | Human readable per-delivery payload log (`payload-YYYY-MM-DD.log`, `0600`) |
| `internal/projectmap/` | The FlowHub configuration file (ADR 0001 step 3). `format.go` holds both on-disk shapes (v1 and v2), the v1 translation, the refusals that name their replacement, the runtime/source block validation, the `prompt_file` reading, and the provenance of every effective value; `projectmap.go` holds the `(source, project)` index, matching, the per-level policy resolution and the loader; `credentials.go` holds the runtime Basic Auth surface (ADR 0004): the four password sources (`auth.password`, `auth.password_env`, `auth.password_file`, and the shared `credentials_file` keyed by runtime name), their precedence and mutual exclusion, the `{env:VAR}` expansion, the `0600` permission gate for any file holding a secret, and `ResolveCredential` — the one function the runtime factory and the activation prober both call; `validate.go` keeps the value checks and the `-print-config` report (the filesystem checks moved to the workspace provider in step 2); `strip.go` removes `_`-prefixed documentation keys |
| `internal/agent/` | The agent-runtime seam (`agent.go`): `Runtime` (`Name`, `Health`, `Run`), the neutral `Turn`/`Result`/`ToolCall`/`PermissionDecision`/`Tokens`, `Phase`, the attachment `Downloads` policy, and the model-reference spelling (`SplitModel`/`ValidateModel`) that configuration also validates against. A runtime builds its own session ruleset from `Turn.AllowedTools`, so a source cannot widen it |
| `internal/agent/opencode/` | The opencode implementation of that seam: the HTTP client (`client.go`, `types.go`: health, agent registry, model catalogue, sessions, permissions), the permission arbiter (`arbiter.go`), the phase-aware policy (`phase.go`) and the one-turn runner (`runner.go`), with `runtime.go` as the thin `agent.Runtime` adapter — it owns the session ruleset and the shell policy, which are this product's |
| `internal/workspace/` | The workspace seam (`workspace.go`): `Entry`, `Request`, `Handle`, `Base`, `Workspace` (`Resolve`, `Prepare`, `Check`, `Remove`) and `Validator`/`ValidateEntries` for the checks a provider can run before anything is created. `Handle.Path` is opaque: FlowHub never stats or joins it |
| `internal/workspace/localworktree/` | The co-located provider: one git worktree per task, branch `flowhub/<task key>`, `.flowhub/` scratch excluded through `info/exclude`; `Resolve` pins the base commit through the origin, `Prepare` produces exactly it (fetching on demand) and refuses to attach to a same-named branch that is not its descendant; `Validator` owns the filesystem checks that used to live in `projectmap` |
| `internal/registry/` | Append-only task registry (`registry.jsonl`), indexed by `(source, key)`: tracker item → repository, runtime, worktree, **base commit**, session, state, plan state, turns, cost, last reply. A row written before the source seam is read as `youtrack` and gains the field when it is next written |
| `internal/rules/` | Trigger policy (`rules.go`: ignore/analyze/plan/execute, trigger origin, `Basis`, self-comment detection, turn budget) over the neutral `event.Event`. The per-turn prompt text moved to the source adapter in ADR 0001 step 1 |
| `internal/dispatch/` | The workers: an intake loop that decides the runtime (once per task, so one issue cannot reach two hosts), one scheduler per runtime, routing, worktree cache, phase arbiter, session reuse, registry update, audit log. `schedule.go` is the per-task scheduler (ADR 0003): deliveries grouped by task, a ready FIFO, a running set and `max_concurrent` workers, so one task is never in two turns while different tasks run in parallel. `runtimes.go` is the addressing seam: the eligibility set a project declares, the `spread` / `first-healthy` ranking with its log line (free worker, committed work, active tasks, name), the sticky binding, the in-flight counters, and the startup check of declared names against the inventory |
| `internal/provision/` | Data-plane host preparation. `runner.go` is the host boundary (bounded commands, injected environment, identity), so the checks are tested without executing anything; `check.go` collects the capability report; `model.go` resolves each installed agent profile and the model it pins against the agent server's catalogue; `assets.go` embeds the agent definition and MCP snippet; `manifest.go` records what was installed (path, SHA-256, version); `install.go` plans and applies, refusing to overwrite a hand edit unless `--force`; `diff.go` renders the refusal; `register.go` enrols the host with a control plane, stores the runtime identity (`0600`) and pushes a fresh report (`doctor --push`); `command.go` routes `runtime init` / `doctor` / `uninstall` |
| `internal/runtimes/` | The control plane's runtime inventory (ADR 0002 step 3): `inventory.go` holds the states (`pending`/`active`/`revoked`), invites and runtime secrets (SHA-256 only, never the value), the sticky runtime binding and the heartbeat that refreshes a host's report and pinned models; `admin.go` is the `/control/v1` listener and its client; `command.go` is the `invite`/`list`/`show`/`remove`/`rotate` CLI, which talks to the running service so a change needs no restart |
| `internal/metrics/metrics.go` | Counters behind `/healthz` |
| `docs/adr/` | Architecture decision records: `0001` the seam that makes event sources and agent runtimes pluggable (a v2 configuration format, addressed runtimes, a pinned base commit per task) — **step 1 (the source seam) and step 5 (addressed runtimes and the pinned baseline) implemented**; `0002` how a data-plane host is installed and enrolled as a runtime — **steps 1–5 implemented** (capability report, artifacts + manifest, inventory + admin API + enrolment, `doctor --push`, activation check); `0003` per-task scheduling and what `max_concurrent` may mean — **implemented** (a scheduler per runtime, the routing memo, the operator's breadth capped by the host's claim) |
| `README.md` | Operator-facing documentation: config table, log formats, pipeline, jq recipes, verification checklist |

Tests live next to the code (`*_test.go`). `go test ./...` and `go vet ./...` are
clean; `gofmt -l .` reports nothing.

## Non-obvious decisions

* **Method-less mux patterns.** `main.go` registers the hook paths without a
  `POST` prefix so that a wrong method still reaches the handler, is audited as
  `method_not_allowed`, and gets a `405`. Using `"POST /path"` would let
  `ServeMux` answer 405 silently and leave the probe unrecorded.
* **Every rejection looks like success.** All lock/validation failures return
  `202` with an empty body and no explanatory header; the reason is only in the
  audit log and `/healthz`. Two exceptions are deliberate: `405` for a wrong
  method and `413` for an oversized body (nginx rejects both earlier anyway).
* **The raw token is never stored.** `store.TokenHeaderInfo` records the header
  name, presence, value length, a SHA-256 prefix and a `MatchesConfigured` flag,
  plus a flag for the literal string `secret` — that is how the open question in
  §4.2 of the security document gets answered without keeping a secret at rest.
  All header values pass through `maskHeaders`, so `Authorization`, `Cookie` and
  the token header can never reach disk.
* **Two log sinks, one queue.** `store.NewMulti` writes each record to JSONL
  (machine readable, full raw body) and to the detail log (human readable). Both
  run on the single `store.Async` goroutine, so ordering matches and the request
  path still does zero file I/O.
* **`source/youtrack.Schema` is the payload-analysis feature.** It walks the raw body and
  emits a sorted `path: type` list with array elements merged, which is how the
  documented-vs-real payload differences (missing `numberInProject`, presence of
  `project.id`, polymorphic `changedFields[].value`) become visible from one real
  delivery. It is computed by the adapter's `Parse` and copied into the record by
  `Handler.describe`, so it lands in both logs.
* **Redaction is centralised and positional.** `Handler.redactPath` and
  `Handler.redactQuery` are applied to the path and the query before either
  reaches the log or the audit record, and they work by *shape* (the segment
  after the hook base; the value of `k`). They must never match the configured
  key: the audited deliveries are precisely the ones whose key did not match, so
  matching the configured value wrote a live key to disk (found and fixed
  2026-09-21). `Options.HookPath` exists only so the handler knows its own route
  shape.
* **Parsing is intentionally lenient.** Nothing but `event` is required, unknown
  fields are ignored, and `changedFields[].value/oldValue` stay
  `json.RawMessage` because their type varies per field kind.
* **The audit sink is an interface** (`webhook.AuditSink`) with a single
  non-blocking `Record` method; `store.Async` wraps a real `store.Recorder`. This
  keeps the HTTP path free of disk latency.
* **Module path** is `github.com/yusiwen/flowhub` (the repository is not yet
  pushed anywhere; change the single `module` line in `go.mod` plus the imports
  if the real path differs).
* **Routing is declared, never guessed.** `internal/projectmap` maps
  `project.key` to a local checkout. A project key that is present but unmapped is
  a hard miss (no issue-prefix fallback — a payload whose key and prefix disagree
  must not be routed on a guess), and startup refuses a mapping whose path,
  work-tree or `origin` does not check out. The live table is
  the XDG path `~/.config/flowhub/config.json` (`$XDG_CONFIG_HOME/flowhub/config.json`
  when set), resolved to an absolute path with `~` expanded; the committed
  template is `config/config.example.json`. A second, dispatch-specific check runs
  at startup (`dispatch.Problems`): every routable entry needs a `default_branch`
  and a worktrees directory (its own or `FLOWHUB_WORKTREE_BASE`).
* **opencode permissions are decided in two layers.** The session-level ruleset
  (passed at session creation) is a short allowlist over a catch-all `ask`, and the
  catch-all comes **first** because the ruleset is an array evaluated
  last-match-wins. `edit` is `ask`, never `allow`, so the phase decides: the
  analysis arbiter rejects it, the execution arbiter allows it. The arbiter then
  answers those requests on an allowlist over shell *segments*. Absolute paths and
  `~` are rejected, because the ruleset's `external_directory` does not police a
  shell command. Rejections carry a reason so the model can adapt. The live check
  is `make test-live` (skipped unless `OPENCODE_LIVE=1`).
* **The dispatcher is a pure decision plus a dumb worker.** `internal/rules.Decide`
  turns (neutral event, recorded task) into one of ignore/analyze/plan/execute with
  no I/O, so the whole workflow is unit tested without opencode, git or a network.
  `internal/dispatch` then does the I/O: route, worktree, session, prompt, arbiter,
  registry. The prompt is built by the source adapter
  (`internal/source/youtrack.Prompt`), because the text names the tracker's tools
  and carries its sign-off contract; the phrase it tells the maintainer to type and
  the phrase the policy matches come from the one `rules.Policy` the adapter owns.
* **One worktree manager per base directory.** A routing entry may declare its own
  `worktrees` path, and a manager owns exactly one base, so `dispatch` caches
  managers by path. The manager fails closed: a worktree inside the repository, an
  existing branch checked out elsewhere, or a repo path that is not a git work
  tree is an error, not a fallback.
* **The dispatcher hook is an interface on the audit path but never blocking.**
  `webhook.Dispatcher` has one method that queues and returns; a full queue drops
  work and increments a counter instead of slowing the publisher. It is called
  only after the 202 is written, and only for accepted deliveries.
* **Two-tier validation.** `config.Load` → `validate()` rejects syntactic
  mistakes (unparsable values, an address without a port). `Config.Problems()`
  is the security gate checked in `main` before any file is touched: today it
  refuses a wildcard listen (`0.0.0.0` / `:` / `[::]`) unless
  `FLOWHUB_ALLOW_WILDCARD_LISTEN=1` acknowledges it. Keeping them apart is what
  lets `-print-config` still show the settings while a start is refused.

## How to run

```bash
nix develop            # or `direnv allow` once; provides Go, curl, jq, openssl
make build             # -> bin/flowhub with version/commit/build-time stamped in
make secrets           # print a fresh FLOWHUB_HOOK_KEY / FLOWHUB_TOKEN pair
make smoke             # end-to-end check against a real process
```

Manual run:

```bash
make build
FLOWHUB_HOOK_KEY=$(openssl rand -hex 32) \
FLOWHUB_TOKEN=$(openssl rand -hex 32) \
FLOWHUB_ALLOWED_SOURCES=127.0.0.1 \
FLOWHUB_LOG_HEADERS=true \
./bin/flowhub -print-config
./bin/flowhub
```

Environment table and the curl examples are in `README.md`. Verification targets:
`make test`, `make test-race`, `make lint`, `make staticcheck`, `make fmt-check`,
`make fuzz`.

## Build tooling

| File | Role |
| --- | --- |
| `Makefile` | Canonical workflow; injects `main.Version` / `main.CommitSHA` / `main.BuildTime` via ldflags and always builds with `CGO_ENABLED=0` |
| `.github/workflows/ci.yml` | Three jobs for every push to `master`, every pull request into it and a manual dispatch: `gate` (the seven targets on `ubuntu-24.04`, built and tested with the version `go.mod` declares, switching to `go1.27.1` for `staticcheck` alone), `cross-compile` (linux/amd64, linux/arm64 and darwin/arm64, with each artifact's format asserted by its magic bytes) and `test-darwin` (`make test` on `macos-15-intel`, where `/tmp` is a symlink to `/private/tmp`) |
| `.github/dependabot.yml` | Keeps the actions' SHA pins current: weekly, one grouped pull request, `dependencies` label |
| `flake.nix` + `flake.lock` | Pinned Go 1.27 dev shell (Go, gopls, gofumpt, git, openssl, curl, jq) |
| `.envrc` | direnv hook that activates the flake; sets `NIX_CONFIG` so the experimental features are on |
| `scripts/smoke.sh` | End-to-end check driven by `make smoke`; self-cleaning, asserts HTTP codes, audit reasons, log modes and credential containment |
| `AGENTS.md` | Project invariants for contributors and coding agents |
| `LICENSE` | MIT |

Two gotchas worth remembering:

* **Nix only sees files in the git index.** After creating a file, run `git add`
  before `nix develop` / `nix flake check`, otherwise the flake build fails with a
  missing-file error.
* **A Nix shell shadows BSD `stat` with GNU coreutils' `stat`**, where `-f` means
  "filesystem". `scripts/smoke.sh` detects the implementation instead of relying
  on `stat -f '%Lp' || stat -c '%a'`, which silently returns the wrong value.

## Verified payload facts (2026-09-20, 13 real deliveries)

Measured against the live YouTrack instance; full evidence and per-delivery samples
in `youtrack-webhook-and-flowhub-security.md` **§5.6**. The dispatcher is built on
these facts — do not re-derive them from the released app's source, which was wrong
about `issue.id`.

| Fact | Consequence for code |
| --- | --- |
| `payload.id` **is** the readable issue key (`TEST-11`) | `task_key = payload.id`; no REST round-trip needed to build it |
| `numberInProject`, `project.id`, user `id` do **not** exist | never wait for them |
| `comments[]` has **no `id` and no `textPreview`** | comment ids cannot be used as idempotency keys; match the trigger against `text` only |
| `issueDeleted` carries **no actor field** | the author allowlist cannot be applied to deletions |
| Adding a **tag fires no event at all** | use a custom field (single/multi-select) as a switch, not a tag |
| `timestamp` is the only usable event time | `updated` lags one event; `created` is ~15 s early; never sort or watermark on them |
| `changedFields[].value` has **six shapes** (enum object, string, user object, Period, epoch-ms number, array) | keep `json.RawMessage`; never assume `string` |
| Custom fields appear **twice** (`State` + `状态`) when the internal and localized names differ | iterate all entries; never index or count |
| Multi-value fields are **full snapshots**, not deltas | set-difference to detect what was added |
| `presentation` is not the UI string (Period gives `PT1H`) | decide with `value.name`, format for display yourself |
| Unchanged fields are **entirely absent** | REST-query the state only when a round actually needs it |
| Date values are anchored at **12:00 UTC** | compare dates in the project timezone, not raw milliseconds |

## Open items carried from the design documents

These were answered by the first real deliveries (2026-09-20); see the table above
and `youtrack-webhook-and-flowhub-security.md` §5.6.

1. ~~Is the header token the configured value or the literal `secret`?~~ → the real token (13/13 deliveries). Lock 2 is a genuine boundary.
2. ~~Does the payload really lack `numberInProject`?~~ → confirmed absent.
3. ~~Is `issue.id` the database form or the readable form?~~ → **readable** (`TEST-11`), contradicting the released app's source.
4. ~~What is the real source IP nginx presents?~~ → `10.1.0.1` (the router forwarding WG→LAN); `X-Real-IP`/`X-Forwarded-For` are not set by nginx.

Still open: the 6 unseen event types, the `created` ~15 s offset, the date anchor
with a second sample, and whether a tag change is truly skipped (needs the app log).

## Dispatcher: what was measured live (2026-09-21)

Two-turn live task in the TEST project (opencode 1.18.31, agent `devops`):

| Step | Result |
| --- | --- |
| `issueCreated` → analyze | 19s, $0.0035, read-only turn, comment posted with the marker |
| `/opencode start` with a plan → execute | 26s, $0.0022, `README.md` edited and committed (signed) on `flowhub/TEST-13`, never pushed |
| State → `In Progress` with no plan → plan | 31s, $0.0051, plan comment plus its blocking questions |
| Own comment (marker, then marker-stripped) | ignored by both layers; the turn count stayed at 2 |

Facts worth remembering:

- The session ruleset is an array evaluated **last-match-wins**, so the catch-all
  `ask` must be the first entry. A trailing catch-all made an allowed tool ask
  again (measured).
- A compound shell command arrives as **one** permission request whose `patterns`
  hold one entry per segment while `metadata.command` holds the whole line. Judge
  the command, and treat `patterns` as a second opinion, not the source of truth.
- `reject` with a reason reaches the model as feedback and it rephrases; `reject`
  does not stall the turn. `always` is never the default.
- A new opencode **agent file** is only picked up by a new directory instance: an
  already-used directory needs a server restart. The per-task worktree is always
  new, so this only bites when testing against a repository path directly.
- A typed-nil `*dispatch.Dispatcher` stored in the webhook's interface is not
  `== nil`; that panic cost the publisher its response while the log said 202.
  `webhookOptions` sets the interface only when there is a dispatcher, and
  `Dispatch` tolerates a nil receiver.

## Next steps (not started)

1. Structured output: ask the agent for `json_schema` output so "no blocking
   questions" can be a machine-readable gate on `/opencode start` instead of prose.
2. Reconciliation polling for deliveries lost while FlowHub was down (the
   published webhook app does not retry).
3. More than one dispatch worker, which needs per-task locking first (a prompt to
   a busy session is silently swallowed).
4. Phase 3: alerting, key rotation, budget circuit breaker, optional HMAC signing
   through a custom workflow rule, aliyun always-on shim.
