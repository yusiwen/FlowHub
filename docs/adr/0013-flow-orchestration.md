# ADR 0013: Flow is the orchestration layer

- Status: accepted
- Date: 2026-10-21
- Issues: #92 (epic), #93 (model)

## Context

FlowHub has one orchestration pipeline, and it is hard-coded. A YouTrack webhook is
decoded by `internal/source/youtrack`, `internal/rules` turns the event into an
action, `internal/dispatch` owns the task, and one turn runs on an agent runtime.
That pipeline is load-bearing: it holds the session registry, the worktree baseline,
the permission arbiter, the reply/nudge contract, the permit/refusal lifecycle, and
the tests that pin them. Nothing about it is expressible as data, so the Hub
dashboard can show it but the operator cannot see, version, or replace it — and the
repository is named for the flow it does not yet have.

The value that must survive is the **task semantics**, not the if/else:

- one task, one worktree, one session, one runtime for the task's life;
- a baseline pinned through the origin and never re-resolved;
- a phase that decides what a turn may change, and a shell policy the source cannot widen;
- a timeout that is not a failure, and an interrupted turn that is not retried blindly;
- refusals and grants scoped to the commands a human actually read;
- one comment per turn, with the marker and the last-reply probe that keep the loop
  from answering itself.

The goal is a flow model that can express today's pipeline exactly, then grow new
upstreams and new steps without a second orchestrator beside it. The model is
normative in `docs/flow-model.md`; the builtin flow is
`testdata/flows/default-youtrack.json`; the parity table in the model names every
dispatch behaviour the fixture must reproduce.

Constraints:

- The webhook request path stays non-blocking: the publisher has a 5 s timeout and
  no retry, so a slow handler is felt by the person editing the issue.
- The Go module stays standard-library only.
- The existing dispatch tests are the oracle during migration; a refactor that
  changes their outcome is wrong even if the new design looks better.
- No behaviour changes until a pilot project is explicitly moved to the flow.

## Decision

**1. There is one orchestration vocabulary.** An upstream adapter emits an event; the
event is appended to a log; a flow run consumes it; a run sequences **task
operations** (`ensure`, `evaluate`, `turn`, `grant`, `revoke`); the task service owns
the worktree, session, runtime affinity and grants. A graph never talks to an
opencode server or a git clone directly.

**2. The upstream adapter and the ingress are separate.** The adapter owns the
source's domain: normalisation, `dedupe_key`, trigger phrase, start states, authors,
self markers, prompt/tool policy and `evaluate(event, task)`. The ingress owns how an
event arrives: `webhook` first, `poll` later. One adapter may gain both; both must
share one dedupe namespace so a webhook and a poll of the same event collapse. The
`Poller` interface and a persisted cursor are part of the model; no poll ingress is
implemented in this round.

**3. The event log is the durable input.** Every accepted delivery is appended to
`<DataDir>/events/YYYY-MM-DD.jsonl` with the neutral envelope. Delivery is
at-least-once; a run is unique per `(flow, trigger, event_id)`; a duplicate delivery
returns the existing run instead of creating a second one. Event order is receive
order; `occurred_at` is for conditions, never for ordering.

**4. The task is first-class, and the task service is the only owner of task state.**
`registry.jsonl` remains the store; its existing fields are the task record
(identity, plan, refusals, grant, last action), and a run references the row rather
than copying it. A task is identified by `(upstream, subject.key)` for the builtin
flow, so existing sessions and worktrees continue across the cutover with no
migration. Task operations are serialized by a lease. Placement (which runtime, which
workspace) is a task decision recorded with its inputs, not a field a flow author
edits.

