# Real E2E harness

`e2e/` is an opt-in end-to-end suite that drives a **real** FlowHub process against
a configured YouTrack test project and the local opencode server. It is not
part of `make test` or CI: it spends real turns, writes to YouTrack, and needs
credentials.

## What it proves

- A real YouTrack webhook reaches FlowHub's `/hooks/youtrack/<key>` and becomes an
  audit record and a neutral event.
- `issueUpdated` is accepted, audited, normalised and ignored by dispatch as
  expected.
- With `E2E_TURN=1`, `issueCreated` starts a real dispatch turn:
  - the task gets a worktree and one opencode session;
  - the agent posts the marker comment to YouTrack;
  - the registry ends in `awaiting_input`, with `replied=true`.
- The session is looked up by the task's **worktree** directory, not the source
  repository: opencode binds sessions to the directory where the task lives.

## Credentials

The suite reads:

- `YOUTRACK_URL`, `YOUTRACK_PROJECT`, `YOUTRACK_TOKEN`, `E2E_REPO` and `E2E_ISSUE`
  from a `0600` file such as `/tmp/flowhub-e2e.env`;
- `FLOWHUB_HOOK_KEY`, `FLOWHUB_TOKEN` and the opencode credentials from
  `~/.config/flowhub/.env`.

Do not commit either file or print its values. The test token can be placed by a
human; the harness never writes it to the repository.

## Run

Direct ingress (test FlowHub on `127.0.0.1:18080`, payload posted by the test):

```bash
make build
make e2e
```

Live webhook (test FlowHub owns `E2E_ADDR`, the address the YouTrack app posts to):

```bash
# stop the real receiver first; the test process must bind the app's URL
make e2e-live
```

Full live operator journey (analyze, first start -> plan, second start -> execute,
then worktree/comment/registry):

```bash
make e2e-live-journey
```

Live permit/revoke (real comment webhook, refusal, permit and revoke):

```bash
make e2e-live-permit
```

Cleanup test issues created by this suite (keeps `E2E_ISSUE`; dry run with
`E2E_CLEANUP_DRY=1`):

```bash
make e2e-clean
E2E_CLEANUP_DRY=1 make e2e-clean
```

Real dispatch turn (creates a new test-project issue and spends one opencode turn):

```bash
make e2e-turn
```

Shadow/on/rollback (one shadow comparison plus an `on` turn; leaves the test
instance `off`):

```bash
make e2e-flow
```

Comment trigger (a real `/opencode start` plan turn):

```bash
make e2e-comments
```

Direct soak (N issue updates through shadow; default 10):

```bash
make e2e-soak
E2E_SOAK=100 make e2e-soak
```

Long soak (500 updates, 100 ms apart; asserts event count, completed flow runs,
zero pending comparisons, zero queue drops):

```bash
make e2e-soak-long
```

Burst (200 direct deliveries at 16-way concurrency; asserts no flow drop):

```bash
make e2e-burst
```

Silent turn (fake runtime): first turn silent + nudge; then both turns silent and
the task must fail, never `awaiting_input`:

```bash
make e2e-silent
```

Runtime failure paths (fake runtime): startup unavailable, a delivery with no
healthy runtime, a prompt that returns 500, and a 1s request timeout under the 5m
turn deadline:

```bash
make e2e-opencode-failures
```

Policy (fake runtime): an author outside the project allowlist creates no task or
session; an allowlisted reporter still works; an unmapped project is refused:

```bash
make e2e-policy
```

Serialization (fake runtime): hold the first turn, deliver a second event for the
same task, and assert it waits for one session/turn:

```bash
make e2e-serialization
```

Crash during execute (fake runtime): hold the third prompt, SIGKILL FlowHub, restart,
and assert failed recovery, same worktree/session, no replayed turn:

```bash
make e2e-crash-execute
```

Crash during an `on` run (fake runtime): SIGKILL while the execute prompt is held,
restart, assert `needs_attention` + failed task + no replay, then create a new issue
to prove the flow is still usable:

```bash
make e2e-crash-on
```

Ingress and durable append (direct receiver): wrong key/token/IP, bad content type,
malformed JSON, unknown event, replay window, oversized body, and a `503` from an
unwritable events directory followed by a successful retry (dedupe key released):

```bash
make e2e-ingress
```

Attachments and other kinds (fake runtime): a curl to the configured attachment
host/prefix is allowed, a curl elsewhere is rejected, and attachment/work-item/
deleted events are recorded without creating a task:

```bash
make e2e-attachments
```

Limits (fake runtime): after two turns, `FLOWHUB_TASK_MAX_COST=0.0015` and
`FLOWHUB_MAX_TURNS=2` each stop a third trigger before any prompt:

```bash
make e2e-limits
```

