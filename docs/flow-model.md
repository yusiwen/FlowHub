# Flow model

Status: normative for the builtin flow (M0). The implementation lands in M1; this
document is the contract its tests are written against. The decision record is
[ADR 0013](adr/0013-flow-orchestration.md), qualified by
[ADR 0014](adr/0014-e2e-based-validation-and-pilot-waiver.md); the epic is #92; the
deferred surface is #91.

The model exists to answer one question before any code moves: **can the current
YouTrack → opencode pipeline be expressed as a flow without a special case in the
engine?** The parity table in §9 is the acceptance test. If a row cannot be
expressed, the model is incomplete, not the pipeline.

## 1. Scope

In scope for this round:

- one upstream (`youtrack`) with a `webhook` ingress;
- one builtin flow (`default-youtrack`), embedded and read-only;
- the task service and the runner the flow uses;
- runs, journals, shadow mode, deployment (`off | shadow | on`);
- `/hub/v1`, the hub token, the CLI;
- the read-only Flows page in the desktop Hub tab.

Out of scope until #91:

- custom/user flows, clone/edit/deploy, the visual editor;
- `poll` ingress implementation (the interface and cursor model are defined here);
- `http.request`, `human.approval`, `wait.timer`, `notify.tracker`;
- parallel branches and joins;
- manual runs, approve/retry/cancel buttons, SSE;
- multi-user actor tokens.

### Terms

| Term | Meaning |
| --- | --- |
| Upstream | An adapter for one source system (YouTrack). |
| Ingress | How events arrive: `webhook`, `poll`, `manual`, `clock`. |
| Event | The neutral envelope every ingress produces. |
| Trigger | A flow's subscription over events. |
| Flow | A versioned definition: triggers, tasks, nodes, edges, limits. |
| Run | One execution of a flow, with a definition and event snapshot. |
| Step | One node's execution inside a run. |
| Task | The unit of continuity: one worktree, one session, one runtime, one grant. |
| Runtime | An agent host (`opencode`) able to run a turn. |
| Placement | Choosing the runtime and workspace for a task. |
| Profile | The upstream's source-specific policy (triggers, prompts, tools, evaluate). |

## 2. Layers

```
YouTrack webhook
  -> upstream/youtrack        decode, normalise, dedupe_key, profile
  -> event log                 <DataDir>/events/YYYY-MM-DD.jsonl
  -> trigger router            flow definitions -> candidate runs
  -> flow engine               run journal, steps, branch decisions
       -> task service         registry, worktree, session, runtime, grants
            -> runner          resolve runtime, acquire capacity, one turn
                 -> agent.Runtime
       -> reserved: http.request, human.approval, wait.timer, notify.tracker
  -> /hub/v1 + desktop Flows page
```

Two separations are load-bearing:

- **Adapter vs ingress.** The adapter knows YouTrack; the ingress knows webhook. A
  second ingress (`poll`) must not change the adapter's normalisation, and both must
  share one dedupe namespace.
- **Flow engine vs task service.** A graph decides *which* task operation happens
  next; it never talks to an opencode server or a git clone. The task service owns
  the worktree, session, runtime affinity, lease and grants.

During M1, `internal/dispatch` still owns the live pipeline. The flow engine runs in
`shadow`: it reads the registry, evaluates the graph, and writes runs, but it never
prepares a worktree, creates a session, or calls a runtime. The runner extracted in
M1 is shared with dispatch so the two paths cannot drift.

## 3. Event

### 3.1 Envelope

```json
{
  "id": "evt_01J0000000000000000000001",
  "schema": 1,
  "upstream": "youtrack",
  "kind": "comment.added",
  "occurred_at": "2026-10-21T09:00:00Z",
  "received_at": "2026-10-21T09:00:01Z",
  "subject": {
    "type": "issue",
    "project": "TEST",
    "key": "TEST-12",
    "title": "Fix the widget",
    "url": "https://youtrack.example/issue/TEST-12"
  },
  "actor": {"login": "yusiwen", "display": "Yusi Wen"},
  "changes": [],
  "comment": {"text": "/opencode start", "author": "yusiwen"},
  "payload": {"id": "TEST-12", "summary": "Fix the widget"},
  "dedupe_key": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
  "origin": "upstream:youtrack"
}
```

| Field | Meaning |
| --- | --- |
| `id` | Hub-assigned, time-sortable (`evt_` + ULID). The run's stable handle to the event. |
| `schema` | Envelope version, currently `1`. |
| `upstream` | Adapter name, e.g. `youtrack`. |
| `kind` | `<subject>.<action>`: `issue.created`, `issue.updated`, `issue.deleted`, `comment.added`, `attachment.added`. `clock.tick` and `flow.finished` are reserved. |
| `occurred_at` | Source time. Used by conditions; **not** an ordering guarantee. |
| `received_at` | Hub time. Used for queue order and lag. |
| `subject` | `type` (`issue`/`task`), `project`, `key`, `title`, `url`. The task identity is `(upstream, key)`. |
| `actor` | `login` and optional `display`. Empty only for events with no actor (an `issueDeleted` delivery). |
| `changes` | Normalised field transitions: `field`, `from`, `to`. |
| `comment` | `text` and `author`, for `comment.added` only. |
| `payload` | Raw adapter payload, bounded (1 MiB), unstable by contract. Flows must prefer the normalised fields. |
| `dedupe_key` | Adapter-computed, stable across ingress modes. YouTrack comments have no id, so the key is `sha256(issue key, timestamp, author, text)`. |
| `origin` | `upstream:<name>` for a source event, `flow:<run-id>` for an event a flow produced. |
| `trace` | Optional ingress trace: delivery id or poll cursor. |