**5. The builtin flow is embedded and read-only.** `default-youtrack` reproduces the
current pipeline: `task.ensure -> task.evaluate -> branch -> task.turn` (analyze,
plan, execute), `task.grant -> task.turn` (permit), or `task.revoke`. YouTrack
conventions live in the upstream profile, not in the graph and not in the engine.
Deployment is separate from definition: `off`, `shadow` (evaluate only, no agent
turns), `on`. Custom flows, cloning and editing are deliberately later (#91); the
first milestone needs parity, not an editor.

**6. A run is a journal.** `meta.json` holds the definition snapshot, the trigger
event snapshot and a folded state; `journal.jsonl` is the append-only truth. A step
that started and has no terminal event on replay becomes `interrupted` and the run
becomes `needs_attention`; agent turns are never retried automatically. A definition
edit never changes an in-flight run.

**7. Approval is event-based for the builtin flow.** In the current pipeline the human
approval is the `/opencode start` comment: a new event, evaluated against the task's
plan state. That stays the builtin flow's approval. A parked `human.approval` node is
reserved for gates that have no natural event source; it is not implemented in this
round, and the desktop gets no approval button. A permit is not an approval: it is a
grant of specific commands the last turn was refused for, recorded with an actor and
cleared when the task is done or revoked.

**8. `/hub/v1` is token-authenticated and deployment state is not definition.** The
API is on the main listener under a path nginx does not proxy, requires
`FLOWHUB_HUB_TOKEN`, and returns real errors. `off | shadow | on` (and, for `on`, the
project scope) live in `<DataDir>/flows/state.json`, so a deployment change is a
state write, not a definition edit and not a restart. The desktop stores the token in
the macOS Keychain; a loopback Hub may read the local `~/.config/flowhub/.env`
instead of prompting.

**9. The migration is staged.** Shadow first, on real events, comparing every decision
with `internal/dispatch`; then one pilot project on `on` while dispatch skips it; then
the intake moves to the flow and `internal/dispatch` becomes a thin compatibility
adapter; then the package is deleted. The registry, the workspace provider and the
runtime adapters do not die with it. The cutover gate is in the model document.

**10. The desktop Flows page is read-only first.** It renders the builtin definition
as a graph, overlays the selected run, and shows runs, steps and node outputs. The
renderer is split into `definition -> validate -> layout -> SVG` with a separate run
overlay and a `view | edit` mode, so the later editor changes the definition and
re-runs the chain instead of rewriting the renderer.

**11. Event durability changes before `on`, not now.** M1 shadow may append the event
asynchronously, exactly like today's audit/dispatch queues, and count a drop. Before
the pilot turns `on`, the append moves before the `202` (without fsync) so an accepted
event cannot be lost; that is an explicit amendment to rule 1 of the delivery
pipeline, recorded in `AGENTS.md` when it lands. Shadow does not need it; a live
cutover does.

## Consequences

- New packages: an upstream seam, a flow model/validator, a runner shared with
  dispatch, the flow engine, and the `/hub/v1` handlers. None of them import an agent
  or workspace implementation; `main` wires those.
- `AGENTS.md`, `CODEBASE.md` and `README.md` eventually describe a flow-driven
  pipeline rather than a dispatcher-driven one. The link updates are deferred while a
  parallel change owns those files; the ADR and model link the epic until then.
- `docs/adr/0009-desktop-three-views.md` said the desktop stores no token and never
  calls a write API. That remains true for the dashboard; the Flows page adds a
  scoped hub token and a deployment toggle in M2. The ADR's consequence is amended
  when the page lands, not silently.
- The event log adds a second append per accepted delivery (the audit is the first).
  Retention and permissions are the same as the audit's (`0600`, 30 days by default);
  a run keeps an event snapshot so pruning does not break run inspection.
- Shadow is a measurable migration: it can disagree, and the disagreement is the
  work. A parity failure blocks `on`; it does not get papered over.
- The builtin flow is a second description of behaviour that the dispatch tests also
  describe. The parity harness is what keeps them from drifting; when the flow owns
  the intake, the harness and the dispatch tests converge on the same assertions.
- The desktop gets a sub-page of the native Hub tab, not a new tab kind or webview.
  The page is another reason the Hub is a first-class target in the registry.

## Alternatives considered

- **A general workflow engine (Temporal, Argo, Cadence).** Rejected: an extra server,
  SDK and dependency tree for a single-binary, standard-library project; the domain is
  small, and the engine's durability model is a journal we can test in-process.
- **Keep dispatch and flow as permanent peers.** Rejected: two orchestrators mean two
  truths about sessions, routing and permissions. The shared runner is a migration
  device, not an end state.
- **Rewrite dispatch in one change.** Rejected: the task semantics are encoded in
  tests and live traffic; a big-bang rewrite would put the production pipeline at risk
  to prove a model that a shadow run can prove without it.
- **A poller per flow.** Rejected: the cursor belongs to the upstream. Per-flow pollers
  split one source's watermark into N and make webhook/poll dedupe impossible.
- **Flow-owned tasks outside the registry.** Rejected: it would create a second
  session/worktree table and lose the "one task, one session" guarantee at the cutover.
- **Parked approval for the builtin flow.** Rejected for now: it changes the
  event-driven lifecycle into long-lived waiting runs, adds resume/timeout semantics,
  and changes the human's existing comment workflow for no parity gain.
- **YAML definitions.** Rejected: a new dependency and a second spelling of the
  configuration language; the repository already uses strict JSON.
- **SQLite for runs.** Rejected for now: JSONL is greppable, replayable, crash-safe
  with a folded snapshot, and consistent with the audit and registry stores. A SQLite
  store can replace it behind the same interface if volume ever demands it.
- **Hard-coded runtime names in a flow.** Rejected: a flow declares requirements and a
  task records the placement; the same definition must run against another host.

## Future work

Custom flows, the editor, the reserved node types, poll ingress, parallel branches and
multi-user actor tokens are tracked in #91. They are explicitly outside this epic's
definition of done.
