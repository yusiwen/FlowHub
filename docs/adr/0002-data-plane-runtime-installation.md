# ADR 0002 — Data-plane runtime installation and enrollment

**Status:** Proposed, partially implemented. The five decisions below were approved
in review on 2026-09-22, and **migration step 1 — `flowhub runtime init --check` —
is implemented**; steps 2–5 are not started.
**Revision:** 1
**Date:** 2026-09-22
**Depends on:** [ADR 0001](./0001-pluggable-sources-and-runtimes.md) — it settles
the topology (one control plane, one or more agent hosts) and the seams; this ADR
settles how a host becomes one of those runtimes and stays correct afterwards.

## Context

ADR 0001 says a runtime is a named block addressed by the dispatcher, and that the
workspace belongs to the host that owns the filesystem. It does not say how that
host gets into a state where it can run a turn. Today the honest answer is "a human
or an agent edited files on it": the `devops` agent definition on the development
host was written by hand during this project, and nothing verifies that a host has
the command-line tools the workflow needs.

That produces two failures. A second host cannot be reproduced from anything in the
repository, and the checks that decide whether a host is usable happen in the wrong
place — the control plane reaching into the data plane, or a probe running as the
wrong user with the wrong `HOME`, which is how a check passes and the first real
push then fails with a 401.

## Decision drivers

1. **Reproducible and idempotent.** One command turns a host into a runtime, and
   running it again changes nothing unless something drifted.
2. **Admin-initiated enrollment.** Registering a runtime is handing a machine work
   to execute; it is an authorization decision, so it starts on the control plane.
3. **Content comes from the binary.** What `init` installs is effectively a
   privileged component: the agent file *is* the agent's system prompt and the MCP
   block decides which tools exist. A compromised control plane must not be able to
   push prompts to every worker.
4. **Nothing hand-maintained is clobbered, and everything installed is removable.**
5. **The data plane initiates contact.** Workers reach the control plane; the
   control plane does not need inbound access to workers.
6. **Secrets stay in the environment.** FlowHub still holds no tracker credential.
7. **One binary, two roles.** No new always-on component on the data plane, so
   there is one artifact and one version to reason about.

## Decision

### One binary, two roles

| Runs on | Commands |
| --- | --- |
| the control plane | `flowhub serve`, `flowhub runtime invite\|list\|show\|remove\|reconfigure\|rotate` |
| a data-plane host | `flowhub runtime init`, `flowhub runtime doctor`, `flowhub runtime uninstall` |

`init` and `doctor` are the two commands a worker host ever needs; everything else
is the operator talking to the inventory.

### The runtime inventory is owned by the service

`runtimes.json` (ADR 0001) is not hand-edited, and not written by two writers. The
serving process is the only writer: the CLI calls the admin API, the registration
endpoint calls the same code, and both persist through it. A mutation is applied to
the in-memory runtime set immediately, so adding or removing a runtime does **not**
need a restart — the earlier "restart and hope" caveat is gone.

An entry carries a state, because enrollment is a two-step handshake:

```json
{
  "_comment": "Written by the service. Never hand-edit; use `flowhub runtime`.",
  "version": 1,
  "runtimes": {
    "builder-a": {
      "state": "active",
      "url": "https://builder-a.lan:4096",
      "advertise": "https://builder-a.lan:4096",
      "agent": "opencode",
      "agent_version": "1.18.31",
      "flowhub_version": "v0.3.0",
      "workspace": { "provider": "remote", "runner": "https://builder-a.lan:8443", "base": "/srv/flowhub-worktrees" },
      "max_concurrent": 1,
      "registered_at": "2026-09-22T08:10:00Z",
      "last_seen": "2026-09-22T08:40:11Z",
      "report_sha256": "…"
    },
    "builder-b": {
      "state": "pending",
      "invite": { "token_sha256": "…", "expires_at": "2026-09-22T08:40:00Z", "projects": ["BEAP_BE"] },
      "secret_sha256": "…"
    }
  }
}
```

`pending → active` happens only after the service has proved it can reach the
advertised URL, so a NAT or firewall mistake fails at enrollment instead of at the
first task. `revoked` is a terminal state that keeps the name reserved. Invite
tokens and runtime secrets are persisted as hashes only, exactly like the webhook
URL key.

### The admin API

A second listener, because the webhook entry and the control API have opposite
semantics: the webhook answers `202` to everything and explains nothing, and the
admin API must return real errors and refusals.

| Setting | Behaviour |
| --- | --- |
| `FLOWHUB_ADMIN_ADDR` | Default `127.0.0.1:8081`. A wildcard bind is refused unless the existing acknowledgement variable is set; the project's "bind a specific address" rule applies unchanged |
| `FLOWHUB_ADMIN_TOKEN` | Required. The admin listener **refuses to start without it** — a silently unauthenticated control surface is the worst outcome, the same rule that governs the entry locks |
| transport | Loopback by default; a LAN or WireGuard address when workers must reach it; TLS terminates in front (or the link rides WireGuard) |