The builtin flow's trigger matches `*`; the kind vocabulary exists for flows that want
to be narrower and for the UI. An unknown kind is accepted and reaches
`task.evaluate`, which ignores it with a reason instead of failing the run.

### 3.2 Dedupe and ordering

- Uniqueness is `(upstream, dedupe_key)` for 24 hours. Webhook duplicates and a
  webhook/poll overlap collapse to one event.
- Delivery is at-least-once. A run is unique per `(flow, trigger, event_id)`; a
  duplicate delivery returns the existing run.
- The event log is append-ordered by `received_at`. There is no global causal order:
  a late delivery may carry an older `occurred_at`, and flows must not assume
  monotonic source time. Per-upstream sequence exists only for diagnostics.

### 3.3 Event log

- `<DataDir>/events/YYYY-MM-DD.jsonl`, one object per line, mode `0600`, directory
  `0700`.
- Retention is 30 days by default (`LogOptions.Retention` in the implementation;
  the environment setting lands with the rest of the flow configuration),
  independent of the run retention.
- Every run stores an event snapshot in its `meta.json`, so pruning the log does not
  break run inspection.
- The audit (`audit.jsonl`) stays as it is during migration; the event log is
  additional, not a replacement. Both can be correlated by the delivery trace.

### 3.4 Replay cursor

- `<DataDir>/flow-cursor.json` holds the last event id the flow worker **accepted
  for processing**, mode `0600`, replaced atomically.
- The cursor advances in scheduler receive order once a run exists (or the event is
  ignored/mode `off`/denied), so a crash mid-run leaves the existing run to recovery
  instead of replaying the event into a second run. Parallel runs are allowed, but a
  later event cannot advance the cursor past an earlier event that has not reached
  that point. A paused delivery does not advance, so it is replayed after the pause
  is lifted.
- At startup the flow reads every event after the cursor and submits it to the
  worker. A missing cursor initializes at the current durable tail, so upgrading a
  deployment does not replay its whole history; an event id that has aged out of the
  30-day log is a startup error rather than a guess.
- The queue can still drop while the process is running. A periodic catch-up
  (default 5s) reads the log after the cursor and blocks until the queue accepts the
  missing events, so the gap is bounded rather than waiting for a restart. Startup
  does one catch-up before serving HTTP; the periodic loop covers the running process.
- The E2E `TestReplayCursor` appends valid events while the process is running
  (periodic catch-up), then stops, appends more, restarts (startup replay), and
  asserts every event produced a run.

## 4. Upstream

### 4.1 Adapter

One adapter per source system. It owns:

- decoding the raw delivery into the envelope (subject, actor, changes, comment);
- the `dedupe_key`, shared by every ingress;
- the **profile**: trigger phrase, start states, author allowlist, self markers,
  prompt templates, reply tools, tool policy, download policy;
- `evaluate(event, task) -> {action, basis, reason}`: what this event means for this
  task, using the task projection.

M1 does not introduce a new configuration file. The existing configuration
(`sources.youtrack`, the environment-held hook key and token) remains the source of
the profile; the upstream name is `youtrack`. A future second source introduces an
`upstreams` block; that migration is deferred until it buys something.

### 4.2 Ingress: webhook (M1)

The existing entry is unchanged: path `/hooks/youtrack/<key>`, three locks, body cap,
content type, JSON, known event, replay window, idempotency, `202` with an empty
body. What changes is what happens after acceptance:

1. the adapter normalises the delivery into an event;
2. the event is appended to the event log (dedupe first);
3. the trigger router decides which flows are candidates;
4. in M1, dispatch still runs the live turn; the flow engine runs shadow runs.

The event append is synchronous and happens **before** the `202` (M2-1): it
validates, dedupes, writes the JSONL line and notifies subscribers, without fsync.
A failed append answers `503`, records `event_append_failed`, and releases both
idempotency keys so a retry can work; a `202` is only sent after the event is on
disk. Audit and dispatch remain asynchronous and keep their drop counters. This is
the amendment to AGENTS rule 1 recorded by ADR 0013 decision 11.

### 4.3 Ingress: poll (reserved)

The model reserves the interface, not an implementation:

```
type Poller interface {
    Poll(ctx context.Context, cursor Cursor) ([]Event, Cursor, error)
}
```

