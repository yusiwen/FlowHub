# ADR 0007 — A human authorises the commands a turn was refused for

**Status:** Accepted, **implemented** (2026-10-09), in the same change: the permit and
revoke phrases and the two actions (`internal/rules`), the refusal set and the grant on
the task row (`internal/registry`), the granted-command exception in the shell policy
(`internal/agent.Granted`, `internal/agent/opencode`), the grant/revoke handling and the
continuation turn (`internal/dispatch`), the "Authorised commands" section of the prompt
(`internal/source/youtrack`), the agent-definition rule, and the README, the operations
manual, `AGENTS.md` and `CODEBASE.md`.

## Context

The shell allowlist refuses any command whose shape it does not recognise, and that is
the design: an unattended turn must not be able to run whatever it likes. The refusal is
reported (rule 16: a silent turn is a failure) and it carries a reason the model can act
on. What was missing is the other end of that loop.

The human reading the comment is the one person who can judge whether the refused
command was legitimate. Until this record, their only options were to change the host's
configuration (`shell_wrappers`, or the allowlist itself) and restart FlowHub — a change
that then applies to **every** task on that host — or to do the work themselves. Measured
2026-10-09: an agent blocked on `mvn -q verify` had no path forward that did not involve
editing a file on the server for a one-off need.

The design documents already assume a human in the permission loop (`ask` → handoff →
reply in `opencode-headless-automation-and-permissions.md`). FlowHub routes that handoff
through the tracker, because that is where the maintainer already is and where the
refusal was reported.

## Decision

**`/opencode permit` authorises exactly the commands the task's most recent turn was
refused for, and continues the task in the same session.**

* **The scope is the system's recorded refusal set, never the human's typing.** The
  dispatcher already records every permission decision with its exact command. A bare
  permit grants those literal strings — the ones the human just read in the comment — so
  no prose is parsed and nothing is inferred. A permit with an empty refusal set does
  nothing and logs why, rather than guessing.
* **The authorisation is literal.** `mvn -q verify` authorised does not authorise
  `mvn -q verify -DskipTests`; a different flag is a different command and needs its own
  permit. Narrow on purpose: the smallest thing that unblocks the task is the smallest
  thing the human has to judge.
* **It lasts until the task ends, and `/opencode revoke` ends it early.** The grant is
  written to the task's registry row (`commands`, `by`, `at`), so it survives a restart
  and a maintainer who answers hours later. It is cleared when the task reaches `done`
  (the authorisation was for work that is now finished) and by an explicit revoke.
  A second permit **adds** to the set instead of replacing it; dropping an earlier
  command would refuse the agent something a human had already approved.
* **Only the allowlist moves.** The deny list, the worktree boundary, the phase rule and
  the attachment-download exception are all consulted **before** the granted set, so a
  grant can only turn "not on the allowlist" into "allowed". `git push`, `rm`, `sudo`,
  `curl` to another host, `python3 -c …`, a path outside the worktree, output
  redirection and command substitution stay refused no matter what a human permits —
  and a read-only turn does not become writable because a command was authorised.
* **The continuation runs in the phase the refusals stopped.** The task row records the
  action of its last turn; the permit turn reuses it, so an implementation blocked by a
  refusal resumes as an implementation rather than as an analysis. A row with no recorded
  action falls back to a read-only turn, and says so in the log.
* **It is registry state, not configuration.** No new environment variable and no new
  configuration key: this is per-task, per-human and short-lived, which is exactly what
  the registry is for. It is logged loudly when granted and when revoked
  ("a human authorised refused commands; the task continues", with the commands and the
  actor), and the row is what `/healthz`'s registry view already exposes.
* **The agent cannot authorise itself.** It posts as the same tracker user as the human,
  so all three reply-recognition layers cover the permit phrase: the
  `<!-- flowhub-auto -->` marker, the probe against the task's recorded last reply, and
  the anchored match (`^\s*/opencode permit\b`, so a phrase *inside* prose or inside the
  blockquote sign-off does not act). The actor must also pass the project's author
  allowlist, exactly as `/opencode start` must.
