# ADR 0016 — A plan turn must read the discussion before it replies

Status: accepted (2026-10-28, issue #177)

Revision 2026-10-28 (#179): Phase 2 landed as ADR 0017. The reply tool is now
gated by the session ruleset and refused by the arbiter until the plan read
completes; the post-turn gate below remains the backstop for sessions created
before the ruleset change.

## Context

On 2026-10-26 the BEAP_BE-73 run produced a plan turn that posted a plan without
reading the maintainer's answers. The plan prompt already told the agent to read the
live issue with `youtrack_get_issue` and `youtrack_get_issue_comments`; the turn made
no MCP call and reused its earlier analysis. The plan re-asked four questions the
maintainer had answered 20 minutes earlier.

The task was then recorded `plan_state=draft`. That row is what the next
`/opencode start` reads as approval to execute: a plan written against stale
discussion would have become the input of an execution turn with edit rights. The
human noticed and drove a manual correction in the same session, but nothing in the
pipeline had stopped the stale plan from being used.

Two smaller facts made the plan prompt thinner than the dispatcher's:

- the upstream conversion dropped `Subject.Body`, so a flow-driven plan prompt
  carried only key/project/summary;
- the flow passed `Trigger: "flow"` to the turn, so the sign-off's basis fell back to
  "a FlowHub automation trigger" instead of naming the state change or the comment.

A prompt-only fix was rejected: the instruction was already present and a live model
skipped it. The turn contract is the only place the project can make it true.

## Decision

1. **`source.ToolPolicy.PlanRead` declares the discussion tools.** YouTrack returns
   `youtrack_get_issue_comments`. The list is data and belongs to the source: a
   tracker without a discussion returns no tools and is unaffected.

2. **The executor gates a plan on a completed read before a completed reply.** Tool
   order is the order the runtime reported the calls in; `mergeTurns` preserves it
   across the follow-up. Reading after posting the plan does not count: the comment a
   human reads was written without it. A corrected reply after a read does count.

3. **One bounded follow-up, then failure.** When a plan turn replies without the read
   (or is silent without it), the executor runs one follow-up in the same session
   with `rules.PromptContext.NudgeRead`; the source writes the prompt. If the merged
   turn still has no reply after a read, the task is `failed` with `plan_state=none`,
   `last_error` names the missing read, and the turn returns an error so a flow run
   also fails. There is no second follow-up.

4. **A failed plan turn clears the plan state.** A draft from an earlier turn is not
   what the failed turn was asked to produce, and leaving it would let the next start
   execute the plan the failure was meant to replace.

5. **The prompt says it too.** The plan turn must call `youtrack_get_issue_comments`,
   treat the session's plan as stale, not re-ask a question the discussion answers,
   and name the newest human comment it read. The follow-up has its own header: it
   must not read as "the previous turn posted nothing" when a plan was in fact posted.

6. **The flow carries the body and the real trigger.** `upstream.Subject` keeps
   `Body`, and the engine passes the decision's trigger (`created`/`comment`/`state`)
   into the turn, so the plan prompt has the requirement text and the sign-off names
   what actually moved the work.

## Consequences

- A plan turn costs one extra `get_issue_comments` call. That is the point: it is the
  cheap read that makes the plan answer the live thread.
- A plan that skipped the read may produce one correction/confirmation comment before
  it is accepted. The alternative — posting the plan during the turn and failing
  afterwards — leaves the plan comment on the issue either way; the gate at least
  makes the second comment state what was missed.
- The failure is fail-closed: `failed` + `plan_state=none`, and the next
  `/opencode start` plans again instead of executing. The human sees the failure in
  `/hub/v1` and the desktop; the issue still carries the plan comment, so the
  correction is visible in the thread.
- A source without a discussion tool declares no `PlanRead` and keeps the old
  behaviour; the gate is not a runtime-wide policy.
- The flow run fails when the task service returns the unread-plan error, so the run
  journal and the task row agree that no plan was produced.

## Alternatives rejected

- **Prompt-only enforcement.** Measured to fail on 2026-10-26: the prompt already
  said "read the live issue ... before you decide anything".
- **An event-log comments digest as the prompt's source of truth.** The durable log
  is a partial copy: deliveries can be refused, the flow can be off, and retention is
  finite. It could be a labelled fallback, but it cannot replace the live read.
- **FlowHub fetching comments itself.** It would add a second YouTrack client with
  its own token and break the source seam (ADR 0001); the agent already owns the MCP
  surface.
- **Rejecting the reply tool before it posts (Phase 2).** It would avoid the first
  plan comment entirely, but needs the reply tool gated per turn and the arbiter to
  see the turn's action and tool history. It is a larger change; this ADR's gate is
  the smallest one that prevents execution of an unread plan.

## Evidence

- `internal/dispatch/dispatch_test.go`:
  `TestAPlanTurnThatSkippedTheDiscussionIsNudgedAndCorrected` (one follow-up, then
  `draft`) and `TestAPlanThatNeverReadsTheDiscussionIsNotAPlan` (`failed`/`none`, and
  the next trigger plans instead of executing).
- `internal/tasks/executor_test.go`: the read-before-reply order table, including the
  "corrected reply after read" case.
- `internal/source/youtrack/prompt_test.go`: the plan's mandatory read, the read
  follow-up, and the combined silent+unread follow-up.
- `internal/source/youtrack/upstream_test.go` and
  `internal/flow/engine/engine_test.go`: `Subject.Body` survives normalization and the
  neutral conversion.
- `internal/flow/engine/live_test.go`: the turn request carries the decision trigger,
  not the literal `"flow"`.
