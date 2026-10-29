# ADR 0018 — Re-planning and the open-questions guard

Status: accepted (2026-10-29, issue #181)

## Context

A plan turn recorded `plan_state=draft` whether or not the plan still needed
answers. The only start triggers (`/opencode start` and the configured state
change) treated a draft as approval and executed, and there was no verb for
"revise the plan". A human could answer the questions in a comment, but comments
are ignored by design (natural language must not trigger work), and
`/opencode analyze` does not change the plan. The result was a choice between
executing a stale plan and leaving the task stuck. Measured on BEAP_BE-74
(2026-10-29): the plan asked five questions, and a single start would have
implemented it before the answers existed.

The same review found a second gap: the plan reply gate required only
`get_issue_comments`, so the issue description and fields (and the discussion
beyond the first page) could be stale; and the execution turn had no read gate at
all, so a reply posted after the plan was not guaranteed to be in the session when
the first edit happened.

## Decision

1. **A fixed replan phrase.** `rules.ReplanTrigger = "/opencode replan"` is
   anchored and case-insensitive, like the permit and revoke phrases. It is
   answered after the self/permit/revoke and max-turns checks, before
   `/opencode start`, and returns `ActionPlan` with `TriggerReplan` **even when a
   draft exists**. `startDecision` is not used, because a draft would turn that
   into an execute. `/opencode start` keeps its meaning: no plan → plan, a ready
   draft → execute.

2. **Open questions are a plan state, not prose.** The plan prompt requires
   exactly one machine marker in the plan comment: `<!-- flowhub-plan-open -->`
   when anything remains to confirm, `<!-- flowhub-plan-ready -->` only when
   nothing does. The executor reads the marker from the completed reply tool's
   input and records `plan_state=questions` for open, missing or both markers
   (fail-closed), `draft` only for ready. `/opencode start` and the start-state
   trigger are ignored while questions remain; the answer path is
   `/opencode replan`, which runs a plan turn and can move the state to `draft`.
   A failed replan clears the plan, as a failed plan turn already did.

3. **Read the live issue, not the session snapshot.** `PlanRead` names both
   `youtrack_get_issue` and `youtrack_get_issue_comments`, and the executor and
   the arbiter require **all** of them to complete before a plan reply. The
   execution turn gets the same list as `RequireReadBeforeWork`, enforced by the
   arbiter's `edit` case before the first file edit. A new reply that arrives
   after the execution preflight read is still not seen; the human must
   intervene, which the pause file and SIGTERM already support.

4. **A replan while a turn runs is refused, not queued.** Queueing it would run a
   revision after the execution it was meant to prevent. `TaskView.State` carries
   the task state into the rules; a replan in `analyzing`, `planning` or
   `executing` is ignored with a reason to pause or wait. `awaiting_input`,
   `failed` and `done` accept a replan; a done task reopens to planning.

5. **The author allowlist stays the existing project field.** `projects[].authors`
   (inherited from `sources.<name>.authors`, empty by default) is applied by the
   dispatcher and the flow engine before the decision, so it already covers
   `/opencode start`, the new replan phrase and every other event. No new field is
   needed; the change adds the regression test and documentation.

6. **The prompt owns the wording, the executor owns the state.** The YouTrack
   prompt names both paths ("answer and `/opencode replan`" vs "`/opencode start`
   once agreed"), requires the markers, and says a revised plan supersedes the
   previous one. The executor never infers the state from prose.

## Consequences

- A plan with open questions cannot be executed by accident; the maintainer sees
  the questions, answers them, and asks for a revision. The revision is a normal
  plan turn, so the read gates and the permission loop apply unchanged.
- The plan state has one more value (`questions`). The `/hub/v1` and the desktop
  show it as the raw string; no new table or migration is needed.
- A draft written before this change has no marker and stays `draft`, so it
  remains startable. The rollout note is one `/opencode replan` per such task.
- The read gates cost one extra permission/read call and, for an execution, one
  preflight read before editing. The alternative is editing on a stale thread.

## Alternatives

- **Automatic replan on any human comment** was rejected: it reintroduces
  natural-language triggering and untrusted input as a scheduler.
- **Replan on the `In Progress` state** was rejected: the state change already
  means "start" when a draft exists, and overloading it makes an approval and a
  revision indistinguishable.
- **A `--replan` argument to the start phrase** was rejected: the start phrase is
  anchored and a separate structural verb is clearer to the model and the human.
- **A plan-questions marker without a replan verb** would detect the problem but
  leave no way to fix it.
- **Relying on the task scheduler's serialization** was rejected: it prevents two
  concurrent turns, not a stale execution. The preflight and the state guard are
  what make the thread fresh.
