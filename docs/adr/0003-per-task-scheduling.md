# ADR 0003 — Per-task scheduling, and what `max_concurrent` may mean

**Status:** Proposed, with the implementation written in the same change (2026-09-27).
The body below is the design as it was written **before** the code; the header records
what actually landed and every departure from the sketch.

## Context

One runtime runs one turn at a time. That is not an accident of the scheduler but the
whole reason there is a scheduler: a prompt sent to a session that is already busy is
silently swallowed (opencode issue #46842), so something must serialize the prompts.
ADR 0001 step 5 chose the *runtime* as the unit, and drew the consequence explicitly:
`max_concurrent` exists in the configuration shape but is refused unless it is `1`.

The approximation is coarse. A session is never prompted twice at once — that is safe.
A runtime with eight free cores and two tasks in unrelated repositories also runs one
turn at a time, which is not a safety property but a throughput accident: the two turns
share nothing. Because the runtime is the only unit, the code obtains a per-*task*
guarantee by holding a per-*runtime* lock, and every future change has to keep working
around that.

Three facts in the tree decide how much work "raise `max_concurrent`" really is. They
are listed because each one is a place where two turns for one task at once would do
real damage, and because two of them are already latent today:

1. **A turn is a read-modify-write of the task row.** `runTurn` reads `registry.Task`
   before the turn and computes `state`, `plan`, `turns`, `cost` and `last_reply` from
   that snapshot, then writes the result with `Registry.Update`. `Update` serializes
   the *file write*, not the compute, so two concurrent turns for one task interleave:
   `turns` counts one of them, and the `state`/`plan` of whichever finished last wins
   even if it started first. `rules.Decide` also reads `Turns`, `Plan` and `LastReply`,
   so the second turn would be judged against a row its sibling is about to replace.

2. **Task creation is a check-then-create.** `ensureTask` does `Registry.Get` →
   `Ensure`, and `Ensure` is `Get` (inside its own lock) followed by `Put`: two
   concurrent callers both see "unknown" and both create. The worktree creation behind
   it is serialized per *repository* by `prepareLock`, not per task, because
   `git worktree add` is the shared resource there.

3. **Routing is decided per delivery, not per task.** `routeOne` resolves a runtime
   from the registry row, and for a task that has no row yet it calls `pickRuntime`
   again for every delivery in the same burst. Two deliveries that arrive together can
   therefore be handed to two different runtimes, and the "refusing to move a bound
   task" check in `runRouted` runs **before** `ensureTask` — so it is a race that is
   usually won, not a proof. This one is reachable today with two hosts configured:
   the first delivery writes the row on host A while the second, already past the
   check, prepares its own worktree and starts its own session on host B.

Those three are why `max_concurrent` cannot simply become `> 1`: the invariant that
protects the session is currently the same lock that protects the row, the worktree and
the routing decision, and loosening it loosens all four at once.

Two counters carry a related assumption. `busyPerRT` counts work *committed* to a
runtime — queued, being prepared, or running — and exists so a burst spreads across
hosts (`two deliveries that arrive together both reach the router before either has
created a task row`, `runtimes.go`). `inFlight` counts turns that are inside
`Runtime.Run`. Both are read by `rankRuntimes` and by `/healthz`.

Finally, `runtimes.Inventory.MaxConcurrent` and `runtimes.Claim.MaxConcurrent` already
exist: the admin API shape reserves a place for the host to say how much work it can
take, and nothing writes it and nothing reads it.

## Decision drivers

* **The unit that cannot be prompted twice is a session, and a task owns exactly one
  session for life** (AGENTS.md rule 9). The scheduler's unit of exclusion must be the
  task, and a task's exclusion must hold across the whole path that touches its row,
  its worktree and its session — not only around the agent call.
* **A human's delivery is never dropped in silence.** A second comment on a busy task
  must be answered after the first turn, not merged, not discarded. Coalescing
  deliveries would mean choosing which of a person's messages to ignore.
* **Fail-closed.** Raising the breadth must not be able to produce two turns for one
  task, and must not be able to move a task between runtimes.
* **No new dependency, no new process, no vendor name in the core** (ADR 0001).
* **The webhook path never blocks**, so nothing on the intake path may wait for a turn.

## Considered options

### A. N workers plus a per-task mutex — rejected

`max_concurrent` goroutines take from the existing queue and lock a per-task mutex
before running the turn. It is the smallest diff, and it is wrong in a way that shows
up under exactly the load it is meant to serve: the lock is taken *after* the delivery
has been dequeued, so a task with several deliveries queued parks workers on its mutex
and starves every other task on that runtime. The router also cannot see the depth of
that pile, because a mutex has no count. A per-task mutex map additionally leaks an
entry per task forever unless a reaper is added, which is another moving part.

### B. A per-runtime scheduler that queues deliveries per task — **chosen**

One queue per runtime, but a scheduler rather than a channel: deliveries are grouped by
task, a task with a running turn is not eligible, eligible tasks are served in arrival
order, and up to `max_concurrent` workers each serve a *different* task. Blocking never
happens on a task — it happens on the scheduler's own condition.

### C. One channel per `(runtime, task)` — rejected

The obvious shape, and the reason it loses is that it is not simpler: a worker cannot
`select` over an unbounded set of channels, so a dispatcher goroutine plus a registry
of channels is needed anyway, and "how much work is waiting for this host" stops being
one number the router can read — which is what `spread` ranks by.

### D. More runtime names instead of more turns per runtime — not an alternative

`spread` already uses a second host. This ADR is about the case where there is one, or
where the second is already busy. Recorded so that "why not just add hosts" has an
answer: because a single host is a legitimate deployment, and because the *local*
workspace provider makes two names on one machine a real configuration.

## Decision

### 1. The serialization unit is `(source, task key)`

A task is the thing that owns a session, a worktree and a row, and it is never in two
turns at once. The runtime stops being a lock and becomes a *capacity*: how many
distinct tasks it may serve at the same time.

### 2. A runtime's work is a scheduler, not a channel

`internal/dispatch/schedule.go` holds one `runtimeScheduler` per runtime name. Its
state is three sets under one mutex:

* `pending map[taskKey][]routed` — deliveries waiting for their task's previous turn;
* `ready []taskKey` — tasks with pending work and no turn running, in arrival order;
* `running map[taskKey]bool` — tasks a worker is serving right now.

The invariant, stated so that it can be asserted in a test: **a task key is in `ready`
at most once, in `running` at most once, and never in both; a running task may also have
pending deliveries.** A worker takes the head delivery of the first ready task, marks it
running, and on completion either re-lists the task (pending work remains) or forgets
it. Nothing holds a lock across the turn: the scheduler's own mutex is held only for the
duration of a map/slice operation, and the "one turn per task" exclusion is the state
machine, not a held mutex — so a worker never blocks on another task's turn.

Two details are load-bearing:

* **Waking is a token, and the consumer always re-checks.** `next` loops: look for a
  ready task, and only if there is none wait on a one-slot `wake` channel. A signal is
  a non-blocking send, so a signal may be lost when the slot is full — which is safe,
  because the slot being full means a token exists, whoever consumes it re-checks after
  consuming it, and the work that was signalled was added before the drop. A lost token
  therefore cannot strand work; the alternative (a condition variable) would need the
  signalling path to hold the lock the turn path wants.
* **A ready task is reported at once, and only once.** `busy` (see §5) must include
  work that has been accepted but not started, or the burst-spread behaviour that
  `busyPerRT` was added for regresses.

### 3. Routing is decided once per task, and remembered until its work drains

The dispatcher keeps `routes map[taskKey]runtimeBinding`, written by the intake loop
when it resolves a runtime for a task that has none, and cleared when the scheduler
reports that task's work has drained. A second delivery in the same burst reuses the
decision instead of ranking again, which removes hazard 3 above at the only place it
can be removed: intake is single-threaded, so the decision is made once and every
delivery of that task reaches the same runtime's scheduler.

Clearing on drain is what keeps the memo a cache rather than a second registry: once
nothing is queued or running, the next delivery re-reads the registry, which by then
carries the binding the first turn wrote. A task whose turns all failed before writing
a row is simply ranked again, which is the same freedom it had before any turn ran.

The registry check in `runRouted` stays, as defence in depth and as the reason a task
whose binding changed in the inventory is still refused rather than re-homed.

### 4. Admission keeps the existing bound, per runtime

A runtime accepts deliveries while its total pending count is below the existing queue
size (`FLOWHUB_DISPATCH_QUEUE`/`Options.QueueSize`); past it the delivery is dropped and
counted exactly as today. The bound stays per *runtime* rather than per task: a
per-task bound would let one task's flood sit inside a host's memory indefinitely,
which is the failure the bound exists to prevent. Dropping is still the last resort and
still loud in the log.

### 5. Counting and ranking

* `queued` — deliveries accepted and not yet started (ready heads plus pending).
* `running` — tasks a worker is serving (rules decision, preparation, turn).
* `in_flight` — turns inside `Runtime.Run`, unchanged in meaning.
* `busy` — `queued + running`: the number `spread` ranks by, unchanged in meaning.
* `max_concurrent` — the breadth the scheduler was started with.

`spread` sorts by `(has a free slot, busy, active tasks, name)`; the new first key means
a host with an idle worker is preferred over one that is merely less loaded, and when
every candidate is full the least busy is still used — capacity changes the *order*,
never the answer "no runtime can take this". `first-healthy` keeps the declared order
untouched, including queueing on a full first choice, because that order is an
operator's failover preference and not a load-balancing one.

### 6. `max_concurrent` is the operator's breadth; the host may only lower it

* In the file: `runtimes.<name>.max_concurrent`, an integer in `1..64`, absent means
  `1`. The upper bound is not a capacity model; it is there so that a typo (`1000`) is
  a startup error instead of a thousand goroutines and a thousand agent turns.
* The host's claim (`Claim.MaxConcurrent`, filled by `init` from the host's CPU count)
  is a **ceiling**: the effective breadth is `min(file, claim)`, and an absent claim
  means "the host makes no claim". Cores are the contended resource because an
  unattended turn may run a build, and the direction is deliberately the safe one: a
  host can say "not that much", a file cannot make a host take more than it claims.