- The cursor is persisted per upstream, at `<DataDir>/upstreams/<name>/cursor.json`,
  and advances only after the events are appended.
- An overlap window (default 10 minutes) replays the tail; dedupe absorbs duplicates.
  YouTrack's `updated` lags, so it is a watermark, never an ordering key.
- One poller per upstream, single-flight, exponential backoff with jitter; a slow
  poll skips the next tick instead of overlapping. Poll failures do not fail the
  service; they surface in `/healthz` (`last_poll_at`, `lag`, `consecutive_errors`,
  `next_poll_at`).
- A webhook and a poll of the same upstream may run together; the adapter's dedupe key
  is the reason that is safe.

No poller is built in this round. The model exists so adding one does not change a
flow.

### 4.4 Health

`/healthz` gains an `upstreams` block: enabled, last event, last error, lag, and the
counts the audit already keeps. The dashboard shows it next to the existing service
card; a poller later adds its own fields without a new endpoint.

## 5. Flow definition

### 5.1 File

- Builtin: `testdata/flows/default-youtrack.json`, embedded into the binary with
  `//go:embed`. It is the source of truth for the graph; the binary serves the same
  bytes from `/hub/v1/flows/default-youtrack`.
- User flows (later): `~/.config/flowhub/flows/<id>.json`, one file per flow, atomic
  write, content hash as the revision. Not implemented in this round.
- Strict JSON: unknown fields are an error, like the routing configuration. `version`
  is explicit; a future version is translated on read, never guessed.

### 5.2 Shape

```json
{
  "version": 1,
  "id": "default-youtrack",
  "name": "Default YouTrack pipeline",
  "description": "Today's dispatch behaviour expressed as a flow.",
  "builtin": true,
  "upstream": "youtrack",
  "triggers": [
    {
      "id": "all-youtrack-events",
      "event": "*",
      "exclusive": true,
      "priority": 0,
      "dedupe": {"scope": "event"}
    }
  ],
  "tasks": {
    "primary": {"identity": "upstream", "reuse": true}
  }
}
```

The block is abridged: `nodes` and `edges` are omitted for brevity and the fixture is
the complete definition.

| Field | Meaning |
| --- | --- |
| `version` | Definition format, `1`. |
| `id` | Stable; `^[a-z][a-z0-9-]{1,63}$`; used in run, branch and worktree names. |
| `name`, `description` | Human labels; not used by matching. |
| `builtin` | `true` means embedded, read-only, no `PUT`/`DELETE`. |
| `upstream` | The adapter name every trigger inherits. |
| `triggers` | Subscriptions; see §5.3. |
| `tasks` | Named task specs; the builtin flow has `primary`. |
| `nodes`, `edges` | The graph; see §5.4. |
| `limits` | Optional run limits (`timeout`, `max_cost`). Absent means the configuration defaults. |

### 5.3 Triggers

| Field | Meaning |
| --- | --- |
| `id` | Stable trigger id, recorded in the run. |
| `event` | Glob over kinds: `*`, `issue.*`, or an exact kind. |
| `exclusive` | At most one exclusive flow runs for one event. |
| `priority` | Higher wins among exclusive matches; ties fail startup, not runtime. |
| `dedupe.scope` | `event` (default), `subject` (one active run per subject), `none`. |
| `when` | Optional expression over `trigger`; must not read task state. |

Task-dependent decisions belong in `task.evaluate`, under the task lease, not in a
trigger. This is what prevents the "trigger saw `plan=draft`, the run later executed
after another event changed it" race. The builtin flow deliberately triggers on `*`
and lets `evaluate` decide; a broad trigger is cheaper to reason about than a
partially informed one.

Only one trigger per flow may match an event; startup warns on overlap. Additive
triggers (multiple flows observing one event) are reserved until custom flows exist.

### 5.4 Graph

- Exactly one `start` node, at least one `end`, every node reachable from `start`,
  no cycles.
- Edges are `{from, to, when?}`. Outgoing edges are evaluated in declaration order;
  the first true condition wins; an edge without `when` is the default and must be
  last. A `branch` node must have at most one true outgoing edge.
- A non-`branch` node has exactly one outgoing edge in v1. Error edges are reserved.
- Node ids are stable and used by the run overlay; edges are identified by
  `(from, to, index)`.

### 5.5 Context and expressions

A step reads:

```
trigger.*        the event envelope
params.*         manual-run parameters (reserved)
run.id, run.started_at
steps.<id>.state, steps.<id>.output.*
tasks.<name>.state | plan | session | worktree | last_reply | last_action |
              refusals | grant
```

No secrets are in the context. A secret is referenced by name in a node's `auth`
field (reserved) and resolved by the engine at the call site; it is never rendered,
logged, or stored in the run journal.

Expressions:

- values: strings (`'...'` or `"..."`), numbers, booleans, `null`, paths;
- operators: `==`, `!=`, `<`, `<=`, `>`, `>=`, `&&`, `||`, `!`, `in`, `matches`
  (RE2);