Flow control (fake runtime): pause the flow, post an event (no run), unpause and
assert the durable event catches up; set `on` for TEST only and prove TEST gets a
flow run while OTHER is still dispatched:

```bash
make e2e-flow-control
```

API (fake runtime): 401/503 auth, invalid deployment/unknown flow, `on` with the
task service, and `on` without it (409):

```bash
make e2e-api
```

Flow concurrency (fake runtime): two distinct tasks, `FLOWHUB_FLOW_CONCURRENCY=2`
and runtime `max_concurrent=2`, must produce two prompts in flight:

```bash
make e2e-flow-concurrency
```

Same-task serialization (fake runtime): two events for one task, one held turn; the
second must wait and then run in the same session:

```bash
make e2e-flow-serial
```

Cancel (fake runtime): hold the execute prompt, `POST /hub/v1/runs/{id}/cancel`,
assert `canceled`, `409` on a second cancel, no extra prompt, and a `run.canceled`
journal event:

```bash
make e2e-cancel
```

Rebind and capacity (fake runtime): a task bound to repo A refuses a mapping to repo B
without preparing a new worktree; a runtime with one slot runs two distinct tasks one
at a time:

```bash
make e2e-rebind
make e2e-capacity
```

Restart hardening (fake runtime): SIGINT while an `on` execute prompt is held, then
restart; and a cursor pointing at an event missing from the log must refuse startup:

```bash
make e2e-restart-hardening
```

Replay (append valid events while running and while stopped; assert periodic
catch-up and startup replay):

```bash
make e2e-replay
```

Restart recovery (stops and restarts the test FlowHub, checks mode and compare
state survive):

```bash
make e2e-restart
```

Permit/revoke (real refusal, `/opencode permit`, continuation, `/opencode revoke`):

```bash
make e2e-permit
```

Crash recovery (SIGKILL a real turn, restart, assert the interrupted task is
marked failed with the interruption reason):

```bash
make e2e-crash
```

`E2E_ISSUE` is the existing issue used by the `issueUpdated` webhook test; the
turn and comment tests create fresh issues in `YOUTRACK_PROJECT`.

## Isolation

The harness creates a temporary `DataDir`, a temp config containing only the
the project named by `YOUTRACK_PROJECT`, and temp worktrees. It does not read or write `./data`, the
production repository, or the production worktrees. A test run stops its FlowHub
process on exit.

Do not run `make e2e-live` while a real FlowHub is listening on the same address.
Do not point the production YouTrack webhook app at a test instance.

## Current coverage

Every checked-in scenario is a Go test in [e2e/e2e_test.go](../e2e/e2e_test.go);
the Make targets are only wrappers around the suite.