* The breadth is read when a runtime's scheduler is first used and does not change while
  the process runs. A re-enrolment that lowers the ceiling, or an edited file, applies
  after a restart. Shrinking a running pool means deciding what to do with the turns it
  already started; that decision is not worth making today.

### 7. A turn announces itself

`runTurn` logs `turn started` with the runtime, task, phase, session and breadth before
calling the runtime, so overlap is observable in the log rather than inferred from two
completion lines. It is also the line that tells an operator the scheduler is doing what
`max_concurrent` asked for.

## Consequences

* `max_concurrent: 4` on one host means four *tasks*, never four prompts to one session.
  A task with ten queued deliveries still runs ten turns, one after another.
* Two tasks in the same repository prepare their worktrees through the existing
  per-repository `prepareLock`, so the new parallelism does not touch the clone's git
  bookkeeping.
* The scheduler is per runtime *name*, so two names pointing at one server each get
  their own breadth — which is what the local-provider deployment already looks like,
  and is worth knowing when the two names are the same machine.
* `rankRuntimes` now depends on the schedulers, so the counters are read from one place
  (`schedMu` → scheduler mutex is the only lock order; the reverse is never taken).
* The verdict "raising `max_concurrent` is configuration, not a redesign" from ADR 0001
  becomes true, with the caveat in §6 about when a change takes effect.