- functions: `len`, `has`; no user-defined functions and no arithmetic;
- a string field may contain `{{ expression }}` templates; a template renders a
  scalar only, and an object/array is an error, not JSON-encoded implicitly.

Compilation happens on save and on startup: syntax, known roots, operator types, and
`steps.<id>` referring to an ancestor are checked before a run exists. An unknown
path at runtime fails the step; it never becomes an empty string. That rule is the
same one the prompt loader and the configuration loader already follow: a silent
no-op is worse than a refusal.

### 5.6 Validation

Startup (and `PUT` later) refuses:

- duplicate node ids, unknown edge endpoints, a missing/duplicate `start`, no `end`;
- a cycle, an unreachable node, a node with no outgoing edge (except `end`);
- a `branch` with two defaults, or a default that is not last;
- an unknown node type, or a config field the type does not define;
- a bad expression, an unknown root, a `steps.<id>` that is not an ancestor;
- a task ref that is not declared, a task identity it cannot satisfy;
- a missing required config (`task.turn.action`, `task.turn.phase`);
- a negative limit, a malformed duration, or an id that is not a safe slug;
- a `builtin: true` definition in a writable location (the embedded one only).

A definition that fails is isolated: the flow is listed with its error and refused,
but it does not stop the receiver. `FLOWHUB_FLOWS_STRICT=1` makes the same failure
stop startup, for CI.

### 5.7 Deployment

Deployment is state, not definition:

```json
{
  "version": 1,
  "flows": {
    "default-youtrack": {
      "mode": "shadow",
      "projects": [],
      "updated_at": "2026-10-21T12:00:00Z",
      "updated_by": "operator"
    }
  }
}
```

- Path: `<DataDir>/flows/state.json`, atomic write, `0600`.
- `mode`: `off` (no runs), `shadow` (evaluate only), `on` (full runs).
- `projects`: for `on`, the project keys the flow owns; empty means all projects.
  `shadow` is always all projects; `off` is none.
- A missing file means every flow is `off`. The builtin flow is never deleted.
- M1 supports `off` and `shadow`; M2-2 adds the project-scoped deployment schema
  and a token-authenticated `POST /hub/v1/flows/{id}/deployment` (the CLI writes
  the same file). `on` requires the shared task service; when it is persisted before
  start, FlowHub builds that service and the selector without starting dispatch intake:
  dispatch skips the projects the flow owns, and the flow engine executes
  `task.ensure`/`task.turn`/`task.grant`/`task.revoke` through that service.

## 6. Node catalogue

### 6.1 v1 nodes

| Type | Config | Output |
| --- | --- | --- |
| `start` | — | — |
| `task.ensure` | `task` | `task_id`, `routable`, `project`, `runtime`, `reused` |
| `task.evaluate` | `task`, `policy` (`upstream`) | `action`, `basis`, `reason` |
| `task.turn` | `task`, `action` (`analyze`/`plan`/`execute`/`last`), `phase` (`analysis`/`execution`/`auto`), `require_reply` | `session_id`, `reply`, `cost`, `tokens`, `timed_out`, `refusals` |
| `task.grant` | `task`, `commands` (`refusals` or a literal list) | `granted`, `actor` |
| `task.revoke` | `task` | `revoked` |
| `branch` | — (outgoing edges carry `when`) | `selected` |
| `end` | `state` (`succeeded` default, or `failed`) | — |

Details:

- **`task.ensure`** resolves the task identity, the project and the runtime. In
  `on` it acquires or creates the registry row and prepares the worktree; in
  `shadow` it resolves the row read-only and never creates a directory or session.
  An unmapped project is not an error: `routable: false`, and `task.evaluate`
  returns `ignore` with the routing reason.
- **`task.evaluate`** calls the upstream profile with the event and the task
  projection. In `on` it runs under the task lease; in `shadow` it reads a
  consistent snapshot without a lease. The output is exactly what dispatch's
  `rules.Decide` returns: `action`, trigger, reason.
- **`task.turn`** runs one turn through the runner. `phase: auto` derives the phase
  from the action (`execute` → execution, otherwise analysis); it exists for the
  permit continuation, which must reuse the action the refusals stopped. With
  `require_reply`, the task service applies the profile's reply contract: one
  bounded nudge in the same session, a second silent finish fails. A timeout is
  reported as `timed_out`, not as a failed step; the task stays executing.
- **`task.grant`** grants the literal commands the task's most recent turn was
  refused for (`commands: "refusals"`) or an explicit list validated against that
  set. A command that was not refused is refused, not silently added. `actor` is
  recorded.
- **`task.revoke`** clears the grant; it runs no turn.
- **`branch`** is a visual and validation anchor; the conditions live on its
  outgoing edges. It exists so the UI has one decision point and the validator has
  one place to enforce "at most one true edge".
- **`end`** terminates the run. A run that took the default branch after an
  `ignore` is `succeeded`, with the reason in the evaluate output; "ignored" is a
  decision, not a failure.

### 6.2 Reserved nodes