| Endpoint | Auth | Purpose |
| --- | --- | --- |
| `POST /control/v1/runtimes/register` | invite token | A worker claims a pending name and submits its capability report |
| `POST /control/v1/runtimes/{name}/heartbeat` | runtime secret | `doctor` pushes a fresh report; updates `last_seen` |
| `POST /control/v1/runtimes/{name}/rotate` | admin token | Re-issue the runtime secret |
| `GET /control/v1/runtimes` | admin token | The inventory, without hashes or secrets |
| `DELETE /control/v1/runtimes/{name}` | admin token | Remove; refuses while non-terminal tasks are bound, `?force=1` overrides without re-homing them |

### Enrollment, end to end

1. **`flowhub runtime invite builder-a --projects BEAP_BE --ttl 30m`** on the control
   plane. This creates the `pending` entry and prints a one-time token. The invite
   response also carries the **repository identities** (remote URL and default
   branch) of the named projects, so the worker knows what to check without the
   operator retyping anything, and so the check is derived from the routing table
   rather than from memory.
2. **`flowhub runtime init --server … --token … --agent opencode`** on the worker.
   It verifies locally, installs, reports, then registers (§ below).
3. **The service probes `--advertise`** (`/global/health`, version compatibility)
   and refuses to activate a host it cannot reach or whose agent is too old. The
   refusal is returned to `init`, which prints it.
4. **The worker keeps a long-lived secret** for heartbeat and re-registration; the
   service stores only its hash.