## Non-goals

* **Priority or fairness between tasks.** Order is arrival order per runtime, and a task
  that keeps receiving deliveries keeps its place. Preemption, per-project weights and
  quotas are not in this step.
* **A distributed lock.** One FlowHub process owns a task. Two processes pointed at the
  same tracker are already unsafe today (two registries, two sessions) and stay out of
  scope; the admin API's runtime inventory is not a lease.
* **Cancelling a running turn.** Breadth makes a cancelled turn more valuable, not less,
  but nothing here stops one.
* **Using the host claim as the *default* breadth.** An enrolment that silently raised
  concurrency would be a behaviour change on upgrade; the file stays authoritative.

## Migration plan

Nothing in the file format changes: `max_concurrent: 1` means exactly what it means
today, and every existing deployment keeps its behaviour with one worker per runtime.
The work is one commit's worth, in order:

1. `internal/dispatch/schedule.go`: the scheduler, its state machine and its snapshot.
2. `internal/dispatch`: intake-level routing memo, `enqueue` through the scheduler,
   `workRuntime` → `workScheduled`, counters and ranking from the scheduler, the
   `turn started` line.
3. `internal/projectmap`: accept `1..64`, refuse `0`, negatives and `> 64` with the
   reason; carry the value into `DeclaredRuntime`.
4. `internal/provision`: claim the host's CPU count; `runtimes` already stores it.
5. `internal/dispatch`: effective breadth = `min(file, claim)`, default `1`.
6. `/healthz`, `README.md`, `AGENTS.md` rule 9, `config.example.json`,
   `CODEBASE.md`, and the ADR 0001 step 5 row.

### Acceptance criteria

| Claim | How it is checked |
| --- | --- |
| Two deliveries for one task never run at once, and the second is not lost | unit test on the scheduler with a blocking turn: the second delivery starts only after the first finishes, and both turns run |
| Different tasks run in parallel up to the breadth | unit test: three tasks, breadth two, peak observed concurrency is two and never more |
| A fresh task cannot be routed to two runtimes | dispatch test with two fake runtimes and two simultaneous deliveries of an unknown task: one session, one worktree, one runtime, two turns |
| A full runtime is ranked after one with a free slot; an all-full set still answers | ranking test with three runtimes and a fixed set of scheduler states |
| A `max_concurrent` outside `1..64` is refused at load with the reason | table test on the configuration file |
| The host's claim lowers, never raises, the effective breadth | unit test on the resolver, with claim present, absent and lower |
| `/healthz` reports the new counters | dispatch snapshot test |
| Two real turns overlap on one host | live run: two issues, one runtime, breadth two, two interleaved `turn started` lines and `in_flight=2` observed while both run |

## Open questions for review

1. **Should the effective breadth be per runtime or per project?** A host may be shared
   by projects with very different appetites. Per runtime is what the file already
   says, and per project would need a second knob and a resolution order; recorded
   rather than adopted.
2. **Is arrival order the right fairness rule?** One task with a fast stream of
   deliveries keeps its turn at the front of `ready`. A round-robin over tasks inside a
   runtime would be a small change to §2 and is deliberately not made speculatively.
3. **Should the claim ceiling exist at all?** It closes a field that the admin API
   already serves, at the cost of a second input to the breadth. The alternative is to
   delete `Claim.MaxConcurrent` and let the operator's file be the only answer; if the
   ceiling turns out to be noise in practice, that is the follow-up.
4. **Does a busy scheduler belong in the routing decision at all?** Today the router
   probes health but not capacity, and relies on the queue. Refusing to route to a
   runtime that is full — rather than queueing — would bound wait time and lose work;
   not adopted.