`http.request`, `human.approval`, `wait.timer`, `notify.tracker`, `parallel`, `join`,
`subflow`. They have no executor in M1. A definition that uses one is refused with
"not implemented in this version"; the catalogue endpoint lists them as reserved so
the UI can grey them out. #91 owns their semantics.

## 7. Task

### 7.1 Identity and reuse

- Builtin: `(upstream, subject.key)`, e.g. `("youtrack", "TEST-12")`. This is exactly
  the current registry index, so a task that was created by dispatch is the same
  task the flow sees; its session, worktree, base commit and grant survive the
  cutover.
- A custom flow (later) may use `(flow, run, task-name)` for work that has no tracker
  subject.
- `reuse: true` means "look up the identity; create only if absent". `Ensure` is
  idempotent; two runs racing on one task produce one row and one workspace.

### 7.2 State

Task state is the registry's existing machine:

```
analyzing -> awaiting_input -> planning -> executing -> done
                                    \-> failed
```

`plan_state` is `none | draft | confirmed`; the builtin evaluate uses `plan != none`
to choose between a planning turn and an execution turn, exactly like
`rules.startDecision`. `last_action` lets a permit continue the action the refusals
stopped (`task.turn.action: last`). A `draft` is written only by a plan turn that
read the issue discussion before it replied; a failed plan turn leaves `none`, so a
stale draft cannot become the next start's execution input.

### 7.3 Lease

- Every task operation (`ensure`, `evaluate`, `turn`, `grant`, `revoke`) is serialized
  per task.
- The builtin flow holds the lease across `evaluate -> turn`, so the decision cannot
  be invalidated between the two.
- The lease is released when the run ends or parks on a human/timer step. In v1 there
  is no parked step in the builtin flow; the lease is therefore per event burst.
- A second run for the same task waits as `queued`; it never runs a turn in parallel.

### 7.4 Placement

- The task spec names a target: an explicit runtime, or (builtin) the project's
  routing entry and its `runtime`/`runtimes`/`runtime_policy`.
- The task service filters eligible runtimes by project, state, capacity and model,
  ranks deterministically (spread by in-flight, then name), and records the
  candidates and the choice in the run journal.
- The chosen runtime is **for life**. If it becomes unavailable, the task fails with
  the reason rather than re-homing; a session cannot move between hosts.
- Workspace: the existing provider, one worktree per task, base commit pinned through
  the origin once. The handle is opaque to the flow.
- A runtime's capacity is acquired per turn, not for the whole task; the shared
  broker is the same one dispatch uses during the migration.

### 7.5 Turn policy

- Agent, model and deadline precedence stays exactly what dispatch does today:
  project entry, then runtime declaration, then process default; the model is pinned
  from the host's attestation when the file does not name one.
- Tools come from the upstream profile; the runtime builds the session ruleset and
  owns the shell policy. A flow cannot widen either.
- `require_reply` uses the profile's reply tools and marker. One nudge, in the same
  session, with a bounded deadline; a second silent finish fails the task.
- A plan turn enforces the profile's `PlanRead`: a completed read must precede a
  completed reply. The reply tool is not on the profile's `Allowed` allowlist; the
  session ruleset asks for it, and the arbiter refuses it until this turn has a
  completed read, then allows it. One bounded follow-up (`NudgeRead`) remains the
  backstop; a plan still posted without the read is failed with `plan_state=none`,
  never a draft. The read tools are the profile's, so a tracker with no discussion
  is unaffected. A session created before the ruleset change still allows the reply;
  the post-turn gate covers it.
- Cost and turns accumulate on the task; `max_cost` and `max_turns` are enforced
  before the next turn, not after the budget is already spent.

### 7.6 Grants

- The only commands a grant may contain are the literal refusals recorded by the
  task's **most recent** turn. A permit comment that arrives with nothing refused is
  ignored with a reason, not silently accepted.
- A grant is scoped to the task, applies to later turns until `done` or `revoke`, and
  is recorded with actor and timestamp.
- The runtime consults the grant **after** its deny list, worktree check and phase
  rule, so a grant can only turn "not on the allowlist" into "allowed", never widen a
  forbidden command or a read-only phase.

## 8. Run and journal

### 8.1 States

| Run state | Meaning |
| --- | --- |
| `queued` | Accepted, waiting for a run slot or the task lease. |
| `running` | A step is executing. |
| `waiting` | Parked on a human/timer step (reserved; not in the builtin flow). |
| `needs_attention` | Interrupted or ambiguous; a human decides. |
| `succeeded` | Reached `end` without a failed step. |
| `failed` | A step failed, or the run budget was exhausted. |
| `canceled` | A human canceled it. |

| Step state | Meaning |
| --- | --- |
| `pending` | Not started. |
| `running` | Executing. |
| `waiting` | Parked (reserved). |
| `succeeded` | Completed; `output` is set. |
| `failed` | Completed with an error. |
| `skipped` | Not executed (branch not taken, or shadow). |
| `interrupted` | Started, no terminal event on replay. |

### 8.2 Journal