5. **Revocation** is `DELETE`. A revoked host's next heartbeat gets a refusal that
   names the reason, and its tasks are refused at their next turn rather than
   re-homed (ADR 0001's sticky-binding rule).

### What `init` does, in order

| Step | Action | On failure |
| --- | --- | --- |
| 1 | Detect the agent runtime (`opencode` binary, version, config directory) | Print install guidance and stop. `init` **never installs the agent itself** — package managers, users and versions differ per host |
| 2 | Check the local toolchain: `git`, and per forge the CLI (`gh` for GitHub, `tea` for Gitea), including **which account and which config file** they are authenticated as | List exactly what is missing |
| 3 | Check the repositories it was invited for: clone exists, `origin` matches, `git ls-remote` works, and **`git push --dry-run` succeeds** | List per repository. A read-only clone is a failure, not a warning: the workflow pushes |
| 4 | Write the embedded artifacts (agent definition, skills) | — |
| 5 | MCP configuration: **print the snippet and the diff**; only create the file when it does not exist, never edit a hand-maintained `opencode.json`/`jsonc` | Report the manual step |
| 6 | Check that required environment variables are present **without reading their values** (in particular the YouTrack token the MCP server needs) | Warn; the agent cannot reply without it |
| 7 | Assemble the capability report, then register | Print the service's refusal |

Steps 1–3 and 6 are also available as `flowhub runtime init --check`: a read-only
report that writes nothing and registers nothing. That is deliberately the first
thing implemented, because it is useful on its own and touches no existing code
path.

### Installed artifacts and the manifest

Every file `init` writes is recorded with its path, its SHA-256 and the FlowHub
version that wrote it, in a manifest next to the artifacts:

* file hash equals the manifest's → overwrite silently (idempotent re-run)
* file was modified by hand → **refuse and show the diff**; `--force` overwrites
* file is in the manifest but missing → restore and say so

`flowhub runtime uninstall` removes exactly the manifest's files and nothing else.
A host can therefore be returned to its pre-`init` state, which is what makes the
privileged installer acceptable to run at all.

### MCP configuration, v1

Opencode loads a fixed set of config file names, so a separate include file is not
an option, and those files may contain comments — a plain JSON round-trip would
destroy the operator's configuration. Therefore, in v1: print the exact snippet and
the diff, create the file only when absent, and never rewrite one that exists. A
JSONC-preserving merge is a later, separate decision.

### The capability report

Machine-readable, secret-free, and the same document the CLI prints:

```json
{
  "collected_at": "2026-09-22T08:09:00Z",
  "invoker": { "user": "yusiwen", "uid": 501, "home": "/Users/yusiwen" },
  "host": { "hostname": "builder-a", "os": "linux", "arch": "arm64" },
  "agent": { "name": "opencode", "version": "1.18.31", "config_dir": "~/.config/opencode" },
  "tools": {
    "git": { "path": "/usr/bin/git", "version": "2.43.0" },
    "tea": { "path": "/usr/local/bin/tea", "version": "0.9.1", "config_file": "~/.config/tea/config.yml", "logins": ["git.yusiwen.cn"] }
  },
  "repos": [
    { "remote": "git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git", "clone": "/srv/repos/beap-be",
      "origin_matches": true, "ls_remote_ok": true, "push_dry_run": "ok" }
  ],
  "artifacts": [ { "path": "~/.config/opencode/agents/devops.md", "sha256": "…", "managed": true } ],
  "env_present": { "YOUTRACK_TOKEN": true },
  "warnings": ["gh is not installed; GitHub-hosted projects cannot open pull requests on this host"]
}
```

Two properties matter more than the field list. The report records **the identity it
ran as** (`invoker.user`, `invoker.home`), which lets the service compare it with
the `HOME` its later turns actually run with — the mismatch that makes a check pass
and the first push fail. And it contains no tokens: config file *paths* and account
*names*, never values.

### What `init` deliberately does not do

* install or upgrade the agent runtime itself
* rewrite a hand-maintained agent config file
* fetch prompts, agents or skills from the network: they are embedded in the binary,
  so updating a fleet means shipping the binary and re-running `init`
* read, generate or transmit tracker credentials
* run a turn

### Relationship to the agent-session probe

An earlier review idea was to check a runtime by creating a temporary session and
asking the agent what it finds. That remains useful, but its role changes: `init`
answers the install-time questions far more cheaply (no model call, and it runs as
the user who will actually work), while an end-to-end smoke turn is the only thing
that proves *a turn can run here at all* — the agent exists, bash works, the MCP
server is reachable. So the smoke turn becomes an optional activation check on the
service side, not the primary probe. Liveness for candidate selection stays the
cheap `/global/health` call, never a model turn.

## Consequences

* A new host is one command plus the operator's own credentials, and the same
  command verifies it; a fleet can be rebuilt from the binary and the invite.
* The control plane gains an authenticated write surface, which is new attack
  surface. It is bounded by: loopback by default, a mandatory admin token, hashed
  invite/secret storage, hashes-only reports, and content that never travels in.
* Enrollment state lives in a second file that must survive a restart, so
  `runtimes.json` becomes real state rather than a cache of the operator's intent.
* Hosts can still drift from each other (different tool versions, different
  provisioning). The report makes drift visible; it does not fix it.
* Still no credential for FlowHub: the report records only whether an environment
  variable is present.

## Non-goals

1. Auto-installing the agent runtime, or managing it with a package manager.
2. Remote distribution of prompts/skills in v1; a signed fetch is a later decision.
3. A JSONC-preserving config merge in v1.
4. Any user interface beyond the CLI.
5. Fleet-wide orchestration (draining, rolling upgrades); workers are enrolled by
   running `init` on them.

## Migration plan

Each step is independently committable and must leave `make fmt-check lint
staticcheck test test-race smoke` green.

| Step | Content | Verification |
| --- | --- | --- |
| 1 | `flowhub runtime init --check` only: detect the agent, check tools, forges and repositories, print the report; writes nothing, registers nothing | Run on this development host and on a Linux worker; the report matches what a human finds by hand, and `--check` leaves no file behind |
| 2 | Embedded artifacts, manifest, drift refusal, `--force`, `uninstall` | Re-running `init` is a no-op; a hand-edited artifact is refused with a diff; `uninstall` restores the pre-`init` file set exactly |
| 3 | `runtimes.json` with states, the admin listener and token, `invite`/`list`/`show`/`remove`, and hot application to the dispatcher | Invite then register a host; the inventory shows `pending` then `active`; removing a runtime with a bound task is refused without `force`; the change takes effect without restarting the service |
| 4 | Capability checks wired into `init` (forges, auth, clone, dry-run push) and `doctor --push` | A host missing `tea`, or with a read-only clone, is refused with a message naming the gap; an expired credential is caught by `doctor`, not by the first task |
| 5 | Optional activation smoke turn on the service side | A host whose opencode answers but whose agent is missing fails activation with that reason |

## Open questions

1. **Admin token provisioning.** Environment only, or also generated on first start
   into a `0600` file so a fresh install works without the operator inventing one?
   Recommendation: environment only, and refuse to start without it — an
   auto-generated token in a file is one more secret on disk to find and rotate.
2. **Should `doctor` repair, or only report?** Recommendation: `doctor` reports and
   `init` installs, with `init --refresh` as the explicit repair path, so a routine
   check never writes.
3. **What does a stale heartbeat mean?** Options: mark the runtime `degraded` and
   keep using it, or stop selecting it for new tasks. Recommendation: exclude a
   runtime from selection after a configurable two missed intervals, but never move
   a task already bound to it — the binding rule from ADR 0001 stays absolute.
4. **Secret rotation direction.** Admin-initiated only, or can a worker re-enroll
   itself with a new invite when its secret is lost? Recommendation: admin-initiated
   `rotate`, plus re-invite for a lost secret, so a compromised worker cannot mint
   itself new credentials.
