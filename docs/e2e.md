# Real E2E harness

`e2e/` is an opt-in end-to-end suite that drives a **real** FlowHub process against
the self-hosted YouTrack `TEST` project and the local opencode server. It is not
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

- `YOUTRACK_URL`, `YOUTRACK_PROJECT`, `YOUTRACK_TOKEN` from a `0600` file such as
  `/tmp/flowhub-e2e.env`;
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

Live webhook (test FlowHub owns `192.168.8.20:8080`, the YouTrack app delivers):

```bash
# stop the real receiver first; the test process must bind the app's URL
make e2e-live
```

Real dispatch turn (creates a new TEST issue and spends one opencode turn):

```bash
make e2e-turn
```

`E2E_ISSUE` defaults to `TEST-38` for the `issueUpdated` webhook test; the turn
test creates a fresh issue itself.

## Isolation

The harness creates a temporary `DataDir`, a temp config containing only the
`TEST` project, and temp worktrees. It does not read or write `./data`, the
production repository, or the production worktrees. A test run stops its FlowHub
process on exit.

Do not run `make e2e-live` while a real FlowHub is listening on the same address.
Do not point the production YouTrack webhook app at a test instance.

## Current coverage

- live and direct webhook delivery of an issue update;
- one real dispatch analysis turn, one worktree/session, one YouTrack comment,
  terminal `awaiting_input` state.

The next scenarios (comment trigger, permit/revoke, duplicate, pause, mode
switch/`on`, restart and soak) are tracked in #118.