`meta.json` holds the immutable definition and event snapshots plus a folded state;
`journal.jsonl` is the truth. Event types:

```
run.queued, run.started, run.waiting, run.resumed, run.finished
task.ensured, task.lease.acquired, task.lease.released
step.started, step.succeeded, step.failed, step.skipped, step.interrupted
edge.selected
run.error
```

Every transition carries `seq`, `ts`, `run`, optional `step`, `attempt`, and the
data the fold needs. Decisions (which edge, which runtime, which action) are written
down with their inputs; the UI and the parity harness read the same records.

### 8.3 Recovery

- On startup, replay every non-terminal run.
- `queued` → resume queueing.
- `running` with a `step.started` and no terminal event → step `interrupted`, run
  `needs_attention`. An agent turn is **never** retried automatically: it may have
  edited files, spent tokens, or both.
- `waiting` → resume waiting (reserved).
- A task whose turn was interrupted is reconciled from the session id the journal
  recorded; the task stays `executing` until inspected, never silently `done`.
- `needs_attention` runs are not pruned by retention.

### 8.4 Idempotency and concurrency

- Event: `(upstream, dedupe_key)`.
- Run: `(flow, trigger, event_id)`; a duplicate returns the existing run.
- Task: `Ensure` is idempotent by identity.
- Turn: a step attempt is journaled before the call. A crash during the call is
  `interrupted`, not a second attempt. A manual retry (later) is a new attempt with
  its own id, never a silent re-run.
- Concurrency: one active step per run in v1; `FLOWHUB_FLOW_CONCURRENCY`
  distinct tasks (default 4, `1`..`64`) with one task's events serialized; a
  per-task lease; per-runtime capacity. Shadow runs consume no runtime capacity.
- Retention: runs 30 days / 1000 by default; event log 30 days; artifacts bounded.
  `waiting` and `needs_attention` survive pruning.

### 8.5 Shadow

Shadow is a first-class mode, not a mock:

- `task.ensure` resolves the project and the task projection read-only; no worktree,
  no session, no registry write.
- `task.evaluate` runs the profile against the projection; its output is the point of
  shadow.
- `task.turn`, `task.grant`, `task.revoke` are recorded as `skipped` with reason
  `shadow`; no runtime is called and no capacity is taken.
- The run still journals the branch, the edge, and the action, so it can be compared
  with dispatch's decision for the same event.
- Shadow never blocks dispatch and never changes a task.

## 9. Dispatch parity

The builtin flow must express every behaviour below. This table is the acceptance
test for M0 and the checklist for M1-4's parity harness.

| # | Today's dispatch behaviour | Model element |
| --- | --- | --- |
| 1 | webhook entry locks, body cap, content type, replay window, `202` | upstream webhook ingress; unchanged |
| 2 | YouTrack decode, known event, idempotency, timestamp facts | upstream adapter; event envelope + `dedupe_key`; event log |
| 3 | `rules.Decide`: ignore / analyze / plan / execute / permit / revoke | `task.evaluate` (profile) + `branch` |
| 4 | trigger phrase, start states, author allowlist, self markers | upstream profile |
| 5 | `(source, project)` → repo, worktrees, runtime set, no guessing | project binding + `task.ensure` placement |
| 6 | one task, one worktree, one session, one runtime for life | task service |
| 7 | phase, agent, model and deadline precedence | `task.turn` config + profile + runtime declaration |
| 8 | permission arbiter and refusals | `task.turn` → `refusals`; the runtime still owns the arbiter |
| 9 | `/opencode permit`, grant lifetime | `task.grant` / `task.revoke` |
| 10 | nudge on a silent finish; a plan turn must read the discussion | `task.turn.require_reply` + profile `PlanRead`; one nudge, then fail |
| 11 | reply tools, marker, last-reply probe | profile reply contract + task `last_reply` + `task.evaluate` |
| 12 | pause file, cost cap, max turns | engine pause + task limits |
| 13 | audit, `/healthz`, logs | event log + run journal + task store + health |
| 14 | timeout ≠ failure; interrupted work | step/run states + recovery |
| 15 | same-task events serialize | task lease |

`testdata/flows/parity-cases.json` turns a representative subset into data: a case is
`{name, event, task, expected}`, where `event` names a fixture, `task` is the
projection (`plan`, `turns`, `last_reply`, `refusals`) or `null`, and `expected` is
`{action, reason_contains}`. The harness compares the flow's `task.evaluate` with
`rules.Decide`. A case with `compare: "routing"` uses the routing comparator instead,
because dispatch refuses an unmapped project before `Decide` runs.

## 10. API

### 10.1 M1 endpoints

