# FlowHub — YouTrack webhook receiver and opencode dispatcher

A Go service that turns DevOps events into work for a headless `opencode`
instance. One process, two halves:

* **the receiver**, always on: authenticates and validates each YouTrack webhook
  delivery, deduplicates it, writes an audit record, and answers `202 Accepted` in
  well under a millisecond. It never calls YouTrack, opencode or any other
  network service on the request path.
* **the dispatcher**, opt-in through `FLOWHUB_DISPATCH=1`: takes an *accepted*
  delivery off a queue and runs one opencode turn for it, inside a git worktree
  created for that issue alone. The agent reads the issue, replies with a
  comment, and — once a human says so — implements the change on its own branch.

With dispatch off, the process is exactly the receiver described above: it
records, it audits, and it does nothing else.

The three documents in the repository root are the specification:

| Document | Role |
| --- | --- |
| `youtrack-webhook-and-flowhub-security.md` | YouTrack side, payload facts, layered security design (§8, §9, §12) |
| `opencode-devops-orchestration-design.md` | Overall FlowHub architecture, session ownership, event-driven model |
| `opencode-headless-automation-and-permissions.md` | opencode HTTP API and the permission loop the dispatcher drives |

## Why the receiver looks like this

Every design decision below is traceable to a measured fact in those documents.

| Constraint from the design | Consequence in this code |
| --- | --- |
| The published Webhook Triggers app posts **synchronously** with a 5s timeout and no retry (§3) | The handler answers immediately and never blocks on I/O; the audit write happens on a background goroutine with a bounded queue |
| Any slow response is felt by the person editing the issue (§4.3) | Target is `<200ms`; the smoke test measures `~0.4ms` locally |
| The app sends **no delivery id and no signature** (§3) | Idempotency is derived locally from `sha256(event, issue id, timestamp, body)`; the shared secrets are the only credentials |
| The token on the wire may be the literal string `secret` (§4.2) | The service never stores the raw token: it records a length plus SHA-256 prefix and explicitly flags the literal case |
| The payload contradicts the official documentation (§5.2) | Parsing is lenient (unknown fields ignored, nothing required but `event`), `changedFields` values stay raw, and every payload's top-level key set is recorded |
| One URL embedded key, one header token, one source restriction (§9.1) | Three independent locks, evaluated in order; any failure returns the same empty `202` as a success |
| The URL key would otherwise leak into access logs (§8.3) | Path and query are redacted before they reach the log or the audit file |
| Every delivery must be auditable and replayable (§9.7) | One JSON line per delivery, accepted or not, including the raw payload |

## Quick start

The Nix dev shell (`nix develop`, or `direnv allow` once) provides Go and the
tools the Makefile uses. Everything below works the same with a plain local Go
toolchain.

```bash
make build            # -> bin/flowhub, with version/commit/build-time stamped in
make secrets          # prints a fresh FLOWHUB_HOOK_KEY / FLOWHUB_TOKEN pair

export FLOWHUB_HOOK_KEY=$(openssl rand -hex 32)   # goes into the webhook URL
export FLOWHUB_TOKEN=$(openssl rand -hex 32)      # goes into the header
export FLOWHUB_DATA_DIR=./data

./bin/flowhub -print-config   # verify the effective settings (secrets masked)
./bin/flowhub
```

Then configure the project in YouTrack (`项目 → Settings → Apps → Webhook
Triggers → Settings`):

| Setting | Value |
| --- | --- |
| Webhook URL | `https://<your-host>/hooks/youtrack/<FLOWHUB_HOOK_KEY>` |
| Header name | `X-YouTrack-Token` |
| Token | the same value as `FLOWHUB_TOKEN` |

The service must be reachable from YouTrack. During development you can drive it
directly with `curl` (see below) instead of exposing a public domain.

### Local delivery test

```bash
KEY=$(openssl rand -hex 32); TOKEN=$(openssl rand -hex 32)
FLOWHUB_HOOK_KEY=$KEY FLOWHUB_TOKEN=$TOKEN FLOWHUB_ALLOWED_SOURCES=127.0.0.1 \
FLOWHUB_LOG_HEADERS=true ./bin/flowhub &

TS=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' \
  -X POST "http://127.0.0.1:8080/hooks/youtrack/$KEY" \
  -H "X-YouTrack-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"event":"issueUpdated","timestamp":"'"$TS"'","id":"2-123","summary":"Fix login",
       "project":{"key":"SP","name":"Sample Project","shortName":"SP"},
       "updatedBy":{"login":"jane.doe","fullName":"Jane Doe"},
       "changedFields":[{"name":"State","oldValue":{"name":"Open"},"value":{"name":"In Progress"}}]}'
# => 202 0.0004s
```

## Commands

The service and the data-plane host are the same binary, and a subcommand is
dispatched before the receiver configuration is loaded — a host that will run
turns has none of the receiver's environment variables and must not fail because
they are absent.

```bash
flowhub                      # run the receiver (and the dispatcher)
flowhub -version             # print the version
flowhub -print-config        # print the effective configuration, secrets masked
flowhub runtime init --check # report what this host can and cannot do; writes nothing
flowhub runtime init         # report, install the files FlowHub manages, enrol
flowhub runtime doctor       # re-check this host; writes nothing
flowhub runtime doctor --push# re-check and report to the control plane
flowhub runtime uninstall    # remove exactly what the manifest records
```

On the service host, the same binary manages the runtime inventory over the
control API — the running service is the only writer, so a change applies without
a restart:

```bash
flowhub runtime invite builder-a --projects TEST --ttl 30m   # prints a one-time token
flowhub runtime list                                         # state, address, agent, pinned models
flowhub runtime show builder-a
flowhub runtime remove builder-a [--force]                   # refuses while tasks are bound
flowhub runtime rotate builder-a                             # re-issue the runtime secret
```

Flags may appear on either side of the positional argument (`remove builder-a
--force` and `--force builder-a` are the same command; the flag package alone
would silently drop the trailing form).

`runtime init --check` is the first half of
[`docs/adr/0002`](./docs/adr/0002-data-plane-runtime-installation.md): it checks
the agent runtime, `git`, the forge command-line tools the declared repositories
need, each clone's `origin`, read access (`git ls-remote`) and **write** access
(`git push --dry-run`, which contacts the remote and updates nothing), the model
each installed agent profile pins against the agent server's catalogue, plus the
presence of required environment variables. It reports the identity it ran as,
because a check that runs as the wrong user passes and then the first push fails.
Exit status is 0 for ready, 2 for not ready, 1 for a usage mistake.

```bash
flowhub runtime init --check --forge git.yusiwen.cn=gitea \
  --repo git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git=/Users/me/git/beap-be
```

`runtime doctor` runs exactly the same check later, so the first task is never the
thing that discovers an expired credential. With `--push` it sends the report to
the control plane, authenticated by this host's **runtime secret**, which refreshes
`last_seen` and the models the dispatcher pins — a repaired profile reaches the next
turn without a restart. A report that does not pass is deliberately **not** pushed,
so `last_seen` keeps meaning "a host that was verified fit".

### Installing the agent files

`flowhub runtime init` writes the agent definition FlowHub manages, then records
every file it wrote — path, SHA-256 and the FlowHub version — in a manifest at
`~/.config/flowhub/manifest.json`. What it installs is **embedded in the binary**,
never fetched from the control plane: the agent file *is* the agent's system
prompt, so a compromised service must not be able to push prompts to every worker.