| Scenario | Test | Target |
| --- | --- | --- |
| live/direct issue update webhook | `TestWebhookDeliversAnIssueUpdate` | `make e2e`, `make e2e-live` |
| real dispatch analysis turn + session + comment | `TestIssueCreatedRunsOneDispatchTurn` | `make e2e-turn` |
| shadow comparison | `TestShadowComparisonOnIssueUpdate` | `make e2e-flow`, `make e2e-live-flow` |
| `on` execution + rollback | `TestOnExecutesAndRollsBack` | `make e2e-flow`, `make e2e-live-flow` |
| full live analyze -> plan -> execute (dispatch) | `TestLiveFullJourney` | `make e2e-live-journey` |
| full live analyze -> plan -> execute (`on`) | `TestOnLiveFullJourney` | `make e2e-live-on-journey` |
| `/opencode start` plan | `TestCommentTriggerStartsAPlan` | `make e2e-comments` |
| duplicate webhook | `TestDuplicateWebhookIsIdempotent` | `make e2e` |
| pause | `TestPauseDoesNotDispatch` | `make e2e` |
| shadow soak / queue drops | `TestShadowSoak` | `make e2e-soak` (`E2E_SOAK=100`), `make e2e-soak-long` (500) |
| concurrent burst / queue drops | `TestBurstSoak` | `make e2e-burst` |
| durable-log replay cursor | `TestReplayCursor` | `make e2e-replay` |
| restart mode + compare state | `TestRestartKeepsComparisonState` | `make e2e-restart` |
| permit/revoke | `TestPermitAndRevoke` | `make e2e-permit` |
| silent turn -> nudge -> answered, then both silent -> failed | `TestSilentTurnNudgeAndFailure` | `make e2e-silent` |
| SIGKILL during execute; failed, same worktree/session, no replay | `TestCrashDuringExecute` | `make e2e-crash-execute` |
| SIGKILL during an `on` run; needs_attention, task failed, engine usable | `TestCrashDuringOnRun` | `make e2e-crash-on` |
| ingress rejects + append 503 + dedupe release | `TestIngressRejectionsAndAppendFailure` | `make e2e-ingress` |
| attachment download allow/deny + other known kinds | `TestAttachmentsAndOtherEventKinds` | `make e2e-attachments` |
| flow runs two distinct tasks concurrently | `TestFlowConcurrentTasks` | `make e2e-flow-concurrency` |
| flow serializes two events for one task | `TestFlowSameTaskSerializes` | `make e2e-flow-serial` |
| cancel an active `on` run | `TestCancelStopsAnActiveRun` | `make e2e-cancel` |
| repository rebind refusal | `TestRepositoryRebindRefused` | `make e2e-rebind` |
| cross-task capacity saturation | `TestCrossTaskCapacitySaturation` | `make e2e-capacity` |
| graceful restart during an active `on` run | `TestActiveOnGracefulRestart` | `make e2e-restart-hardening` |
| aged-out replay cursor refuses startup | `TestAgedCursorRefusesStartup` | `make e2e-restart-hardening` |
| `/hub/v1` auth, invalid deployment, disabled API, on without tasks | `TestAPIAuthAndInvalidDeployment` | `make e2e-api` |
| flow pause, then catch-up after unpause | `TestFlowPauseAndCatchUp` | `make e2e-flow-control` |
| project-scoped `on` (TEST flow, OTHER dispatch) | `TestProjectScopedMode` | `make e2e-flow-control` |
| cost cap stops before a third prompt | `TestCostCapStopsATask` | `make e2e-limits` |
| max turns stops before a third prompt | `TestMaxTurnsStopsATask` | `make e2e-limits` |
| same-task second delivery waits; one session, no overlap | `TestSameTaskSecondDeliveryWaits` | `make e2e-serialization` |
| author allowlist deny/allowed | `TestAuthorAllowlistAndUnmappedProject` | `make e2e-policy` |
| unmapped project creates no task | `TestAuthorAllowlistAndUnmappedProject` | `make e2e-policy` |
| opencode unavailable at startup | `TestOpencodeUnavailableAtStartup` | `make e2e-opencode-failures` |
| no healthy runtime before a delivery -> ignored, no task row | `TestOpencodeUnavailableBeforeDelivery` | `make e2e-opencode-failures` |
| prompt returns 500 during a turn -> failed, not `awaiting_input` | `TestOpencodePromptFailure` | `make e2e-opencode-failures` |
| one-request timeout under a 5m turn deadline | `TestOpencodeRequestTimeout` | `make e2e-opencode-failures` |
| SIGKILL + restart recovery | `TestCrashRecovery` | `make e2e-crash` |
| live permit/revoke (dispatch) | `TestPermitAndRevoke` (live) | `make e2e-live-permit` |
| live permit/revoke (`on`) | `TestOnLivePermitAndRevoke` | `make e2e-live-on-permit` |
| cleanup test issues/temp dirs | `TestCleanupTestResources` | `make e2e-clean` |

`make e2e-all` runs every non-live, non-permit opt-in scenario in direct mode in
one process, including burst and replay/catch-up. Live journeys and the permit
tests are separate because they need the real webhook app or install a prompt file.
`make e2e-live-journey` runs the full live journey with dispatch;
`make e2e-live-on-journey` runs the same journey with the flow in `on` mode.
`make e2e-live-permit` runs the permit/revoke test against live webhooks with
dispatch; `make e2e-live-on-permit` runs it through the flow in `on` mode.
`make e2e-clean` deletes only issues with this suite's summary prefixes and removes
its temp dirs; it never deletes `E2E_ISSUE`.
`make e2e-permit` is separate because it installs a project prompt file before
the process starts, which would affect the other turns.

Not automated in the harness: YouTrack webhook app configuration and the test
project/repo/runtime setup. The formal 1-week pilot is waived by ADR 0014;
ordinary-use logs are reviewed after cutover instead.

## Known gaps

These are not E2E yet. Most have unit coverage in `internal/webhook`,
`internal/dispatch` or `internal/agent/opencode`; a unit test is not a real
process/API test. The E2E programme and its pre-cutover order are tracked in #119;
this section is the durable gap snapshot, not a separate tracker. #144 is the
closed audit that produced this list.

| Area | Missing E2E |
| --- | --- |
| routing/auth | none known after the rebind test |
| turn failure | none known after the 500/timeout/startup tests; author deny and unmapped project are in the routing row |
| same-task concurrency | none known after the capacity test |
| crash | none known after analysis/execute/on run tests |
| attachments/other kinds | none known after the allow/deny + other-kind test |
| limits | none known after the cost/max-turns test |
| flow control | none known after pause/project-scope tests |
| API | none known after the cancel test |
| restart | none known after graceful active-on restart + aged-cursor tests |
| pilot | waived by ADR 0014; post-cutover observation is not automated |

The current coverage table above is the complement: what **is** checked by a real
process, real HTTP/webhook and (for the live rows) real YouTrack/opencode.