All under `/hub/v1` on the main listener, all requiring
`Authorization: Bearer $FLOWHUB_HUB_TOKEN`:

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/hub/v1/flows` | summaries: id, name, builtin, mode, revision, last run |
| GET | `/hub/v1/flows/{id}` | definition + revision + validation |
| GET | `/hub/v1/runs?flow=&state=&limit=&cursor=` | run summaries |
| GET | `/hub/v1/runs/{id}` | run snapshot: steps, tasks, outputs, error |
| GET | `/hub/v1/runs/{id}/events?after=<seq>` | journal events for polling |

M2 adds `POST /hub/v1/flows/{id}/deployment` (`off|shadow|on`, project scope) and
`POST /hub/v1/runs/{id}/cancel`. `on` answers `409` when the service was started
without the task service. A cancel stops an active run: the run becomes `canceled`,
its running step becomes `interrupted` with `canceled by operator`, and the journal
gets `run.canceled`; a terminal run (or a run not active in this process) answers
`409`. A canceled turn is not retried, and the task row keeps the interruption.
There is no `PUT`/`DELETE` for the builtin flow.

- Errors are JSON: `{"error": "...", "details": [{"path": "...", "message": "..."}]}`
  with `400`/`401`/`404`/`409`/`503`. Unlike the webhook entry, the caller is a
  program or an operator and gets the real reason.
- No CORS, no SSE. The desktop polls with `after`; the main listener's write timeout
  is 10 s, so a long-lived stream does not belong here.
- `/healthz` gains `flows` (`enabled`, `mode`, `definitions`, `runs`, `waiting`,
  `needs_attention`, `shadow_compared`, `shadow_mismatches`, `pending`,
  `concurrency`, `active_tasks`, `pending_tasks`) and `upstreams`.
- Run summaries carry `compared`/`match`/`differences`; a run snapshot carries the
  joined `comparison` (dispatch and flow action/reason). Comparisons are joined by
  the event id and persisted in `<DataDir>/flow-compare.jsonl`.

### 10.2 CLI

```
flowhub flow list
flowhub flow show <id>
flowhub flow mode <id> off|shadow        # M1
flowhub flow mode <id> on --project TEST # M2
```

The CLI writes `<DataDir>/flows/state.json` atomically and records `updated_by` from
`FLOWHUB_OPERATOR` or the OS user. It does not need the API token; it runs on the
service host. The API is for the desktop and for a remote operator.

## 11. Security

- **Token.** If `FLOWHUB_HUB_TOKEN` is unset, `/hub/v1` answers `503` with the
  configuration hint; there is no unauthenticated write surface. The token is in the
  0600 env file, masked by `-print-config`, and never logged. It is not the control
  API's admin token and cannot invite/remove/rotate a runtime.
- **Desktop.** Keychain first; a loopback Hub may read the local `.env` instead of
  prompting. `instances.json` continues to hold no secret.
- **Event payload is untrusted.** It is passed to a prompt inside a delimited
  untrusted block with the existing warning; a flow's prompt can add instructions but
  cannot remove that block. Expressions read data, never execute it.
- **Secrets** are referenced by name and resolved at the call site; they are not in
  the run context, the journal, the UI or the logs.
- **Grants** are limited to recorded refusals; the agent posts as the human, so the
  self-marker, last-reply probe and anchored phrase still gate permit/revoke.
- **Deployment** records an actor; a mode change is audited with the previous mode.
- **Exposure.** `/hub/v1` is on the main listener but nginx proxies only `/hooks/`.
  Bind to loopback or WireGuard; if the listener is wildcard, the token is the
  boundary, not the bind.

## 12. Hub Flows page (read-only, M1)

The Hub tab gains a sub-navigation; no new tab kind, no new webview.

```
Hub tab
├── header: name, URL, health, refresh (existing)
├── sub-nav: Dashboard | Flows
├── Dashboard: existing cards + a Flows card
└── Flows
    ├── flow header: name, builtin, mode, revision, trigger, updated
    ├── main: SVG graph + node inspector
    └── bottom: run list + selected run's step chain
