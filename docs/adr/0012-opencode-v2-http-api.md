# ADR 0012: speak both opencode HTTP API generations

- Status: accepted
- Date: 2026-10-21

## Context

opencode 2 removed the v1 HTTP surface outright. It was not deprecated: the routes
are gone from the binary, and `/global/health` — the endpoint FlowHub's startup
probe and every turn's client used — now answers the web app's HTML shell with
status **200**. FlowHub decoded that as JSON and refused to start with
`opencode: default: GET /global/health: decode response: invalid character '<'`.

Measured on this host, 2026-10-21, against `opencode serve` 2.0.32:

| Request | Answer |
| --- | --- |
| `GET /global/health` | `200 text/html`, 6962 bytes (the SPA shell) |
| `GET /api/info` (Basic Auth) | `200 {"version":"2.0.32","pid":…,"urls":[…],"paths":{…}}` |
| `GET /api/health` (Basic Auth) | `404` |
| `strings opencode \| grep -c global/health` | `0` |

The upstream migration guide says integrations that call the v1 server API must
migrate ([Migrate from V1](https://opencode.ai/v2/docs/migrate-v1/)), and the API
reference describes the `/api/*` surface
([API](https://opencode.ai/v2/docs/api)). The generated document calls the surface
"experimental", and `info.version` in its OpenAPI header is still `0.0.1`, so it is
a moving target; the shapes below are the ones 2.0.32 actually returned.

The replacement is not a rename. The v2 surface changes the request and response
contracts, and four changes are load-bearing for this project:

- **Responses are wrapped** in `{"data": …}`, the message list is paginated with a
  cursor, and a long session therefore needs more than one request.
- **A session owns its location, agent and model.** Session-scoped routes take no
  `?directory=`; the endpoints that resolve configuration take
  `?location[directory]=`, a deepObject. A prompt carries only `{text}`.
- **Completion is explicit.** The server appends an `idle` message with
  `outcome: succeeded | failed | interrupted`. v1 needed three signals (a completed
  assistant message, a non-busy status, no pending permission) because status
  lagged; v2 states the end of the turn.
- **The permission vocabulary changed.** `{permission, pattern, action}` became
  `{action, resource, effect}`, `bash` became `shell`, `task` became `subagent`, and
  the reply body became `{decision, message}`. A compound command arrives as one
  request whose `resources` holds one entry per segment, with no
  `metadata.command` to reconstruct. The default for `external_directory` also
  changed from ask to **allow**, so a ruleset that relied on v2's default would
  silently read and write outside the task worktree.

The alternative considered and rejected was to pin every host back to opencode
1.18.35. `mise` still offers that version, but the operator's configuration has
already moved to the v2 shape (`mcp.servers`, `cli.json`), so a downgrade takes the
MCP servers with it — and a hub that cannot follow its agent server forward is not a
hub. The other alternative, a v2-only rewrite, would break a host that has not been
upgraded yet; the control plane can enrol such hosts, and a runtime that silently
stops working after an upgrade elsewhere is the worst failure this project can have.

## Decision

**One `Client`, two transports, detected once per server.** The first call probes
`/api/info`; a JSON answer with a non-empty `version` means v2. Otherwise the client
probes `/global/health`; JSON means v1. A `401` is reported as a credential failure
instead of falling through, because the second probe would repeat it. A server that
answers neither produces one error naming both attempts and the HTML fallback.
Detection is cached only on success, so a server that was down at start-up is
retried.

The v1 body moved to `internal/agent/opencode/client_v1.go`; the v2 body is
`client_v2.go`. The exported surface (`Health`, `Agents`, `Catalogue`,
`CreateSession`, `PromptAsync`, `Session`, `SessionStatuses`, `Messages`,
`Permissions`, `ReplyPermission`) is unchanged for the callers in `cmd/flowhub` and
`internal/provision`; the two signature changes (`SessionStatuses` and `Permissions`
take a session id, `ReplyPermission` takes one too) are confined to the runner,
because v2's permission routes are session-scoped.

**One policy, translated at the edge.** The arbiter, the phase rule, the wrapper
handling and the grant set keep speaking v1 names. The v2 transport maps
`action: "shell"` to `bash`, `resources` to `patterns`, `save` to `always`, and
reconstructs the command (`resources` joined with `&&`, unless the server sends
`metadata.command`). Teaching the policy two vocabularies was rejected: two
allowlists that can drift is how a command gets allowed in one generation and
refused in the other.

**The ruleset is built in the server's vocabulary.** `Runtime.Run` asks the client
which generation it speaks and fills `Task.Ruleset` or `Task.RulesetV2`; the
transport refuses the other one rather than creating a session with a ruleset the
server ignores. The v2 ruleset keeps the catch-all first (evaluation is still
last-match-wins), keeps `edit` and `shell` at `ask` so the phase can decide, and
keeps the explicit `external_directory: deny` that v1 needed — in v2 it is the only
thing that keeps a turn inside its worktree, because the default is now allow.

**v2 completion is the idle marker.** `runV2` takes a message baseline before the
prompt and finishes when an `idle` message appears after it with no permission
pending. An `outcome: failed` still finishes the turn (v1 recorded an assistant
error the same way) and carries the structured error's message in `Result.Error`.
The first-response bound is unchanged, and catches a server that admits the prompt
and then writes nothing at all.

**A recorded session that no longer exists is re-created.** A registry row written
against a v1 server names a session a v2 server has never heard of. Refusing would
make that task undeliverable for good, so `runV2` treats a 404 from the message list
as "this task needs a new session", logs it loudly and creates one; the result
carries the new id, which the dispatcher already writes back to the registry. A v1
turn keeps its old behaviour.

**Model pinning stays explicit.** Measured on 2.0.32: a session created with
`agent: "devops"` and no `model` runs under the server's *global default* model, not
the agent profile's model (the profile reported `deepseek/deepseek-flash`). FlowHub
already pins the model the activation check reported (ADR 0002), so turns are
unaffected; the live check and the documentation now say why the pin is not
optional on v2.

## Consequences

- Both generations are tested: the v1 fake server in `runner_test.go` and the v2 one
  in `runner_v2_test.go`, plus protocol detection, pagination, the ruleset
  translation, the failed-outcome path and the dead-session path.
- The v2 surface is labelled experimental upstream. A future 2.x can change it; the
  detection and the transports are the only places that need to follow, and the
  runtime records the server version it talked to (`Health`), so a mismatch is
  visible before it becomes a failed turn.
- `Agents` on v2 uses the agent's **id**, not its display name ("build", not
  "Build"), because that is the value a session is created with. A profile compare
  on the label would pass and then create a session that fails with "Agent not
  found".
- `Catalogue` on v2 keeps only enabled, non-deprecated models: a disabled model is
  not offered for a new session, so the enrolment check must not treat it as
  offered.
- The live check (`make test-live`) now pins `OPENCODE_MODEL` and builds the ruleset
  for the server's generation; on v2 an unpinned turn uses the server's default
  model, which may not be one the operator's credentials can reach.
- Removal condition for v1: when every host the inventory can enrol reports
  `version >= 2` on the health probe. At that point `client_v1.go`, the v1 runner
  loop and the v1 fake go away, and `Protocol` can stop probing.

Measured end to end: `OPENCODE_LIVE=1` against opencode 2.0.32, agent `devops`,
`deepseek/deepseek-flash`, a throwaway directory. The adapter detected v2, created
the session with the v2 ruleset, answered one `shell` permission request
(`ls -a` → `once`, translated to the `bash` vocabulary), and finished on an idle
message with `outcome=succeeded`, 4.7 s, 9556 tokens, answer `4`.