The manifest is what makes the installer safe to re-run:

| On disk | What happens |
| --- | --- |
| matches this build | nothing (a no-op) |
| matches what FlowHub wrote, but this build embeds something newer | updated |
| missing | restored |
| there, identical, but not in the manifest (the hand-written case) | adopted, no write |
| **changed by a human** | **refused, with a diff**; `--force` overwrites |
| unreadable | refused, and reported as unreadable rather than missing |

`uninstall` deletes exactly the manifest's files — a file that was edited
afterwards is kept unless `--force` is given — and then removes the directories it
created, so the tree goes back to what it was.

The agent's **MCP configuration is never rewritten**: v1 prints the block to merge
when the file already exists, and only creates it when there is nothing to
destroy. That file may hold comments and other servers, and a JSON round-trip would
delete both.

`--config-root <dir>` installs into a throwaway tree instead of the real one, which
is how the round trip is tested without touching an operator's configuration. The
capability report names the configuration directory it *read*, so a report from an
overridden root never points the operator at the default one.

### Enrolling a host

A host becomes a runtime in three steps: an operator invites the name on the
service, the host installs and registers, and the service probes the address the
host advertised before it activates it.

```bash
# on the service host
flowhub runtime invite builder-a --projects TEST
# -> prints the token and the command to run on the other machine

# on the host that will run turns
flowhub runtime init --server http://gateway.lan:8081 --name builder-a \
  --token <one-time token> --advertise http://builder-a.lan:4096
```

The host keeps the returned secret in `<config-root>/../flowhub/runtime.json`
(mode `0600`); the service stores only its SHA-256. `--advertise` is what the
service probes, so a NAT or firewall mistake fails at enrolment rather than at the
first task. The probe is more than a health check: it also requires the agent
profile the host claimed to exist on **that** server, and the model the host
reported to be one the server offers. opencode accepts a session for an agent it
does not have and silently falls back to its own default agent, so without that
check a host could enrol and then run every turn with the wrong step budget and
permission block. A server whose *cached* agent is older than the reported one is a
warning rather than a refusal — the dispatcher pins the reported model, so the turn
is correct either way, and the warning says to restart the agent server.

The claim carries the models the installed profiles pin, and the dispatcher pins
that model on every turn: the agent server resolves an agent name against a list it
cached when it started, so without pinning a repaired profile would be verified by
the check and ignored by the turn.

**A task is bound to the runtime that prepared its worktree, for life.** Removing a
runtime refuses while non-terminal tasks are bound to it (`--force` overrides, and
those tasks are then refused one by one with the reason instead of silently moving
to another host).

## Configuration

Flags: `-version`, `-print-config`.

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `FLOWHUB_ADDR` | `127.0.0.1:8080` | Listen address. Bind the WireGuard address, the LAN address a router forwards to, or loopback behind a reverse proxy. A wildcard bind (`0.0.0.0:8080`, `:8080`, `[::]:8080`) refuses to start unless `FLOWHUB_ALLOW_WILDCARD_LISTEN=1` acknowledges it |
| `FLOWHUB_ALLOW_WILDCARD_LISTEN` | `false` | Acknowledge a wildcard bind. Required for container images (the process must listen on the pod address); the right choice nowhere else |
| `FLOWHUB_HOOK_PATH` | `/hooks/youtrack` | Base path of the endpoint |
| `FLOWHUB_HOOK_KEY` | *(unset)* | Lock 1: URL embedded secret. Empty disables the lock |
| `FLOWHUB_TOKEN_HEADER` | `X-YouTrack-Token` | Header carrying the shared token |
| `FLOWHUB_TOKEN` | *(unset)* | Lock 2: expected header value. Empty disables the lock |
| `FLOWHUB_ALLOWED_SOURCES` | *(unset)* | Lock 3: comma separated IPs/CIDRs, e.g. `10.8.0.0/24,127.0.0.1`. Empty disables the lock |
| `FLOWHUB_DATA_DIR` | `./data` | Audit log directory (created `0700`, files `0600`) |
| `FLOWHUB_MAX_BODY_BYTES` | `262144` | Body cap; mirrors nginx `client_max_body_size` |
| `FLOWHUB_REPLAY_WINDOW` | `10m` | Reject payloads whose `timestamp` is further away. `0` disables it |
| `FLOWHUB_DEDUPE_TTL` | `24h` | How long an idempotency key is remembered |
| `FLOWHUB_QUEUE_SIZE` | `1024` | Audit queue depth between handler and writer |
| `FLOWHUB_LOG_HEADERS` | `true` | Record the received headers (values included, sensitive ones masked) plus a token fingerprint |
| `FLOWHUB_LOG_FILE` | `<DataDir>/flowhub.log` | Application log file; `-` or `none` disables file logging |
| `FLOWHUB_LOG_MAX_BYTES` | `33554432` | Rotate the application log at this size |
| `FLOWHUB_LOG_MAX_FILES` | `5` | Rotated archives to keep (`flowhub.log.1` … `.N`) |
| `FLOWHUB_DETAIL_LOG` | `true` | Write the human readable per-delivery payload log |
| `FLOWHUB_DETAIL_MAX_BODY_BYTES` | `65536` | How much of a body is pretty printed in the payload log |
| `FLOWHUB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `FLOWHUB_LOG_FORMAT` | `text` | `text` or `json` |
| `FLOWHUB_SHUTDOWN_TIMEOUT` | `10s` | Graceful drain budget on `SIGINT`/`SIGTERM` |
| `FLOWHUB_CONFIG_FILE` | `~/.config/flowhub/config.json` | The configuration file: sources, runtimes, and the `(source, project)` → local repository table. `$XDG_CONFIG_HOME/flowhub/config.json` when that is set. A leading `~` is expanded, and the path is made absolute at startup. A missing file only warns (the receiver still records); a file that exists but fails validation stops startup |
| `FLOWHUB_PROJECTS_FILE` | *(alias)* | The old name of `FLOWHUB_CONFIG_FILE`, still read. The file is no longer only a routing table, so the new spelling is preferred; setting both uses `FLOWHUB_CONFIG_FILE` |
| `FLOWHUB_SOURCE` | `youtrack` | Which registered event source this receiver serves. It selects the adapter that decodes deliveries and the `sources.<name>` block that supplies the trigger policy |
| `FLOWHUB_YOUTRACK_HOOK_KEY` | *(unset)* | Lock 1 (URL key) for the YouTrack source. `FLOWHUB_HOOK_KEY` is the same variable under its old name; a second source gets its own, which is why the locks are per source |
| `FLOWHUB_YOUTRACK_TOKEN` | *(unset)* | Lock 2 (header token) for YouTrack. `FLOWHUB_TOKEN` is the alias |
| `FLOWHUB_YOUTRACK_ALLOWED_SOURCES` | *(unset)* | Lock 3 (source IP allowlist) for YouTrack. `FLOWHUB_ALLOWED_SOURCES` is the alias |

The dispatcher (`opencode-devops-orchestration-design.md` §12) is off until it is
switched on explicitly:

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `FLOWHUB_DISPATCH` | `false` | Run opencode turns for accepted deliveries. Off means record-only |
| `FLOWHUB_OPENCODE_URL` | `http://127.0.0.1:4096` | Base URL of the headless opencode server. Startup probes `/global/health` and refuses to start if it does not answer |
| `FLOWHUB_OPENCODE_USER` | *(unset)* | HTTP Basic Auth username for every call to an agent server. Set with the password or not at all; required before a runtime that is not on loopback is usable |
| `FLOWHUB_OPENCODE_PASSWORD` | *(unset)* | HTTP Basic Auth password, matching the server's `OPENCODE_SERVER_PASSWORD`. Never printed: `-print-config` shows a fingerprint |
| `FLOWHUB_DISPATCH_AGENT` | `devops` | opencode agent for turns whose routing entry names none |
| `FLOWHUB_DISPATCH_QUEUE` | `32` | Deliveries waiting for a runtime's worker. A full queue drops work instead of blocking the publisher |
| `FLOWHUB_TASK_DEADLINE` | `15m` | One turn's budget. A deadline is not a failure: the session keeps running and is marked `executing` |
| `FLOWHUB_FIRST_RESPONSE` | `90s` | How long a turn may take to produce its first assistant message before it is **failed**. A prompt the agent server never turns into a turn (a model its provider dropped) leaves the session idle with no message, which otherwise looks like a slow turn until the deadline. Capped at a third of `FLOWHUB_TASK_DEADLINE`, floored at `5s` |
| `FLOWHUB_MAX_TURNS` | `8` | A task that triggers more often than this stops and asks for a human |
| `FLOWHUB_TASK_MAX_COST` | `0` | Stop a task whose accumulated opencode cost passes this many dollars. `0` disables the check |
| `FLOWHUB_TRIGGER` | `/opencode start` | The comment that means "implement it" |
| `FLOWHUB_START_STATES` | `In Progress` | Comma separated workflow states that mean "implement it" |
| `FLOWHUB_SKIP_ANALYZE_ON_CREATE` | `false` | Skip the automatic read-only analysis of a newly created issue |
| `FLOWHUB_REGISTRY_FILE` | `<DataDir>/registry.jsonl` | Task registry: issue → repository, worktree, session, state, cost |
| `FLOWHUB_PAUSE_FILE` | `<DataDir>/DISPATCH_OFF` | Kill switch. While this file exists, deliveries are audited and ignored |
| `FLOWHUB_WORKTREE_BASE` | *(unset)* | Fallback checkout directory for routing entries that declare none. Facts about it (exists, outside the repository) are checked by the workspace provider, not by the routing table |