* **The permit is answered before the turn budget.** A revoke must never be blocked, and
  a permit is a human answering a refusal they have just read — the opposite of the
  runaway loop the budget exists to stop. Both are still gated on their own reasons: a
  permit with nothing refused is ignored with a stated reason.

## Alternatives rejected

* **`/opencode permit <command>` — the human types the command.** More flexible and one
  more chance to get it wrong: the string would no longer be anchored to what the agent
  was actually refused, so the human could authorise something they never saw refused,
  and the audit would no longer be comparable with the comment they were answering. The
  system-side set is the one both parties already share. (Trailing words after the
  keyword *are* accepted — "permit, go ahead" — and cannot widen anything, because the
  set never comes from the human's text.)
* **Lifting the deny list too.** It reads as the obvious meaning of "permit", and it
  would make `git push` a matter of one comment. The deny list is the set of things no
  unattended turn may do; changing it is a configuration decision on the host, made by
  someone who has looked at the machine, not a per-task exception granted from a web
  form.
* **A configuration key for the phrases.** `FLOWHUB_TRIGGER` is configurable because it
  is the human's main verb and a deployment may rename it. These two are structural: the
  agent's own comment names them, so a renamed phrase would have to be re-explained to
  the model through another prompt field, and every deployment would have a different
  keyword for the same thing.
* **A time-limited grant.** Attractive, and it invents a number nobody can justify — long
  enough for a slow task, short enough to matter. "Until the task ends, revocable" is a
  boundary an operator can reason about without a clock.
* **One turn only.** Also defensible, and it fails the common case: an implementation
  that needs `mvn` needs it for every later verification step, and re-permitting each one
  is friction that pushes operators back to editing the host config — the thing this
  decision exists to avoid.
* **Storing grants outside the registry** (a separate file, or the control-plane
  inventory). The registry is already the per-task record with the index, the durability
  and the audit trail this needs; a second store would need its own locking and its own
  cleanup for no gain.
* **Auto-permitting after N refusals, or letting the agent ask for permission inline.**
  Both remove the human from the only step that requires judgement. The model may *ask*
  in its comment (and does, because the prompt tells it to), but the authorisation is a
  human's comment or it does not exist.

## Consequences

* **The narrow scope is the visible cost.** An agent that retries with an extra flag is
  refused again and needs a second permit. That is the honest reading of "authorise
  exactly what you read", and the alternative — approving a command *shape* — is the
  allowlist pattern language this decision deliberately avoids introducing.
* **`replied` and the refusal set are on the same row, and both are load-bearing.** The
  set is replaced every turn (the authorisation is about the turn the human just read)
  while the grant accumulates, so the two must not be confused: `refusals` is "what the
  last turn was stopped by", `grant` is "what a human has authorised so far".
* **The trust boundary moves to "who can comment on the issue".** The author allowlist
  is the only gate on a permit, exactly as it is on `/opencode start`. A project that
  declares no `authors` therefore accepts a permit from anyone who can comment — which
  was already true of the trigger, and is now a stronger statement because a permit
  authorises command execution. The operations manual says so in the deployment
  checklist; a future change could require an explicit allowlist before dispatch is
  allowed at all.
* **A granted command is a policy exception living in a data file.** The registry is
  append-only and replayed on load, so a grant is visible in `jq` output, in the log line
  that created it, and in the row itself. There is no silent grant path: `grantPermit`
  logs, `revokeGrant` logs, and a permit that adds nothing logs that too.
* **One more keyword for a maintainer to know.** It reaches them from three places: the
  prompt's reply section names it whenever a refusal is what the reply is about, the
  agent definition explains it, and §8.7 of the operations manual documents it — because
  a keyword nobody knows about is a feature that does not exist.
