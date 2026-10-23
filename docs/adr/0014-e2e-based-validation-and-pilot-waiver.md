# ADR 0014: E2E-based validation replaces the one-week pilot

- Status: Accepted
- Date: 2026-10-23
- Issues: #92 (epic), #119 (E2E harness)
- Qualifies: ADR-0013, decision 9 and the cutover gate in `docs/flow-model.md`

## Context

ADR-0013 made the one-week pilot with one real project the gate between `shadow`
and intake cutover. That gate assumes a project that is safe to put under
`on` while two people keep working in it: a bad turn edits a real worktree, posts
a real comment, and spends real model tokens.

The operator's environment does not provide that project. There are two
developers, one project, and it is not acceptable to use that project as the
pilot. The current YouTrack + opencode workflow is already working manually:
a person writes a prompt in opencode, the agent reads the issue and replies.
FlowHub changes the orchestration around that workflow, not the workflow itself.

The repo now has a real E2E harness that exercises the same process the pilot
would: real YouTrack deliveries, real opencode turns, `off`/`shadow`/`on`,
permit/revoke, crash recovery, replay, pause, limits, attachments, API auth,
failure paths, soak and burst. The harness is opt-in and does not run in CI, but
it is reproducible from the repository and does not require a production project.

Keeping the one-week pilot as a hard gate would therefore block the work on an
environment that does not exist, while the failure modes a pilot is meant to
catch are either covered by E2E or can be observed from real logs after the
change is in use.

## Decision

**1. The formal one-week pilot is waived by the operator.** M2-4 is no longer a
pre-cutover gate. It becomes **post-cutover observation**: after the flow is in
use, review `audit.jsonl`, `flow-compare.jsonl`, `/healthz.flows`, the registry and
the comments, and decide whether the flow is behaving as expected.

**2. E2E is the primary validation gate during implementation.** The gate is:

- the full E2E suite, including the opt-in failure-path targets, run at least three
  times;
- an accelerated event soak (target 1000 events), not a wall-clock soak;
- live dispatch and `on` journeys, repeated a limited number of times because they
  spend real model tokens;
- crash during execute and during an `on` run;
- rollback `on` -> `off` -> `on` with dispatch still available.

**3. No wall-clock soak is a blocking gate.** A 24h or 72h soak is optional
observation, not an acceptance requirement. The accelerated soak exercises event
volume, replay and queue behavior; a wall-clock soak would additionally test slow
leaks and day rollover, and that residual risk is accepted here.

**4. Real usage logs are observation, not a second pilot.** Once the flow is in
use, the operator reviews the logs above. This is deliberate operational
observation, not a claim that the one-week gate was satisfied.

**5. M3-1 (#112) does not wait for the pilot.** Extracting the task service out of
`internal/dispatch` can proceed while the flow is validated by E2E. The task
semantics, not the package boundary, are the compatibility contract.

**6. Rollback remains unchanged.** `flow mode off` returns a project to dispatch;
the pause file stops new work; restarting stops a stuck process. The flow run and
task records stay for inspection.

## Consequences

- `#92` must record the waiver and replace the M2-4 checkbox with post-cutover
  observation; `docs/flow-model.md` must say the one-week gate was waived and the
  replacement evidence is E2E plus real logs.
- The project must not claim that the original cutover gate was met. Future
  readers will see both ADR-0013's original gate and this qualification.
- Residual risks are explicitly accepted:
  - no production traffic distribution;
  - no week-long time dimension;
  - a limited number of real-model turns;
  - no real-project business semantics;
  - no real deployment/nginx/disk/long-lived connectivity test.
- E2E remains a repository artifact: every claim about validation can be rerun
  with a make target. It does not become a CI job because it needs the self-hosted
  YouTrack and a local opencode.
- If the operator later needs a stricter gate (for a larger team, a production
  project, or a regulated change), a new ADR can reinstate the pilot. This ADR
  does not amend ADR-0013's task semantics; it only qualifies its migration gate.

## Alternatives considered

- **Wait for a safe real project.** Rejected: none is available, and the wait has
  no bounded end.
- **Use the two-person project anyway.** Rejected: a bad turn can edit a real
  worktree and post a real comment; the operator explicitly does not accept that
  risk for a test.
- **Run a 24h/72h wall-clock soak instead.** Rejected as a blocking gate: it adds
  elapsed time without adding real traffic, and the operator can observe time-based
  behavior in ordinary use. It remains available as an optional check.
- **Quietly change #92 and the flow model.** Rejected: the pilot is written into
  ADR-0013 and the cutover gate; a silent edit would leave two conflicting
  acceptance records.
- **Call E2E a pilot.** Rejected: E2E uses synthetic/fake failure paths and a test
  project; calling it a pilot would hide the residual risk instead of recording it.

## Follow-up

1. This ADR moves to `Accepted` after operator review.
2. Then update #92, `docs/flow-model.md` and the flow-model/test links.
3. Continue the remaining functional work, including active-on restart/aged cursor
   and #112.
4. Begin ordinary use on the test project; review the logs after real usage rather
   than waiting for a calendar.