The control API (ADR 0002 step 3) is a second listener, because the webhook entry
and the admin API have opposite semantics: the webhook answers `202` to everything
and explains nothing, and the admin API returns real errors and refusals.

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `FLOWHUB_ADMIN_ADDR` | *(unset)* | Control API listen address, e.g. `127.0.0.1:8081`. Unset turns the listener off. A wildcard bind is refused like the webhook's |
| `FLOWHUB_ADMIN_TOKEN` | *(unset)* | **Required** when `FLOWHUB_ADMIN_ADDR` is set; the listener refuses to start without it |
| `FLOWHUB_RUNTIMES_FILE` | `<DataDir>/runtimes.json` | Runtime inventory. Written only by the service |

The control API serves `/control/v1/runtimes` (`GET` inventory, `POST …/invite`,
`POST …/register`, `POST …/{name}/heartbeat`, `POST …/{name}/rotate`,
`DELETE …/{name}`) plus an open `/control/v1/health`. Everything but `register`,
`heartbeat` and the health endpoint needs the admin token as
`Authorization: Bearer …`.

`FLOWHUB_DISPATCH=1` refuses to start when dispatching cannot work: no routing
table, an entry with no `default_branch`, an entry with no `worktrees` directory
and no fallback, or an opencode server that is not answering. A hub that accepts
deliveries and then fails every one of them is worse than one that does not start.

Disabled locks produce a loud startup warning, and `-print-config` lists the
active locks. All three locks are expected to be set once the public entry exists
(`youtrack-webhook-and-flowhub-security.md` §12.2).

## Routing: YouTrack project → repository

opencode operates on a directory, so every delivery has to be resolved to the
repository it belongs to. That resolution is **declared in a file, never
guessed**:

```bash
mkdir -p ~/.config/flowhub
cp config/config.example.json ~/.config/flowhub/config.json   # then edit it
./bin/flowhub -print-config                                   # shows the table and any problem
```

```json
{
  "version": 2,

  "sources": {
    "youtrack": {
      "enabled": true,
      "policy": {
        "trigger": "/opencode start",
        "start_states": ["In Progress"],
        "max_turns": 8
      }
    }
  },

  "runtimes": {
    "builder-a": { "url": "https://builder-a.lan:4096", "agent": "devops", "deadline": "15m" },
    "builder-b": { "url": "https://builder-b.lan:4096", "agent": "devops" }
  },

  "projects": [
    {
      "source": "youtrack",
      "project": "BEAP_BE",
      "also": ["BEAP"],
      "repo": {
        "path": "/Users/yusiwen/git/work/pipechina/beap-be",
        "remote": "git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git",
        "default_branch": "master"
      },
      "worktrees": "/Users/yusiwen/git/work/pipechina/beap-be-worktrees",
      "agent": "devops",
      "runtimes": ["builder-a", "builder-b"],
      "runtime_policy": "spread"
    }
  ]
}
```

The file has three parts and one rule: **no secret is ever written in it.** Locks
and key material stay in the environment (`FLOWHUB_YOUTRACK_HOOK_KEY`,
`FLOWHUB_YOUTRACK_TOKEN`, `FLOWHUB_YOUTRACK_ALLOWED_SOURCES`), and a runtime's
password is named by the variable that holds it (`"auth": {"user": "opencode",
"password_env": "FLOWHUB_BUILDER_A_PASSWORD"}`), so the file stays safe to copy,
diff, back up and review.

**Version 1 files still load.** A file with no `version` key can only have meant
YouTrack, so it is translated on read and reported as
`config_format: 1 (translated; migrate the file to version 2)`. What version 2 adds
is the mechanical thing version 1 could not express: the entry names its tracker in
a *field* (`source` + `project`) instead of in the field's *name*
(`youtrack_key`), and the index is per source — so a YouTrack `TEST` and a Gitea
`TEST` are two projects. A version 2 file that still writes `youtrack_key` is
refused with the replacement named in the error.

**Three levels, and the report says which one won.** Outermost is the environment
(`FLOWHUB_TRIGGER`, `FLOWHUB_START_STATES`, `FLOWHUB_SKIP_ANALYZE_ON_CREATE`,
`FLOWHUB_MAX_TURNS`, `FLOWHUB_OPENCODE_URL`, `FLOWHUB_DISPATCH_AGENT`,
`FLOWHUB_TASK_DEADLINE`, `FLOWHUB_OPENCODE_USER`/`PASSWORD`), then the file's
`sources.<name>` and `runtimes.<name>` blocks, then the project entry for the values
a project may override (`agent`, `model`, `authors`, `runtime`/`runtimes`,
`enabled`). `-print-config` prints each effective value with its level:

```
effective:          sources.youtrack trigger="/opencode start" (level: FLOWHUB_TRIGGER)
effective:          runtimes.builder-a deadline=20m (level: runtimes.builder-a)
effective:          projects[youtrack:TEST] model=deepseek/v4 (level: projects[youtrack:TEST])
```

