# ADR 0001 — Pluggable event sources and agent runtimes

**Status:** Proposed (awaiting review — nothing in this document is implemented)
**Date:** 2026-09-21
**Scope:** the shape of the seam between "an event happened somewhere" and "an
agent works on it". It does not change behaviour, configuration or the audit
format on its own.

## Context

FlowHub is positioned as a workflow hub that aggregates DevOps nodes and agent
runtimes (`opencode-devops-orchestration-design.md`). Today it implements exactly
one of each: YouTrack as the event source, `opencode serve` as the agent runtime.
The other two planned sources (Gitea, Drone) are named in `AGENTS.md`.

The question this ADR answers: **can adding a source, or swapping the agent
runtime, avoid touching the orchestration core — and if so, where exactly is the
seam?**

Measured coupling today (non-test `.go` files, lines mentioning `youtrack`):

| Package | Lines | What the coupling actually is |
| --- | --- | --- |
| `internal/rules` | 13 | Field *semantics* (`Delivery.States`, `CommentText`) and the prose in `prompt.go`. Imports only `registry`. |
| `internal/opencode` | 4 (1 comment) | Nothing structural: client, runner, arbiter and phase logic are source-agnostic. |
| `internal/worktree` | 1 | A comment. |
| `internal/registry` | 2 | Field names and comments. |
| `internal/webhook` | 5 | The payload model is YouTrack-shaped; the three entry locks are transport-level and source-agnostic. |
| `internal/projectmap` | 18 | `youtrack_key`, `also_keys`, the issue-ID prefix fallback. Genuinely source-specific. |
| `internal/dispatch` | 14 | `webhook.Parse` + `describe()` (the translation), `AllowedMCPTools` (`youtrack_*` tool allowlist) and the reply check `call.Name == "youtrack_add_issue_comment"`. |
| `internal/agent` | — | Does not exist yet; `sessionRuleset()` currently lives in `dispatch`. |

Two facts make the seam cheap to introduce:

1. **There is exactly one translation point.** `dispatch.describe(rec,
   *webhook.Payload) → rules.Delivery` (`internal/dispatch/dispatch.go:512`),
   fed by a single `webhook.Parse` call (`dispatch.go:198`). Everything
   downstream already consumes `rules.Delivery` / `registry.Task`, not
   `webhook.Payload`.
2. **The receiving half already has a seam.** `webhook.Dispatcher` is an
   interface, so the receiver does not know what happens next.

