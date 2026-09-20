# CODEBASE — FlowHub

Map for future sessions. Read this first, then the three design documents in the
repository root.

## What this project is

FlowHub is a Go service that turns DevOps events into work for a headless
`opencode` instance. Target event sources are YouTrack, Gitea and Drone; the
first implemented source is YouTrack.

Current state: **phase 1 complete** — the YouTrack webhook receiver exists,
is tested, and only records deliveries. It does **not** talk to opencode yet.

## Where things live

| Path | Responsibility |
| --- | --- |
| `cmd/flowhub/main.go` | Wiring, flags (`-version`, `-print-config`), HTTP server, `/healthz`, graceful shutdown, dedupe sweeper |
| `internal/config/config.go` | Environment parsing, validation, `Warnings()`, masked `Report()` |
| `internal/logging/` | Application logger: stderr tee plus a size-rotated log file (`flowhub.log`) |
| `internal/webhook/payload.go` | Lenient YouTrack payload model, event classification, timestamp and issue-id parsing |
| `internal/webhook/schema.go` | `Schema()`: flat sorted `path: type` report of a payload, array elements merged |
| `internal/webhook/handler.go` | The delivery pipeline: three locks, body limits, JSON, replay window, idempotency, audit, 202 |
| `internal/dedupe/dedupe.go` | TTL idempotency cache (`FLOWHUB_DEDUPE_TTL`) |
| `internal/store/store.go` | `Record` (the audit schema) and the `Recorder` interface |
| `internal/store/async.go` | Non-blocking audit queue; drops and counts instead of slowing the response |
| `internal/store/multi.go` | Fans one record out to several recorders |
| `internal/store/jsonl.go` | Daily rotating append-only JSONL writer (`webhook-YYYY-MM-DD.jsonl`, `0600`) |
| `internal/store/detail.go` | Human readable per-delivery payload log (`payload-YYYY-MM-DD.log`, `0600`) |
| `internal/projectmap/` | YouTrack project → repository routing table (`projectmap.go` loader/matching, `validate.go` filesystem+remote checks, `strip.go` `_`-prefixed doc keys) |
| `internal/metrics/metrics.go` | Counters behind `/healthz` |
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
* **`webhook.Schema` is the payload-analysis feature.** It walks the raw body and
  emits a sorted `path: type` list with array elements merged, which is how the
  documented-vs-real payload differences (missing `numberInProject`, presence of
  `project.id`, polymorphic `changedFields[].value`) become visible from one real
  delivery. It is computed in `Handler.describe`, so it lands in both logs.
* **Redaction is centralised** in `Handler.redact`, applied to path and query
  before either reaches the log or the audit record.
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
  `config/projects.json` (gitignored, host-specific paths); the committed template
  is `config/projects.example.json`. Nothing dispatches to opencode yet: this layer
  loads, validates and reports.
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
in `youtrack-webhook-and-flowhub-security.md` **§5.6**. These are the facts phase 2
must build on — do not re-derive them from the released app's source, which was
wrong about `issue.id`.

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

## Next steps (not started)

1. Phase 2: `task_key → session_id` registry, per-session serial queue, SSE
   subscription to `/event`, `devops` agent, permission arbiter (see
   `opencode-headless-automation-and-permissions.md` §5 and §6).
2. Phase 2: YouTrack trigger rules (author allowlist, `/opencode` trigger,
   project allowlist) and comment reply-back — build them on the verified payload
   facts above and the measured constraints in
   `opencode-devops-orchestration-design.md` §9.1.
3. Phase 3: alerting, key rotation, budget circuit breaker, optional HMAC signing
   through a custom workflow rule, aliyun always-on shim.