### Prompt files

A source (`sources.<name>.prompt_file`) and a project
(`projects[].prompt_file`) may each name a file of extra instructions, resolved
relative to the configuration file. Both are **appended** to the adapter's built-in
prompt, in their own `## Project instructions` section, and both are read when the
configuration is loaded:

* a missing, empty, directory or larger-than-32-KiB file **refuses the start** — a
  configured file that silently had no effect is the failure mode this project
  refuses everywhere;
* the file is guidance about *how* to work in a repository ("this repo is Java, run
  `mvn -q verify`"), not a replacement for the contract. The untrusted-input warning,
  the reply tool, the sign-off that names the trigger, and the
  `<!-- flowhub-auto -->` marker stay in the adapter's code, because a text file that
  could delete the marker would break the loop prevention that stops FlowHub from
  answering its own replies.

One limit is still deliberate and refused rather than ignored:
`runtimes.<name>.max_concurrent` accepts only `1` — a session belongs to one runtime
and a prompt sent to a busy session is silently swallowed, so raising it needs
per-task locking that does not exist yet.

### Which host runs the work

A project names one runtime (`"runtime": "builder-a"`), several
(`"runtimes": ["builder-a", "builder-b"]`), or none — and none means "any
configured runtime may serve this project". A list is an **eligibility set**, not
an ordered preference list.

`runtime_policy` decides which of them takes a **new** task. It is `spread` by
default and may be set for the whole table (the top level of the file) or per
project:

| Policy | Rule |
| --- | --- |
| `spread` (default) | Fewest turns in flight, then fewest non-terminal tasks in the registry, then name order — so a second machine is used instead of sitting idle |
| `first-healthy` | The first name in the declared order that answers, so the rest are failover |

Every tie-break is deterministic, and the choice is logged with the numbers it was
made from:

```
msg="runtime chosen" runtime=builder-tmp policy=spread eligible=2 in_flight=0 active_tasks=1 candidates=builder-tmp,builder-b
```

**A task that is already bound goes to its own runtime, always**, whatever the
policy says and whatever the project's eligibility set has become since. Failover
happens at creation only: if the first candidate does not answer, the next
eligible one is tried and the task binds to whichever succeeded. A bound task whose
runtime is gone is refused with that reason rather than silently re-homed, because
a session on another host is a fresh context that has lost the analysis and the
plan.

### The baseline every task starts from

Two hosts mean two clones, and a clone is only as new as its last fetch — so with
`spread` the *normal* case would give two tasks for one project two different
starting points, and a patch that applies on one host would not apply on the other.
The base commit is therefore resolved **through the origin** and pinned:

* `git ls-remote origin refs/heads/<default_branch>` answers the question once, when
  the task is created, and that commit is recorded in the registry
  (`base_commit`) and never re-resolved — a long-running task's patches stay
  reviewable against a fixed base even if the origin moves;
* the worktree is created **at that commit**; a clone that has never seen it fetches
  it on demand (`git cat-file -e` first, so the fetch happens once, not per task),
  and a host that cannot obtain it fails instead of quietly using an older commit;
* if a `flowhub/<task key>` branch already exists, it is attached to **only when it
  descends from the pinned base** — a stray branch from an unrelated run must not be
  built on silently.

A repository with no `origin` remote has no shared truth to pin: the local ref is
used and the task log says so, because "pinned" and "assumed" are different claims.

Startup checks the names against the inventory: a name that was **revoked** is a
configuration error and stops the start (with the fix in the message), while a name
that is merely not enrolled yet is a warning naming the invite/init commands — the
normal state between the two halves of enrolment, which must not block the start
that performs it.

**Why it cannot come from YouTrack.** The webhook payload carries only
`{key, name, shortName}` for the project, and YouTrack exposes no REST field or
endpoint for the VCS-integration repository URL — so there is nothing to read
even though YouTrack knows the answer. Measured evidence:
`youtrack-webhook-and-flowhub-security.md` §5.6.

### Matching

| Rule | Behaviour |
| --- | --- |
| Index | `(source, project)`, both matched **case-insensitively**. The same project key in two sources is two entries; the same key twice in one source is refused (aliases included) |
| Aliases | `also` lets several tracker projects share one repository |
| Project key | Taken from the event's subject, which the adapter fills — including the YouTrack convention that a payload with no project object falls back to the issue ID's prefix (`BEAP_BE-12` → `BEAP_BE`). **The router itself never guesses**: a delivery with no project, or with one this file does not map, is a hard miss |
| `enabled: false` | Never matched, and not validated (it may point at a checkout this host does not have) |
| `runtime` / `runtimes` | The eligibility set for a new task. A name that is not a runtime name (`^[a-z0-9][a-z0-9._-]{0,62}$`) is refused when the file is loaded, because it could never be enrolled |
| `runtime_policy` | `spread` or `first-healthy`, per project or for the whole file; anything else is refused at load |
| `sources.<name>` | `enabled`, the trigger `policy`, an author allowlist a project may narrow, and `prompt_file` (extra instructions, appended — see below) |
| `runtimes.<name>` | `url`, `auth` (a username plus the *name* of the variable holding the password), `agent`, `model`, `deadline`, `max_concurrent` (only `1`) |

### Startup validation (fail-closed)

A mapping file that exists must be correct, so these all **stop the process**
before any file is created:

* an unknown `version`, or any unknown JSON member — a typo in a field name is an
  error, not a no-op;
* a version 2 file that uses a version 1 spelling (`youtrack_key`, `also_keys`),
  refused with the replacement named;
* a `source` this binary cannot build (the error lists the adapters it knows);
* a `prompt_file` that is missing, empty, a directory, or over 32 KiB;
* a `runtimes.<name>` block that cannot be honoured: a URL that is not http(s), a
  `max_concurrent` other than `1`, an unparsable `deadline`, a user without a
  `password_env` (or the reverse), or a `password_env` naming an empty variable;
* a duplicate `(source, project)`, or an `agent` name that is not a plain
  identifier, or a `model` that is not spelled `provider/model-id`;
* `repo.path` missing, relative, not a directory, or not a git work tree — checked
  by the **workspace provider** (`localworktree.Validator`), because it is a fact of
  the host that will prepare the checkout rather than of the file;
* `repo.path` not matching `repo.remote` — compared against `.git/config`, so a typo
  cannot route work into the wrong clone (leave `remote` empty to skip only this
  check);
* `worktrees` inside `repo.path` or equal to it, or neither `worktrees` nor
  `FLOWHUB_WORKTREE_BASE` set while dispatch is on;
* `runtime_policy` that is neither `spread` nor `first-healthy`, or a runtime name
  that could never be enrolled (see above).

Keys whose name starts with `_` are documentation and are ignored, which is how
the example carries its explanations in a format without comments.

A **missing** file is only a warning: the receiver records deliveries without routing
them.

The table lives **outside the repository**, at `~/.config/flowhub/config.json`
(`$XDG_CONFIG_HOME/flowhub/config.json` when that variable is set), because its
paths are host-specific. `FLOWHUB_PROJECTS_FILE` overrides it; a leading `~` is
expanded (Go does not expand it for you) and the value is resolved to an absolute
path at startup, so the loader, the logs and `-print-config` all name the same
file — whatever the working directory was. The committed template is
`config/config.example.json`.

A match becomes work only when `FLOWHUB_DISPATCH=1`. The dispatcher then turns
the match into a per-task git worktree, an opencode session bound to that
directory, and a row in the task registry; an unmapped project is refused before
any of that happens. See "The dispatch workflow" below.

## opencode integration

```bash
make test-live                                        # throwaway directory
OPENCODE_LIVE_DIR=/path/to/repo make test-live        # read-only inspection of a real repository
```

Both are skipped by `make test` unless `OPENCODE_LIVE=1`, because a live run
spends tokens and creates real sessions.

### The turn lifecycle

```
POST /session?directory=<worktree>   session-level ruleset, agent, task key
POST /session/{id}/prompt_async      204, then the model works
  loop: GET /permission?directory=   answer what the ruleset chose to ask about
        GET /session/status?         busy / idle
        GET /session/{id}/message    a new assistant message with time.completed
  -> final text, cost, tokens, and every permission decision
```

Completion needs all three signals: `status` alone lags after a turn ends, and a
pending permission keeps a session `busy` — so "busy" never means "working" and
"idle" never means "finished".

### Permissions are decided in two layers

1. **Session-level ruleset** (first line): a short allowlist — reads and the
   YouTrack tools the turn needs — over a catch-all `ask`. The catch-all has to
   come **first**, because the ruleset is an array evaluated *last-match-wins*: a
   trailing catch-all overrides every specific entry before it. `edit` is `ask`
   rather than `deny`, because only the phase can decide whether a change is
   allowed, and the arbiter is what answers it.
2. **Arbiter** (second line): answers the `ask` requests, which would otherwise
   hang the turn forever. It is an allowlist over shell *segments*:
   * the command is split on `&&`, `||`, `;`, `|` and newlines, and **every**
     segment must match an allow entry;
   * redirection, command substitution and background execution are rejected;
   * **paths outside the worktree are rejected** — `cat ~/.ssh/id_rsa`,
     `tail -f /var/log/system.log`, `ls /Users/…`. The ruleset's
     `external_directory` does not cover a shell command, so this is the check
     that does;
   * the request's own `patterns` must agree with its `metadata.command`;
   * build and test commands are allowed because they are the agent's job — and
     they are also arbitrary code execution, so the containment that matters is
     the per-task worktree, not this list.
   A rejection carries a reason, which opencode hands to the model as feedback, so
   the model adapts instead of repeating the call.

Measured on 2026-09-20 against opencode 1.18.31: a read-only turn finishes in
about 6 seconds, and the model often submits a *compound* command
(`git status && echo --- && git log -3 && git branch --show-current`) as a single
permission request with one pattern per command. A live two-turn task (analysis,
then implementation of a one-file change) costs roughly half a cent.

## The dispatch workflow

What a maintainer sees, once `FLOWHUB_DISPATCH=1`:

1. **An issue is created.** The agent gets a read-only turn: it inspects the
   repository in a fresh worktree, then posts one comment with what it found, the
   risks and the open questions. It cannot edit, commit or change YouTrack state
   in this phase.
2. **Either** somebody comments `/opencode start`, **or** the issue moves to a
   state listed in `FLOWHUB_START_STATES`. If no plan has been produced yet, that
   turn produces the plan and the blocking questions instead of implementing.
3. **With a plan in hand, the same trigger implements it**: the agent edits files
   in its worktree, runs the narrowest test or build it can find, and commits
   locally on branch `flowhub/<issue key>`. It never pushes and never touches the
   shared checkout. The branch is what a human reviews.
4. Every turn ends with exactly one YouTrack comment. The last element of that
   comment is a Markdown blockquote that says the reply was produced
   automatically by opencode and names what triggered the turn — measured
   rendering: `> 本评论由 opencode 自动生成，触发来源是本问题的创建。` — and its
   final line is `> <!-- flowhub-auto -->`. The marker is how FlowHub recognises
   its own replies; the blockquote is how a *reader* can tell an automated reply
   from a human one, and the trigger it names comes from the decision rather than
   from the model's own account of why it replied.

Which deliveries become work is a pure function of the neutral event and the
recorded task (`internal/rules`), so it is unit tested without opencode, git or
the network. The event comes from the source adapter
(`internal/source/youtrack`), which is also where the per-turn prompt lives:

| Delivery | Action |
| --- | --- |
| Our own comment (marker, a distinctive repeat of our last reply, or `/opencode` in any other form) | ignore |
| Task already at `FLOWHUB_MAX_TURNS` | ignore, and ask for a human |
| `/opencode start` comment, or the configured state change, **and** no plan yet | plan |
| `/opencode start` comment, or the configured state change, **with** a plan | execute |
| `issueCreated` (unless `FLOWHUB_SKIP_ANALYZE_ON_CREATE=1`) | analyze |
| Anything else, including a project with no repository mapped | ignore |

The trigger is anchored, so a reply that merely *mentions* `/opencode start` — as
the agent's own analysis does, when it tells the maintainer how to continue —
cannot start anything. Loop prevention therefore has three independent layers:
the marker, a probe against the text of our last reply (for when the marker gets
lost), and the anchored trigger.

### The task registry and the worktree

One task is one YouTrack issue. The registry
(`<DataDir>/registry.jsonl`, append-only) records, per task: the repository, the
worktree, the opencode session id, the agent, the state, the plan state, the
number of turns, the accumulated cost, and the agent's last reply text. State
moves `analyzing → awaiting_input → executing → done`, with `failed` for a turn
that broke.

The worktree is the real containment. Each task gets its own checkout from the
entry's `default_branch`, on its own branch, under the entry's `worktrees`
directory (which validation requires to be outside the repository). The agent
therefore cannot walk into another task's checkout, and the dispatcher refuses to
reuse a session when the routing table now points at a different repository than
the task was created against.

Turns are serialized **per runtime**, not per process. The reason a session cannot
be prompted twice at once is that a busy session silently swallows the second
prompt — and a session belongs to one runtime for life, so the runtime is the right
unit: an intake loop decides which runtime takes a delivery and hands it to that
runtime's own queue, and each runtime runs its turns with a single worker. Two
runtimes therefore work in parallel, one runtime still works one task at a time,
and `FLOWHUB_DISPATCH_QUEUE` bounds each queue (a full queue drops, it never
blocks the publisher).

### The agent definition

FlowHub passes `agent: devops` when it creates the session; the agent itself lives
outside this repository, in the opencode configuration
(`~/.config/opencode/agents/devops.md` on this host). That file owns the model,
the step budget and the durable part of the contract — untrusted issue text, only
your own worktree, never push, never touch secrets, one reply with the marker.
The per-turn prompt (`internal/source/youtrack/prompt.go`, the source adapter)
restates the parts that must hold even if the agent file is changed. A routing
entry may name its own `agent` instead, and that wins over
`FLOWHUB_DISPATCH_AGENT`.

The session-level ruleset in `internal/agent/opencode/runtime.go` stays the first
permission layer and the arbiter the second, in both phases. The execution phase
is the only phase in which `edit` is granted, which is what keeps an analysis turn
harmless even if the model decides to be helpful. Which tools are allowed at all,
which call counts as the reply and where a download may land come from the
source's `ToolPolicy` (`internal/source/youtrack/tools.go`); the dispatcher only
enforces them, so an adapter cannot widen the shell policy.

### Measured on this host

Runs on 2026-09-21 against opencode 1.18.31, TEST project, a repository whose only
file is `README.md`. Every row is from the application log, the task registry and
the YouTrack comments themselves. The **source** column matters: the first three
rows were driven by a recorded payload posted at the receiver, the rest arrived as
real webhooks from the YouTrack app through the gateway (`remote_ip 10.1.0.1`,
all three locks passing) on a receiver listening on the fixed LAN address.

| Source | Delivery | Action | Result |
| --- | --- | --- | --- |
| replayed payload | `issueCreated` TEST-13 | analyze | 19s, $0.0035, read-only, three bash permissions answered, one comment; no file changed |
| replayed payload | `commentAdded` `/opencode start` (plan on file) | execute | 26s, $0.0022, edited `README.md`, committed `d1bae41` on `flowhub/TEST-13` (signed, not pushed) |
| replayed payload | `issueUpdated` State → `In Progress` (no plan yet) | plan | 31s, $0.0051, plan comment with its blocking questions |
| **real webhook** | `issueCreated` TEST-17 | analyze | 14s, $0.0034, read-only analysis and one comment |
| **real webhook** | the agent's own comment coming back | ignore | `our own comment (contains <!-- flowhub-auto -->)` |
| **real webhook** | `commentAdded` `/opencode start` | execute | 22s, $0.0023, edited `README.md`, committed `3856d80` on `flowhub/TEST-17`, registry `state=done, turns=2, cost=$0.0057` |
| replayed payload | the agent's reply with the marker stripped | ignore | `our own comment (repeats our previous reply)` |
| replayed payload | a comment that merely mentions `/opencode start` | ignore | `no rule matched this event` — the trigger is anchored |
| **real webhook** | `issueCreated` TEST-18, after the sign-off change | analyze | 11s, $0.0031, reply ended with the blockquote below, rendered in Chinese |

Two arbiter decisions from the same run are worth keeping: `git remote -v` was
rejected by the deny list, and an absolute path (`ls -la /Users/…/TEST-14`) was
rejected as outside the worktree. In both cases the model read the reason and
rephrased, which is why a rejection is a safe default rather than a dead end. The
one rough edge: a `git commit -m "<multi-line message>"` is split on the newline
inside the message, so the second line is judged as a command and rejected; the
model recovered by sending a single-line subject.

### A leak this run found (and the fix)

The live run recovered the deployment's real URL key from FlowHub's own audit
log. `Handler.redact` replaced only the *configured* key, and the deliveries that
get audited are exactly the ones whose key did **not** match — so any receiver
started with a rotated or freshly generated key (which is what
`export FLOWHUB_HOOK_KEY=$(openssl rand -hex 32)` does on every restart) wrote
the app's live key into `data/webhook-*.jsonl` and `data/payload-*.log` in
plaintext. Redaction is now by *shape*: in a path, the segment after the hook base
is the key; in a query string it is `k`. It is applied whether or not lock 1 is
enabled, because a disabled lock is a configuration choice, not a licence to
store the secret. `TestRedactionNeverEchoesAnUnrecognisedKey` pins it.

### Stopping it

* `touch <FLOWHUB_PAUSE_FILE>` (default `<DataDir>/DISPATCH_OFF`) — every delivery
  is still audited, nothing runs. Remove the file to resume: no API, no restart,
  no credential.
* `FLOWHUB_DISPATCH=0` and a restart.
* `GET /healthz` shows `dispatch.{queued,handled,ignored,dropped,paused,agent}`
  plus a per-runtime view (`runtimes.<name>.{queued,in_flight,depth,last_error}`),
  so "which host is stuck" is one call; `-print-config` prints the effective policy.

## Endpoints

| Endpoint | Behaviour |
| --- | --- |
| `POST /hooks/youtrack/<key>` | The webhook entry point |
| `POST /hooks/youtrack?k=<key>` | Same, for deployments that prefer a query string (the app only requires a URL that is not comma separated) |
| `GET /healthz` | Local only; counters, active locks, queue depth, current audit file, dispatch counters and pause state. nginx does not proxy this path |

## Pipeline and rejection reasons

```
three locks -> body cap -> content type -> JSON -> known event -> replay window
            -> idempotency -> audit (background) -> 202 empty
```

Rejections are deliberately indistinguishable from success at the HTTP layer:
status `202`, empty body, no explanatory header. The reason is written only to the
local audit log and to the `/healthz` counters.

| Reason | Trigger |
| --- | --- |
| `method_not_allowed` | Anything but `POST` (answered `405`) |
| `bad_url_key` | Path/query key missing or wrong |
| `bad_header_token` | Token header missing or wrong (constant-time compare) |
| `source_not_allowed` | Source IP outside `FLOWHUB_ALLOWED_SOURCES` |
| `body_too_large` | Body above `FLOWHUB_MAX_BODY_BYTES` (answered `413`) |
| `body_read_error` | The body could not be read |
| `unexpected_content_type` | A `Content-Type` other than `application/json` was sent (a missing one is tolerated) |
| `invalid_json` | Body is not valid JSON |
| `unknown_event` | `event` is not one of the eleven events the app can send |
| `unparseable_timestamp` | `timestamp` is neither RFC 3339 nor epoch milliseconds |
| `outside_replay_window` | `timestamp` is further than the replay window from local time |
| `duplicate` | The idempotency key was seen within `FLOWHUB_DEDUPE_TTL` |

`event` values accepted: `issueCreated`, `issueUpdated`, `issueDeleted`,
`commentAdded`, `commentUpdated`, `commentDeleted`, `workItemAdded`,
`workItemUpdated`, `workItemDeleted`, `issueAttachmentAdded`,
`issueAttachmentDeleted`.

## Log files

Three files land in `FLOWHUB_DATA_DIR`, all written on the background queue
goroutine so they never delay the `202`:

| File | Audience | Contents |
| --- | --- | --- |
| `flowhub.log` | operator | Startup banner, one line per verdict, errors. Rotates by size (`flowhub.log.1`…`.N`), also tees to stderr |
| `payload-YYYY-MM-DD.log` | you, reading the real wire format | One verbose block per delivery: request line, source addresses, masked headers, locks, verdict, payload keys, a per-path type schema, and the pretty-printed body |
| `webhook-YYYY-MM-DD.jsonl` | machines | One JSON object per delivery, including the full raw body, for replay and `jq` |

```bash
# Watch deliveries as they arrive, readable form
tail -f data/payload-$(date -u +%F).log

# Watch verdict lines only
tail -f data/flowhub.log | grep -E 'accepted|rejected'
```

A block looks like this:

```
====================================================================================================
2026-09-20 10:04:50.496  ACCEPTED                   event=issueCreated
----------------------------------------------------------------------------------------------------
request   POST /hooks/youtrack/***
source    remote=10.8.0.2  x-forwarded-for=10.8.0.2  x-real-ip=10.8.0.2
client    user-agent=YouTrack/2026.3  content-type=application/json
body      305 bytes  sha256=bf1b9a25fd11cb2b…
locks     url_key=ok  header_token=ok  source_ip=ok  (configured: url_key,header_token,source_ip)
verdict   handled=0ms  dedupe=4fa5b6cb56e6a696…  clock_skew=496ms
issue     id=2-123  form=database  project=SP  summary=Fix login
actors    primary=jane.doe  all=jane.doe
payload   timestamp=2026-09-20T02:04:50.000Z  numberInProject_present=false  changedFields=-  comments=-
headers   (7, sensitive values masked)
          Content-Type: application/json
          User-Agent: YouTrack/2026.3
          X-Real-IP: 10.8.0.2
          X-YouTrack-Token: <masked len=64 sha256:a0d4eef3eaef>
token     X-YouTrack-Token present=true len=64 sha256:a0d4eef3eaef matches_configured=true literal_secret=false
keys      (8)  created, description, event, id, project, reporter, summary, timestamp
schema    (14 paths)
          created: number
          description: string
          event: string
          id: string
          project: object
          project.key: string
          ...
payload
  {
    "event": "issueCreated",
    ...
  }
```

Notes:

* **The `schema` block is the payload-analysis tool.** It lists every JSON path
  with the type found there and merges array elements, so one delivery tells you
  whether `project.id`, `numberInProject`, a user object's `id` or a polymorphic
  `changedFields[].value` are actually on the wire. Compare the output of
  different event types with `diff` to spot per-event differences.
* **Header names are printed in net/http canonical form** (`X-Real-Ip`), because
  that is how they are stored after parsing — except the token header, which is
  printed with your configured spelling.
* **No credential is written to disk.** The token header, `Authorization`,
  `Proxy-Authorization`, `Cookie` and `Set-Cookie` values are replaced by
  `<masked len=… sha256:…>`. `token_header.matches_configured` tells you whether
  the app sent your real token without storing it, and `literal_secret=true`
  catches the known platform bug (§4.2).
* The payload log truncates a body at `FLOWHUB_DETAIL_MAX_BODY_BYTES`; the JSONL
  record always keeps the whole body (up to `FLOWHUB_MAX_BODY_BYTES`).

## Audit log

One file per UTC day, `webhook-YYYY-MM-DD.jsonl`, one JSON object per delivery.
Accepted and rejected deliveries are both recorded, including the raw body.

```bash
# Live view
tail -f data/webhook-$(date -u +%F).jsonl | jq -c '{ts,accepted,reason,event,issue_id,primary_actor}'

# Rejections by reason
jq -r 'select(.accepted|not) | .reason' data/*.jsonl | sort | uniq -c | sort -rn

# Slowest deliveries (the app times out at 5s)
jq -r '.handled_ms' data/*.jsonl | sort -n | tail -3

# Compare the real payload shape across event types
jq -r 'select(.payload_schema) | "\(.event)\t\(.payload_schema | join(" "))"' data/*.jsonl | sort -u
```

## Receiver verification: closing the open questions

The design documents list facts that could only be confirmed by a real delivery
(§13). Every one of them is answerable from the logs without changing code.

| Open question | Where to look |
| --- | --- |
| Is the header token the configured value or the literal `secret`? (§4.2, §13.1) | `payload-*.log` → the `token` line (`matches_configured`, `literal_secret`) or `jq -r 'select(.token_header) \| [.token_header.matches_configured, .token_header.looks_like_literal_secret] \| @tsv' data/*.jsonl \| sort -u` |
| Is `numberInProject` really absent? (§5.2, §13.2) | `payload-*.log` → `numberInProject_present` / the `schema` block, or `jq -r '.has_number_in_project' data/*.jsonl \| sort \| uniq -c` |
| Does `project` carry `id`? Do user objects carry `id`? (§5.2) | `grep -E '^ +(project\|.*author\|.*By)\.' data/payload-$(date -u +%F).log \| sort -u` |
| What are the real types of `changedFields[].value`? (§5.4) | `payload-*.log` → `changedFields[]…` lines in the `schema` block |
| Is `issue.id` the database form (`2-123`) or the readable form (`SP-123`)? (§13.3) | `payload-*.log` → `issue … form=` or `jq -r '.issue_id_form // "not-parsed"' data/*.jsonl \| sort \| uniq -c` |
| What exactly is in the payload? | `jq -r 'select(.payload_keys) \| (.payload_keys \| join(","))' data/*.jsonl \| sort -u` |
| Which source IP does nginx present? (§13.6) | `payload-*.log` → the `source` line (`remote`, `x-forwarded-for`, `x-real-ip`) |
| Clock skew between YouTrack and this host | `payload-*.log` → `clock_skew=` |

Because a wrong assumption here silently drops events, `FLOWHUB_REPLAY_WINDOW`
can be set to `0` for the very first delivery, and `-print-config` shows which
locks and checks are active before going live.

## Deployment sketch

Per `youtrack-webhook-and-flowhub-security.md` §8.3, nginx terminates TLS and
does the coarse filtering; the service does the rest.

```nginx
location = /hooks/youtrack/<your-key> {     # one path, nothing else
    allow <youtrack_host_ip>;
    deny all;
    if ($request_method != POST) { return 405; }
    access_log off;                          # the key lives in this URL
    client_max_body_size 256k;
    limit_req zone=yt_hook burst=10 nodelay;
    proxy_pass http://<flowhub-addr>:8080;
    proxy_read_timeout 5s;                   # the app gives up after 5s
    proxy_connect_timeout 2s;
}
location / { return 404; }
```

FlowHub should listen on the WireGuard address, so the public internet has no
route to it at all. Do not put a CDN in front: it hides the real source IP and
breaks the allowlist.

### Wildcard binds need an explicit acknowledgement

A wildcard bind (`0.0.0.0:8080`, `:8080`, `[::]:8080` — the kernel treats all
three the same) makes the port answer on **every** interface the host is attached
to, including the office LAN and any Wi-Fi it joins. Measured behaviour with
`FLOWHUB_ADDR=0.0.0.0:8080`:

```
lsof: flowhub ... TCP *:18099 (LISTEN)
curl 127.0.0.1:18099/healthz     -> 200
curl 192.168.8.20:18099/healthz  -> 200     # the LAN address, no tunnel involved
```

So it is refused at startup unless it is acknowledged:

```bash
$ FLOWHUB_ADDR=0.0.0.0:8080 ./bin/flowhub
flowhub: refusing to start:
  - FLOWHUB_ADDR 0.0.0.0:8080 listens on every interface, which exposes the
    receiver on whatever network the host is attached to (LAN, Wi-Fi) instead of
    only the intended path: bind a specific address (the WireGuard address, the
    LAN address a router forwards to, or 127.0.0.1 behind a reverse proxy), or
    set FLOWHUB_ALLOW_WILDCARD_LISTEN=1 if a wildcard bind is genuinely required
    (for example inside a container)
$ echo $?
1
```

The refusal happens before any file is created, so a rejected start leaves
nothing behind. `-print-config` still works and renders the same text as a
`problem:` line, and `/healthz` reports `"listens_on_all_interfaces"`.

**In a container, acknowledge it** — inside the container the wildcard is the
only address that works, and the isolation comes from how the port is published:

```bash
docker run -d --name flowhub \
  -e FLOWHUB_ADDR=0.0.0.0:8080 \
  -e FLOWHUB_ALLOW_WILDCARD_LISTEN=1 \
  -e FLOWHUB_HOOK_KEY=... -e FLOWHUB_TOKEN=... \
  -e FLOWHUB_ALLOWED_SOURCES=172.17.0.0/16 \
  -p 127.0.0.1:8080:8080 \
  -v /srv/flowhub/data:/data -e FLOWHUB_DATA_DIR=/data \
  flowhub
```

`-p 127.0.0.1:8080:8080` publishes the port on the host loopback only, so the
container's wildcard is reachable from the host (or a reverse proxy on it) and
from nothing else. Never publish with `-p 8080:8080`, which binds the host side
to `0.0.0.0` and undoes the point of the acknowledgement.

Two non-container cases where a wildcard bind is also defensible:

* a **host firewall restricts the port** to the reverse proxy — binding the
  proxy's own address is still simpler;
* the host is **otherwise isolated** (a dedicated VM whose only interface is the
  private network). Note that this is a property of the host, not of the
  container, so it deserves the same explicit acknowledgement.

Binding a specific address remains the strongest option: it is also fail-closed,
because an address the host no longer has makes the process refuse to start
(`bind: can't assign requested address`) instead of silently listening
everywhere. For a laptop that moves between networks, that is the behaviour you
want — or bypass the problem entirely with an SSH reverse tunnel (design document
§8.5): `ssh -N -R 127.0.0.1:18080:127.0.0.1:8080 aliyun`, then bind
`127.0.0.1:8080` and let the tunnel work from any network.

## Development

```bash
make build        # bin/flowhub; version, commit and build time are stamped in
make test         # unit tests (seed corpus of the fuzz target included)
make test-race    # race detector; needs cgo, which the Nix shell leaves on
make test-repeat  # same suite three times to catch leaked global state
make lint         # go vet
make staticcheck  # pinned staticcheck
make fmt          # gofmt -w on the Go sources
make fmt-check    # fail if any Go file is not gofmt-clean
make smoke        # end-to-end: real process, real HTTP, asserts the logs
make secrets      # print a fresh FLOWHUB_HOOK_KEY / FLOWHUB_TOKEN pair
make fuzz         # fuzz the webhook parser (FUZZTIME=30s)
make test-live    # live turn against opencode (OPENCODE_LIVE=1, spends tokens)
make all          # cross-compile linux-amd64, linux-arm64, darwin-arm64
make releases     # cross-compile and produce .tar.gz archives
```

`make smoke` is the fastest way to see the whole system work: it starts the
binary on a loopback port with throwaway secrets, posts one delivery per
interesting shape (accepted, duplicate, bad token, literal `secret`, bad URL key,
unknown event, invalid JSON, stale timestamp, wrong method), then asserts the
HTTP responses, the audit reasons, the payload schema, the log file modes and
that neither secret reached disk. It cleans up its temporary directory.

### Toolchain

`flake.nix` + `.envrc` give a reproducible Go 1.27 dev shell via Nix and direnv
(`direnv allow` once, or `nix develop`). The shell pins `GOTOOLCHAIN=local`,
unsets any host `GOROOT` and keeps `CGO_ENABLED` at the platform default so the
race detector works; release and cross builds set `CGO_ENABLED=0` in the
Makefile. `flake.lock` pins the exact nixpkgs revision.

Layout:

| Path | Responsibility |
| --- | --- |
| `cmd/flowhub` | Wiring, flags, HTTP server, `/healthz`, graceful shutdown |
| `internal/config` | Environment parsing, validation, masked reporting |
| `internal/logging` | Application logger: stderr plus a size-rotated log file |
| `internal/event` | The neutral event IR every source decodes into and the core consumes |
| `internal/source` | The event-source seam: `Request`, `Decoded`, `Source`, and the data-only tool policy |
| `internal/source/youtrack` | The YouTrack adapter: payload model, payload schema report, neutral-event decoding, tool allowlist, per-turn prompt |
| `internal/webhook` | The delivery pipeline: entry locks, body limits, source decode, replay window, idempotency, audit, redaction |
| `internal/dedupe` | TTL idempotency cache |
| `internal/store` | Audit record, non-blocking queue, daily JSONL writer, human readable payload log |
| `internal/projectmap` | The configuration file: v1/v2 loader with the v1 translation, `sources`/`runtimes`/`projects`, the `(source, project)` index, per-level policy precedence with provenance, canonical paths, fail-closed validation, runtime addressing |
| `internal/rules` | Trigger policy over the neutral event: ignore/analyze/plan/execute, self-comment detection, turn budget, reply basis |
| `internal/agent` | The runtime seam: `Runtime`, the neutral `Turn`/`Result`, the turn phase and the download policy |
| `internal/agent/opencode` | opencode client, permission arbiter, session ruleset and the runner that drives one unattended turn |
| `internal/workspace` | The workspace seam: `Workspace`, `Handle`, the pinned `Base`, and entry validation |
| `internal/workspace/localworktree` | The co-located provider: one git worktree per task, baseline pinned through the origin |
| `internal/dispatch` | The workers: intake loop, one queue and worker per runtime, worktree, session, registry, audit |
| `internal/metrics` | Counters used by `/healthz` |
| `config/config.example.json` | Committed version 2 template for the configuration file |
| `scripts/smoke.sh` | The end-to-end check behind `make smoke` |

`AGENTS.md` records the project invariants that contributors and coding agents
must preserve; `CODEBASE.md` is the file-level map.

## License

MIT — see [`LICENSE`](./LICENSE). Copyright (c) 2026 Siwen Yu.

## Not implemented yet

* Structured "are there blocking questions?" output. Today the agent is asked to
  say so in prose and a human starts the implementation; a later version can use
  opencode's `json_schema` output format to gate `/opencode start` on a machine
  readable answer.
* **Enforced refusal of credential-shaped reads.** opencode's own `read *.env`
  gate is overridden by FlowHub's session ruleset (`read` is allowed so the agent
  can read source), and the arbiter allows the `read` tool by name without looking
  at the path. The prompt and the agent definition both forbid reading secrets, so
  the layer that is missing is the mechanical one; closing it needs the shape of a
  `read` permission request measured first, then a pattern check in the arbiter.