What is *not* clean is the ownership of knowledge: source knowledge
(`AllowedMCPTools`, the reply tool name) and runtime knowledge (`sessionRuleset`,
`AllowedMCPTools`'s relationship to `bash` rules) both live in `dispatch`.

## Decision drivers

1. Adding a source must not require editing `dispatch`, `opencode`, `worktree` or
   `registry`.
2. The security boundary must survive the refactor. A source may declare **which
   tools** the agent may call; a source must never be able to widen the **shell**
   allowlist or the external-directory rules (`AGENTS.md` invariant 8).
3. The audit trail must stay greppable and replayable by the `jq` recipes in
   `README.md`.
4. Standard library only, one binary, one process, two users (see `AGENTS.md`).
5. Decisions that are still moving (structured output, the confirmation gate,
   reconciliation polling) must not be frozen into interfaces.
6. Distinguish two axes. A source and a runtime are independent, and a design
   that conflates them will be wrong for both.

## Considered options

### A. In-process interfaces with a compile-time registry — **chosen**

`internal/event` holds a neutral IR; `internal/source` holds the `Source`
interface plus one adapter per source, registered in `main` (or by blank import).
`internal/agent` holds a `Runtime` interface that `opencode` implements.

* Cost: one refactor (~1 day) plus 150–300 lines per new source (signature
  verification, payload parsing, one prompt).
* Buys: new sources and new runtimes are additive; the core never learns their
  names.
* Cost we accept: adding a source requires a rebuild. For a single-binary,
  two-user deployment that is the same as editing config.

### B. Configuration and prompt templates only (no code) — rejected as the primary form

Declare event names, trigger words, state fields and the prompt text in files.

* Buys: prompt and trigger changes without a rebuild.
* Why not primary: signature verification (YouTrack header token vs Gitea
  `X-Hub-Signature-256` HMAC vs Drone HMAC) and payload parsing cannot be
  expressed as configuration without inventing a small language, and a debugging
  session that turns into "guess the template" is exactly what this project's
  log-first discipline exists to avoid.
* Kept as a **complement** (migration step 3): policy and prompt text become
  per-source files once a second source exists to prove the shape is right.

### C. Go plugins (`.so`) — rejected

* Requires identical toolchain, dependency versions and build flags between host
  and plugin; failure mode is a load error at startup or a crash mid-turn.
* Needs cgo on macOS, breaking the current `CGO_ENABLED=0` static build.
* The security model would have to be re-drawn: a `.so` runs in-process with full
  authority, so "a source can only declare tool allowlists" stops being
  enforceable.

### D. Out-of-process plugins over IPC — rejected

* Would give real isolation and independent releases, at the cost of a protocol,
  supervision, a second credential store and a much larger failure surface
  (`nginx`-style restart semantics, message loss on restart, versioning).
* Solves a problem this deployment does not have: there is no third party
  writing plugins, and no plan to publish one.

## Decision

Adopt **A**, with the following shape.

### Two axes, two interfaces

```
YouTrack ─┐                                  ┌─ opencode serve  (today)
Gitea    ─┼→ [Source] → event.Event → [dispatcher] → [agent.Runtime] → worktree
Drone    ─┘     ↑                            ↑
          verify/parse/policy/prompt    rules/worktree/registry/audit
```

### `internal/event` — the neutral IR

```go
type Kind string // created | commented | state_set | updated | deleted | other

type Attachment struct{ Name, URL, MIME string; Bytes int64 }

type Subject struct {
    Project     string // routing key as the source spells it
    Key         string // stable task key, e.g. "TEST-17"
    Title       string
    Body        string
    URL         string
    Attachments []Attachment
}

type Event struct {
    Source   string    // "youtrack" | "gitea" | "drone"
    Kind     Kind
    Subject  Subject
    Actor    string
    State    string    // new workflow state, already normalised by the adapter
    StateField string  // which field carried it, for the log only
    Comment  string    // the comment that triggered this turn, if any
    Occurred time.Time
    Raw      []byte    // audit only; never parsed twice
}
```

`rules.Delivery` becomes this type (or is replaced by it), so `rules` keeps its
current property of importing nothing but `registry`.

### `internal/source` — the source adapter

```go
type Request struct { // what the receiver hands over, after the entry locks
    Method, Path, Query string
    Headers             http.Header
    RemoteIP, UserAgent string
    Body                []byte
}

// ToolPolicy is data, not behaviour: the dispatcher performs the checks, so a
// source cannot smuggle in logic.
type ToolPolicy struct {
    Allowed  []string // MCP/plugin tools the agent may call unattended
    Reply    []string // a completed call to any of these means "the turn replied"
    Download struct{ Hosts []string; Prefix string }
}

type Source interface {
    Name() string
    Parse(req *Request) (*event.Event, error) // verify signature + decode
    Policy() rules.Policy                     // trigger, start states, self markers, budget
    Tools() ToolPolicy
    Prompt(action rules.Action, e *event.Event, ctx rules.PromptContext) string
}
```

Decisions inside this section:

* **The three entry locks stay in the receiver.** They are transport-level (URL
  key, header token, source-IP allowlist) and every source deserves the same
  ones. `Source.Parse` only verifies *source* signatures (for example a Gitea
  HMAC), which the receiver cannot know about.
* **`ToolPolicy` is data.** The current hard-coded reply check
  (`dispatch.go:420`) and `AllowedMCPTools` (`dispatch.go:44`) become fields of
  the YouTrack adapter. `dispatch` keeps the generic loop
  (`anyCompleted(result.Tools, spec.Tools.Reply)`), so the source never gains the
  ability to widen the shell allowlist.
* **`Policy.StateFields` disappears.** The adapter knows which field carries the
  workflow state and normalises it into `Event.State`; the policy only lists the
  *values* that mean "start implementing".
* **Attachment downloads stay constrained** by `ToolPolicy.Download` and the
  existing prefix, which the arbiter already enforces (`opencode.CurlHosts`,
  `CurlOutputPrefix`). `opencode` should receive that policy as a parameter
  rather than reading a constant.

### `internal/agent` — the runtime

```go
type Turn struct {
    Directory, Prompt, Agent, Model, SessionID string
    Ruleset []PermissionRule
    Metadata map[string]any
    Deadline time.Duration
}

type Result struct {
    SessionID string
    Text      string
    Finished  bool
    TimedOut  bool
    Cost      float64
    Tokens    Tokens
    Tools     []ToolCall // {Name, Status}
    Permissions []PermissionDecision
}

type Runtime interface {
    Name() string
    Health(ctx context.Context) (Version string, err error)
    Run(ctx context.Context, turn Turn) (Result, error)
}
```

`opencode.Runner` already matches this shape; the adapter is a thin wrapper.
`sessionRuleset()` moves out of `dispatch` into the runtime, because "which
permissions the session starts with" is a runtime property, while "which tools a
source needs" is the source's.

### What `dispatch` looks like afterwards

It keeps: the queue, the pause file, the routing lookup, the cost and turn
budgets, the worktree hand-off, the registry update and the audit log. It loses:
`webhook.Parse`, `describe`, `AllowedMCPTools`, the reply-tool name, the session
ruleset and every `opencode.*` type. Its inputs become `(*store.Record,
*event.Event, source.Source, agent.Runtime)`.

### Consequences

* Positive: a new source is one new package plus a line in `main`; a new runtime
  is one adapter; the core stops naming vendors; the security boundary becomes
  explicit (tool policy is data owned by the source, shell policy is code owned
  by the runtime).
* Negative: one more indirection in the hot path (negligible — this path is
  already asynchronous and dominated by the model); the interface must be
  designed against two real sources eventually, not one.
* Accepted cost, recorded deliberately: **the audit schema is not made generic
  yet.** `store.Record` keeps its current YouTrack-derived fields
  (`issue_id_form`, `has_number_in_project`, `comment_ids`, `payload_schema`,
  `token_header`). Splitting it into `envelope` + `source_facts map[string]any`
  is deferred to the commit that adds the second source, so the shape is derived
  from two real cases instead of one. Until then a second source's audit records
  simply leave those fields empty, which is already how a payload without
  `numberInProject` looks today.

## Non-goals

1. No plugin SDK, no public extension API, no out-of-tree plugins. Adding a
   source means editing this repository and rebuilding.
2. No `.so` loading and no out-of-process sources (options C and D).
3. No generic audit schema before a second source exists.
4. No multi-tenant, multi-user or per-team isolation.
5. No configuration language for event parsing. Policy and prompt text may become
   files (step 3); payload decoding stays Go.

## Migration plan

Each step is independently committable and must leave `make fmt-check lint
staticcheck test test-race smoke` green. Behaviour must not change before step 3.

| Step | Content | Verification |
| --- | --- | --- |
| 0 | This ADR | reviewed and accepted |
| 1 | Add `internal/event` and `internal/source`; move `webhook.Parse`, `describe`, `AllowedMCPTools`, the reply check and `rules.Prompt` behind a `youtrack` adapter; `dispatch` consumes only the IR | Existing tests unchanged and green; `make smoke` unchanged; `dispatch` no longer imports `webhook` |
| 2 | Add `internal/agent`; make `opencode` implement it; move `sessionRuleset` there; `dispatch` no longer imports `opencode` | `go list -deps` shows the cut; the live path is re-verified with one real webhook turn |
| 3 | Per-source policy and prompt files (`sources/<name>.json` + template), with the env variables as the outermost default | A test asserts an override changes the rendered prompt; `-print-config` shows the effective per-source values |
| 4 | Gitea adapter as the acceptance test for the seam (HMAC-SHA256 `X-Hub-Signature-256`, issue and PR text) | A real Gitea webhook drives one analysis turn; the core packages show no diff beyond registration |

Step 4 is the point of the whole exercise: if adding Gitea requires touching
`dispatch`, the seam is wrong and should be revised rather than worked around.

## Open questions for review

1. **`Raw` in the IR, or the record?** Should `event.Event` carry the raw body
   (so the audit keeps writing exactly one record shape), or should the receiver
   keep owning the raw bytes and hand the parsed event plus a record id to the
   dispatcher? Recommendation: keep `Raw` in the event for now; revisit together
   with the `source_facts` split.
2. **Policy precedence.** With two sources there are three levels: environment
   default, per-source, per-project. Recommendation: per-project wins over
   per-source wins over the environment default, and `-print-config` prints the
   effective value with its origin. Confirm before step 3.
3. **Are attachments first-class?** The prompt currently tells the agent to
   download to a fixed prefix, and the arbiter allows exactly that. Gitea carries
   attachments too. Recommendation: keep them as `Subject.Attachments` and let
   `ToolPolicy.Download` decide where they may land.
4. **Task key collisions.** `registry` keys tasks by `Subject.Key`. Two sources
   could produce the same key (`TEST-17` in YouTrack and in a Gitea tracker).
   Recommendation: qualify the registry key with the source (`youtrack:TEST-17`)
   from the start, and migrate existing rows on first read. Confirm, because it
   changes the registry format.
5. **One runtime per process, or per task?** Recommendation: one runtime,
   selected by configuration, until a real need for two appears.