```

```
┌ Hub · flowhub@office ──────────────────────────────────────────────┐
│ ● Connected · updated 3s ago                            [Refresh]  │
│   Dashboard    │    Flows                                          │
├────────────────────────────────────────────────────────────────────┤
│ Default YouTrack pipeline  [builtin] [shadow]  rev 4f2a1c          │
│ YouTrack · all events · shadow: decisions only, no agent turns      │
├──────────────────────────────────────────────┬─────────────────────┤
│   ┌───────┐  ┌────────┐  ┌──────────┐        │ Evaluate event      │
│   │ Start │─▶│ Ensure │─▶│ Evaluate │──┐     │ task.evaluate       │
│   └───────┘  └────────┘  └──────────┘  │     │ succeeded · 3 ms    │
│                                        ▼     │ action: execute     │
│                              ┌────────────┐  │ reason: comment     │
│   Analyze / Plan / Execute ◀─│   Branch   │  │ output {…}          │
│                              └────────────┘  │                     │
├──────────────────────────────────────────────┴─────────────────────┤
│ Runs  [All ▾]  42 runs · updated 3s ago                            │
│ ▸ #42 succeeded 2m  comment.added  execute  TEST-12                │
│   ensure ✓ 2ms → evaluate ✓ 3ms → branch ✓ → execute ✓ 22s → end ✓ │
└────────────────────────────────────────────────────────────────────┘
```

- Graph: read-only SVG, auto-layout, fit-to-width, vertical scroll; no drag, no
  zoom; node click selects, Esc clears. Status comes from the selected run (latest by
  default); untaken edges are dimmed; a shadow run shows `task.turn` as skipped.
- Runs: state, time, trigger kind, action, duration, task key; selecting a run
  overlays it and expands the step chain; a step selects its node.
- Inspector: flow overview by default; node config, status, timing, output JSON,
  refusals, task/session on selection. No action buttons.
- States: no token / 401 / 503 / engine off / flow off / shadow / no runs / running /
  failed / stale. Keep the last good data and show its age, like the dashboard.
- Compile chain: `definition -> validate -> layout -> SVG`; the run overlay is a
  separate input. The renderer has a `view | edit` mode; M1 only uses `view`. Node
  ids and edge keys are stable and ports are reserved, so the later editor changes
  the definition and re-runs the chain instead of rewriting the renderer.
- Polling: overview every 5 s while the sub-page is visible; selected run every 2 s
  while non-terminal; stop on hide or terminal.

The M1-6 slice implements this page against the contract above: `desktop/src/flows.ts`
owns the read-only layout/renderer, `desktop/src-tauri/src/hub_api.rs` the API
client, and `token_store.rs` the Keychain token. Custom-flow editing still belongs
to M4.

## 13. Migration

| Stage | What changes | Gate |
| --- | --- | --- |
| M0 | This model, ADR, fixture; no code | parity table has no gaps |
| M1 | upstream/event log, runner, engine, shadow, API, read-only page | shadow runs on real events; zero agent calls |
| M2 | event durability, per-project deployment, shadow diff, E2E validation | E2E suite repeated; accelerated 1000-event soak; no wall-clock pilot (ADR 0014) |
| M3 | intake cutover, dispatch thin adapter, deletion, post-cutover observation | no dispatch turn; ordinary-use logs reviewed; `make test`/`smoke` |
| M4 | custom flows, editor, reserved nodes | #91 |

Cutover gate, in full (ADR 0014 waives the one-week pilot):

- E2E-based validation: the full suite repeated at least three times, an
  accelerated 1000-event soak, repeated live dispatch/`on` journeys, crash during
  execute/on and a rollback drill;
- 100% decision parity where the comparison is available (`action`, ignore reason,
  routing); every difference explained in #92;
- after cutover, ordinary use on the test project is observed from `audit.jsonl`,
  `flow-compare.jsonl`, `/healthz.flows`, the registry and comments: no lost events,
  no duplicate turns, task/session continuity, comment behaviour unchanged;
- rollback is the project's deployment state back to `dispatch`, with the flow's run
  records kept;
- then all projects.

The formal one-week pilot and the 100-real-event calendar gate are explicitly
waived by ADR 0014; the project does not claim those gates were met.

"Dispatch dies" means the package and its hard-coded pipeline die. The registry, the
workspace provider, the runtime adapters, the permission arbiter and the grant model
survive behind the task service; they are the reason the cutover is safe.

## 14. Invariants

1. An event is appended once per `(upstream, dedupe_key)`; a duplicate is absorbed.
2. A run is unique per `(flow, trigger, event_id)`; a duplicate returns the existing run.
3. A task has one worktree, one session and one runtime for life; it is never re-homed.
4. Task operations are serialized by the lease; no two turns for one task overlap.
5. A shadow run never creates a worktree or a session and never calls a runtime.
6. A step that started and has no terminal event on replay becomes `interrupted`; an
   agent turn is never retried automatically.
7. An unknown expression path fails the step; it never becomes empty.
8. A grant contains only literal commands recorded as refusals for the task's most
   recent turn, and records its actor.
9. A run uses the definition snapshot it started with; editing a flow never changes
   an in-flight run.
10. `/hub/v1` requires the token; no secret enters the context, journal, UI or logs.
11. The builtin flow is immutable; deployment (`off | shadow | on`) is separate state.
12. The flow engine imports no runtime or workspace implementation; `main` wires them.

## 15. Future

#91 tracks custom flows, the editor, the reserved node types, poll ingress, parallel
branches and multi-user actor tokens. They are outside this round's definition of
done. The model reserves their names and shapes so adding them is an implementation,
not a rewrite.

## 16. References

- [ADR 0013](adr/0013-flow-orchestration.md) — the decision record.
- [ADR 0014](adr/0014-e2e-based-validation-and-pilot-waiver.md) — E2E-based
  validation and the operator's pilot waiver.
- Epic #92 — delivery plan and status.
- #91 — deferred surface.
- `testdata/flows/default-youtrack.json` — the builtin flow.
- `testdata/flows/events/` — event fixtures.
- `testdata/flows/parity-cases.json` — the decision table for the parity harness.
- `internal/registry/registry.go` — the task store the model keeps.
- `internal/source/youtrack/` — the adapter the upstream profile grows from.
- `internal/dispatch/dispatch.go` — the pipeline being replaced.