* Reconciliation polling: a delivery lost while FlowHub was down is lost, because
  the published webhook app does not retry. A periodic sweep of issues in the
  start states would close that gap.
* FlowHub posting the reply itself (today the agent posts it through the YouTrack
  MCP server, so FlowHub holds no YouTrack credential).
* More than one dispatch worker, which needs per-task locking first.
* Phase 3: alerting, key rotation, and optional HMAC signing through a custom
  workflow rule.
* Further event sources (Gitea, Drone), further agent runtimes, and running them on
  other hosts: proposed in
  [`docs/adr/0001-pluggable-sources-and-runtimes.md`](./docs/adr/0001-pluggable-sources-and-runtimes.md)
  (the seam; a **v2 configuration format** with `sources`, addressed `runtimes` and
  the project table; a pinned base commit per task) and
  [`docs/adr/0002-data-plane-runtime-installation.md`](./docs/adr/0002-data-plane-runtime-installation.md)
  (`flowhub runtime init` / `invite`, the admin API, the artifact manifest).
  ADR 0002 steps 1–5 are implemented (`init` / `--check` / `doctor --push`,
  `invite`, the admin API, the artifact manifest, the activation check); the
  per-project runtime list and the `spread` / `first-healthy` policy of ADR 0001
  step 5 are not — today the dispatcher takes the first healthy runtime in name
  order for a new task, and a task that is already bound stays where it is.
