# ADR 0017 — A plan reply is rejected until the discussion has been read

Status: accepted (2026-10-28, issue #179)

## Context

ADR 0016 (#178) made the task service refuse to record a plan that was posted
without a completed `get_issue_comments` call. That gate runs after the turn, so it
can only fail the task and ask once for a correction: the stale plan has already
reached the issue, and the maintainer sees a plan followed by a correction.

The reply tool was still on the session's allowlist. The server therefore never
asked the arbiter for it, and the second permission layer had no chance to refuse
the call that matters most. A plan is the one turn where a wrong comment is
expensive: it is what the next `/opencode start` reads as approval to execute.

## Decision

1. **The reply tool is not on the session allowlist.** `AllowedMCPTools` drops
   `youtrack_add_issue_comment`, so the ruleset's catch-all `ask` applies and the
   server asks the arbiter. `ReplyTools` still names it for the executor and the
   arbiter; the reply is a deliverable, not a tool that needs an allowlist entry.

2. **The turn carries two generic lists.** `agent.Turn.ReplyTools` names the reply
   calls; `agent.Turn.RequireReadBeforeReply` names the tools that must have
   completed in this turn first. The executor sets `ReplyTools` always and
   `RequireReadBeforeReply` from `ToolPolicy.PlanRead` only on a plan turn. Both
   travel through `runner.Turn` into the runtime; no tracker name enters the core.

3. **The arbiter decides the reply.** `opencode.Arbiter` gets `ReplyTools` and
   `RequireReadBeforeReply`; `ObserveTools` records the completed tool calls of the
   turn. A reply request is allowed when there is no read requirement or one of the
   required reads has completed in this turn; otherwise it is rejected with a reason
   that names the read tool, so the model can read and retry. A failed read does not
   count, and a read from an earlier turn does not count: the plan must be based on
   the live thread.

4. **The runner records tools before answering permissions.** In both the v1 and
   the v2 loop, the latest messages are fetched and `ObserveTools` runs before
   `answerPending`. Without that order the reply request could be answered against
   a snapshot that predates the read that just completed, and a compliant turn would
   be refused.

5. **ADR 0016's post-turn gate stays.** It is the backstop for sessions created
   before this ruleset change (a task is bound to its session for life, so an old
   session keeps its old allowlist) and for any reply call the runtime does not
   surface as a permission request. It also keeps the fail-closed `plan_state=none`
   semantics.

6. **The E2E fake reports the read.** A fake that posted the reply without a read
   would now fail its own plan turn; the fake adds the same completed read a real
   turn makes.

## Consequences

- Every reply is gated once. Analysis and execute replies are allowed immediately
  by the arbiter (no read requirement); a plan reply is refused until the read.
  The cost is one permission round trip and one `permission answered` log line per
  reply, which is also the audit record that the reply was allowed by policy.
- A plan without the read never reaches the issue: the model sees the refusal,
  reads, and retries. The Phase 1 nudge remains for a model that stops after the
  refusal, and for sessions created before this change.
- The reply tool's absence from the allowlist is the only ruleset change; the
  session's `Allowed` list is otherwise unchanged. A source without a reply tool
  sets no `ReplyTools` and is unaffected.
- The arbiter's completed-tool set is per turn (a new arbiter is built for each
  runtime `Run`), so a read in a prior turn cannot satisfy a later plan.
- The decision is source-neutral: the runtime knows lists of names, not YouTrack.

## Alternatives rejected

- **Phase 1 only.** Measured to leave the stale plan on the issue and require a
  second comment; the maintainer reads the wrong plan first.
- **A session ruleset `deny` for the reply.** It would remove the tool rather than
  gate it, and the model could neither post the plan after reading nor post the
  analysis/execute comment.
- **Hard-coding the YouTrack tool names in the arbiter.** The core is meant to name
  no vendor (ADR 0001); the source declares its names as data.
- **Checking the reply after it runs.** The tool has already posted the comment;
  this is the failure ADR 0016 exists to bound, and it cannot prevent the first
  comment.
- **A separate permission round trip only in the executor.** The executor runs after
  the turn; it cannot refuse a tool call.

## Evidence

- `internal/agent/opencode/arbiter_test.go`:
  `TestAPlanReplyNeedsTheDiscussionReadFirst` — refused before the read, a failed
  read does not count, a completed dotted v2 read allows, and a turn without a read
  requirement allows.
- `internal/agent/opencode/runner_test.go` (v1) and `runner_v2_test.go` (v2):
  `TestV1ReplyPermissionWaitsForTheDiscussionRead` and
  `TestV2ReplyPermissionWaitsForTheDiscussionRead` — the runner observes the read
  before answering the reply request, and the server sees `reject`/`once`.
- The existing Phase 1 tests still pass unchanged; the E2E fake now reports the read.
