# FlowHub Operations Manual

An end-to-end guide for installing, configuring and operating FlowHub: the
YouTrack side, the opencode side, and the FlowHub service itself. It assumes no
prior knowledge of the project. Every command is meant to be copy-pasteable;
where a value depends on your environment it is written as `<UPPER_CASE>`.

Read `README.md` for the reference tables (every environment variable, every
rejection reason). This manual is the *procedure*: what to do, in what order, and
how to tell that it worked.

---

## Table of contents

1. [Deployment models — pick one first](#1-deployment-models--pick-one-first)
2. [Prerequisites](#2-prerequisites)
3. [Prepare the opencode side](#3-prepare-the-opencode-side)
4. [Prepare the YouTrack side](#4-prepare-the-youtrack-side)
5. [Write the routing table](#5-write-the-routing-table)
6. [Build and verify the binary](#6-build-and-verify-the-binary)
7. [Phase 1: run the receiver only](#7-phase-1-run-the-receiver-only)
8. [Phase 2: turn the dispatcher on](#8-phase-2-turn-the-dispatcher-on)
9. [Multi-host: enrol a runtime](#9-multi-host-enrol-a-runtime)
10. [Deploy as a service](#10-deploy-as-a-service)
11. [Day-2 operations](#11-day-2-operations)
12. [Troubleshooting playbook](#12-troubleshooting-playbook)
13. [Appendix: file, port and secret inventory](#13-appendix-file-port-and-secret-inventory)

---

## 1. Deployment models — pick one first

FlowHub is one binary with two halves. Choose the model before configuring
anything, because it decides which variables you need.

| Model | What runs | Good for | Required |
| --- | --- | --- | --- |
| **A. Receiver only** | The webhook receiver, recording every delivery | Your first day. Observing real traffic, answering "what does YouTrack actually send" | Nothing but the three locks |
| **B. Single host (receiver + dispatcher)** | Everything on one machine: receiver, dispatcher, opencode | The normal setup: one builder | Model A plus `FLOWHUB_DISPATCH=1` and a running `opencode serve` |
| **C. Control plane + builders** | Receiver/dispatcher on one host, agent turns on one or more enrolled hosts | Several repositories, or a laptop that must hold the clones | Model B plus the control API (`FLOWHUB_ADMIN_ADDR`, `FLOWHUB_ADMIN_TOKEN`) and `flowhub runtime init` on each builder |

```text
YouTrack ──webhook──▶ nginx ──▶ FlowHub receiver ──▶ audit log
                                     │
                                     │ (FLOWHUB_DISPATCH=1)
                                     ▼
                              dispatcher ──▶ git worktree (one per task)
                                     │
                                     ▼
                              opencode serve ──▶ devops agent ──▶ YouTrack comment
```

Two hard facts drive the whole design, and they are not bugs:

* the YouTrack app posts **synchronously with a 5 s timeout and no retry**, so
  FlowHub answers in milliseconds and does the work in the background;
* an event that arrives while FlowHub is down **is lost**. There is no
  reconciliation yet. Do not deploy this expecting guaranteed delivery.

---

## 2. Prerequisites

### 2.1 Build host

| Requirement | Why | Check |
| --- | --- | --- |
| Go 1.24 or newer (1.27 recommended) | to build | `go version` |
| `git` | worktree per task, baseline pinning | `git --version` |
| `openssl` | generating the shared secrets | `openssl version` |
| Nix (optional) | the pinned dev shell | `nix --version` |

With Nix, `direnv allow` once or `nix develop` gives you the whole toolchain.
Without it, any Go 1.24+ toolchain works.

### 2.2 Service host (where FlowHub runs)

* Reachable from YouTrack's network — through nginx or a VPN.
* A writable data directory (`FLOWHUB_DATA_DIR`, default `./data`). It will hold
  the audit logs, the task registry and the runtime inventory. Files are created
  `0600`, the directory `0700`.
* For model B/C: a local checkout of every repository FlowHub will operate on,
  with a working `origin` remote and push access.

### 2.3 YouTrack

* A self-hosted or Cloud instance where you can install an app and edit projects.
* The official **Webhook Triggers** app (plugin `29469`).
* Two things that will bite you if you skip them:
  * the app's own SSRF check **rejects private IPv4 literals** (`10.x`, `192.168.x`,
    `172.16-31.x`, `169.254.x`). You must use a **domain name**, even if it resolves
    to a private address;
  * the URL **must not contain a comma** (the app separates multiple URLs with
    commas). Use a hex key.

### 2.4 opencode

* The `opencode` CLI on the host that runs turns, version 1.18 or newer.
* `opencode serve` listening on a port FlowHub can reach.
* A model provider configured and working (`opencode` auth, or a self-hosted
  OpenAI-compatible endpoint).

```bash
opencode --version
opencode serve --hostname 127.0.0.1 --port 4096 &
curl -s http://127.0.0.1:4096/global/health
```

---

## 3. Prepare the opencode side

Do this **before** the FlowHub side: `flowhub runtime init --check` verifies this
setup, and it is much easier to fix here than to debug through a failed turn.

### 3.1 Install the FlowHub agent and MCP snippet

FlowHub embeds the agent definition and the MCP snippet in the binary; nothing is
downloaded. The installer writes them and records every file in a manifest.

```bash
flowhub runtime init --check      # report only: writes nothing, registers nothing
```

Exit status: `0` ready, `2` not ready, `1` usage mistake. Add `--json` for a
machine-readable report, `--strict` to treat warnings as failures, and
`--config-root <dir>` to install into a throwaway tree instead of the real one.

**Read the warnings, not just the exit code.** A check that could not run is
deliberately *not* a failure: on the reference host `runtime init --check` exits
`0` while warning that the model catalogue could not be read (`GET /provider: HTTP
401`), because the agent server had not been given credentials. The turn would
still run — but the profile's model would be unverified, which is exactly the
failure mode `--strict` exists to catch. Use `--strict` in CI or before enrolling
a host, and treat an "not checked" line as unfinished setup.

When the report looks right:

```bash
flowhub runtime init
```

This installs:

| Target | Content |
| --- | --- |
| `~/.config/opencode/agents/devops.md` | the agent definition (model, step budget, permission block, system prompt) |
| `~/.config/opencode/opencode.json` | the MCP snippet — **printed, not merged**, when the file already exists |

The manifest is `~/.config/flowhub/manifest.json`. A file a human edited is
**refused with a diff**, never silently overwritten; `--force` overwrites.

Merging the printed snippet is not optional for a working deployment: without the
YouTrack MCP server the agent can read the repository but has **no way to post its
reply**, so every turn ends without a comment. §3.2 shows the block.

### 3.2 Add the YouTrack MCP server

The agent posts its reply through a YouTrack MCP server. FlowHub deliberately
holds no YouTrack credential, so this is the *agent's* credential, in the agent's
configuration.

Merge the snippet `runtime init` prints (or the file
`internal/provision/assets/opencode/mcp.json`) into `~/.config/opencode/opencode.json`:

```json
{
  "mcp": {
    "youtrack": {
      "type": "remote",
      "url": "https://<YOUR_YOUTRACK_HOST>/mcp",
      "headers": {
        "Authorization": "Bearer {env:YOUTRACK_TOKEN}"
      }
    }
  }
}
```

Then export `YOUTRACK_TOKEN` for the opencode process. On this host that lives in
`~/.config/opencode/.env`; a systemd unit would use `EnvironmentFile=`.

**Never put the token value in `opencode.json`** — the file is read by humans and
may be committed. `{env:...}` is resolved by opencode at load time.

### 3.3 Verify the agent is registered

```bash
curl -s http://127.0.0.1:4096/agent | python3 -m json.tool | grep -A2 '"devops"'
```

**The agent registry is cached per server process and per directory instance.** A
newly written or edited agent file is not visible until `opencode serve` restarts
and a *new* directory instance opens it. Per-task worktrees are always new, so
production is unaffected — but when you test against an existing repository path
directly, restart the server first.

### 3.4 Decide the model and pin it

Pick a model that is fast and cheap; turns are frequent and mostly read-only.
The agent file ships with `deepseek/deepseek-flash`; change it to whatever your
provider offers, spelled `provider/model-id`.

The dispatcher **pins the model the runtime reported** at `init` on every session
it creates. That is deliberate: the agent registry is cached, so a repaired
profile would otherwise be verified by the check and ignored by the turn. If you
change the model in the agent file, re-run `flowhub runtime init` (or
`runtime doctor`) so the reported model matches.

---

## 4. Prepare the YouTrack side

### 4.1 Install and configure the Webhook Triggers app

1. `Administration → Apps → Marketplace` → install **Webhook Triggers**
   (plugin `29469`).
2. Attach it to the project(s) FlowHub will serve:
   `Administration → Apps → Webhook Triggers → Manage projects`.
3. Open the project's configuration:
   `项目 → Settings → Apps → Webhook Triggers → Settings`.

Fill in:

| Setting | Value |
| --- | --- |
| `webhookToken` | the value of `FLOWHUB_YOUTRACK_TOKEN` (≥32 chars) |
| `headerName` | `X-YouTrack-Token` (must equal `FLOWHUB_TOKEN_HEADER`) |
| `webhooksOnIssueCreated` | `https://<YOUR_ENTRY_HOST>/hooks/youtrack/<FLOWHUB_HOOK_KEY>` |
| `webhooksOnCommentAdded` | same URL |
| `webhooksOnIssueUpdated` | same URL |
| the remaining event fields | same URL (the full set is listed in `README.md`) |

Notes that save time later:

* The URL must be a **domain**, not a private IP literal (see §2.3).
* The URL must not contain a comma.
* Put the same URL in `webhooksOnAllEvents` only if you want a catch-all; the app
  de-duplicates it against the per-event fields.
* The app sends **11 event types**. `corrupt` events do not exist; anything else
  is rejected by FlowHub as `unknown_event`.

### 4.2 Pick your entry topology

Three viable options, in decreasing strength:

1. **nginx with an IP allowlist** (recommended). Public TLS endpoint, but only
   YouTrack's host may reach it. Skeleton in §10.3.
2. **VPN-only** (WireGuard/Tailscale). FlowHub binds the VPN address; the public
   internet has no route. Strongest, needs YouTrack to reach your VPN.
3. **Loopback behind a reverse proxy on the same host**. Only if YouTrack and
   FlowHub share a machine.

Do **not** put a CDN in front: it hides the real source IP and breaks lock 3.

---

## 5. Write the routing table

FlowHub refuses to guess which repository an issue belongs to. That mapping is a
file you write; an unmapped project is refused, never defaulted.

### 5.1 Create the file

```bash
mkdir -p ~/.config/flowhub
cp config/config.example.json ~/.config/flowhub/config.json
chmod 600 ~/.config/flowhub/config.json
```

The default path is `~/.config/flowhub/config.json`
(`$XDG_CONFIG_HOME/flowhub/config.json` when set). `FLOWHUB_CONFIG_FILE`
overrides it. The file lives outside the repository on purpose: it is
host-specific.

> **Known rough edge in the template.** `config/config.example.json` references
> `prompt_file` paths (`prompts/youtrack.md`, `prompts/test-project.md`) that do
> not exist in the repository. A `prompt_file` is validated at load time and a
> missing file **stops the start**. Either create those files or **delete the
> `prompt_file` lines** from your copy. Deleting them is the fast path.

### 5.2 The minimum that works

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
    "local": {
      "url": "http://127.0.0.1:4096",
      "agent": "devops",
      "deadline": "15m"
    }
  },

  "projects": [
    {
      "source": "youtrack",
      "project": "<YOUTRACK_PROJECT_KEY>",
      "repo": {
        "path": "<ABSOLUTE_PATH_TO_LOCAL_CLONE>",
        "remote": "git@<your-git-host>:owner/repo.git",
        "default_branch": "main"
      },
      "worktrees": "<ABSOLUTE_PATH_OUTSIDE_THE_CLONE>",
      "runtime": "local"
    }
  ]
}
```

What each value means, and how to get it right:

| Field | Rule |
| --- | --- |
| `project` | The **project key** as it appears in issue IDs (`BEAP-12` → `BEAP`). Case-insensitive. When a payload carries both `project.key` and `project.shortName`, `key` wins. |
| `also` | Extra project keys that share this repository. All of them map to the same entry. |
| `repo.path` | Absolute, canonicalised on load. Must be a directory, a git work tree, and its `origin` must equal `remote`. |
| `repo.remote` | Compared against `.git/config`. **Leave it out to skip the check — don't, it is what stops a typo from routing work into the wrong clone.** |
| `repo.default_branch` | Resolved through the origin to a commit when a task is created, then pinned for the task's life. Required when dispatch is on. |
| `worktrees` | Must be **outside** `repo.path` (or a sibling). One checkout per task lands here. Required unless `FLOWHUB_WORKTREE_BASE` is set. |
| `runtimes.<name>.url` | The opencode server to talk to. Omit it only when the runtime is enrolled through the control plane. |
| `runtime` / `runtimes` | An **eligibility set**, not an order: a project naming none may be served by any configured runtime. `runtime_policy` (`spread`, the default) decides which one takes a new task. |
| `agent` | The agent profile name inside opencode (`devops`). **Never the product name** — asking opencode for an agent called "opencode" silently falls back to its own default agent, which is looser. |
| `max_concurrent` | How many **distinct tasks** a host serves at once, `1`..`64`. A single task is never in two turns at once. |

By default no secret goes in this file: locks live in the environment. A runtime's
Basic Auth password has four possible sources — see §5.3 — and only the inline one
puts a secret here, which then requires mode `0600`.

### 5.3 Credentials for the agent server

If `opencode serve` was started with `OPENCODE_SERVER_PASSWORD` set, every call to it
must carry Basic Auth; `/global/health` answers `401` without it. Pick **one** source
per runtime — naming two is refused at load:

| Source | Written in the runtime block | Secret lives in |
| --- | --- | --- |
| environment (default) | `"auth": {"user": "opencode", "password_env": "FLOWHUB_LOCAL_OPENCODE_PASSWORD"}` | the process environment |
| shared file | `"auth": {"user": "opencode"}` plus top-level `"credentials_file": "credentials.json"` | that file |
| one file | `"auth": {"user": "opencode", "password_file": "local.pass"}` | that file |
| inline | `"auth": {"user": "opencode", "password": "…"}` | the configuration file itself |

The shared file is the one to use when several agents exist:

```json
// ~/.config/flowhub/credentials.json  — chmod 600
{
  "runtimes": {
    "local": { "user": "opencode", "password": "…" },
    "builder-a": { "user": "opencode", "password": "{env:TEAM_OPENCODE_PASSWORD}" }
  }
}
```

Rules that decide whether the start is allowed:

* **`chmod 600`.** The credentials file, every `password_file`, and the
  configuration file *when it carries an inline `password`* are refused if group or
  other can read or write them. A file that only names variables and paths is not
  checked. (macOS ACLs are outside the permission bits this reads.)
* Paths resolve like `prompt_file`: relative to the configuration file, `~`
  expanded.
* The file is read **once**, at startup — rotating a password needs a restart.
* `{env:VAR}` is expanded then, and an unset or empty variable refuses the start
  rather than becoming an empty password.
* A runtime that names a `user` with no resolvable password refuses the start. If
  you configured a password some other way and see the refusal, run
  `flowhub -print-config` — it names the source in force, never the value.

### 5.4 Validate it

```bash
./bin/flowhub -print-config
```

This prints each effective value **and the level that supplied it** (trigger,
locked masks, dispatch settings, the routing table). It exits `0` for a
syntactically valid file, and non-zero with the reason on stderr for a file that
cannot be read or parsed:

```
flowhub: configuration file /path/config.json: sources.youtrack.prompt_file: stat /path/prompts/youtrack.md: no such file or directory
```

**`-print-config` does not run the filesystem checks.** Whether a clone exists, is
a git work tree, has the declared `origin`, and whether the worktree directory
sits outside it are decided by the workspace provider **at startup**, printed as a
refusal:

```
flowhub: refusing to start, dispatch is not possible:
  - youtrack:TEST: /path/to/repo is not a git work tree (no .git)
```

So the verification loop is: `-print-config` for the file, then a real start for
the host. Typical first-run problems, with the message each produces:

| Message | Cause | Fix |
| --- | --- | --- |
| `sources.youtrack.prompt_file: stat …: no such file or directory` | the template's referenced prompt file | create it, or delete the `prompt_file` line |
| `<path> is not a git work tree (no .git)` | wrong path, or a linked worktree instead of the primary clone | point at the primary clone |
| `<path> origin is "X" but the mapping declares "Y"` | typo, or an `https`/`ssh` spelling mismatch | copy `git remote get-url origin` verbatim |
| `worktrees <path> is inside the repository` | the directory is under `repo.path` | move it out |
| `no directory is configured for task checkouts` | neither `worktrees` nor `FLOWHUB_WORKTREE_BASE` | set one |
| unknown JSON member | a typo in a key name | field names are strict; fix the spelling |

Two details worth knowing: the path in the message is **canonicalised**, so on
macOS `/tmp/…` prints as `/private/tmp/…` — it is the same directory. And a
`prompt_file` problem aborts `-print-config` itself, so remove those lines first if
you copied the template verbatim.

---

## 6. Build and verify the binary

```bash
make build          # -> bin/flowhub with version/commit/time stamped in
make test           # unit tests
make smoke          # end-to-end: a real process, real HTTP, asserted logs
```

`make smoke` is the confidence check: it starts the receiver on a loopback port
with throwaway secrets, posts nine delivery shapes and asserts the HTTP codes, the
audit reasons, the payload schema and that no secret reached disk. It should end
with `SMOKE PASSED`.

If you are on a machine where the Go build cache is not writable (a sandbox), set
`GOCACHE` and `GOMODCACHE` to a directory you can write:

```bash
export GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomodcache
```

---

## 7. Phase 1: run the receiver only

Do not skip this phase. It answers the questions that no amount of reading
settles: is the token the real value, what source IP arrives, is the project key
what you think it is.

### 7.1 Generate the secrets

```bash
make secrets
```

Produces a fresh pair. Put them in a root-readable environment file:

```
# /etc/flowhub/flowhub.env   (mode 600, owned by the service user)
FLOWHUB_YOUTRACK_HOOK_KEY=<hex>
FLOWHUB_YOUTRACK_TOKEN=<hex>
FLOWHUB_YOUTRACK_ALLOWED_SOURCES=<youtrack_or_proxy_source_ip>
```

Use the **new** per-source variable names. `FLOWHUB_HOOK_KEY` and
`FLOWHUB_TOKEN` still work as aliases, but a second source would need its own.

### 7.2 Start it

```bash
FLOWHUB_ADDR=127.0.0.1:8080 \
FLOWHUB_DATA_DIR=/var/lib/flowhub \
FLOWHUB_CONFIG_FILE=~/.config/flowhub/config.json \
./bin/flowhub
```

The dispatcher is off by default, so the process only records. Note that
`-print-config` says nothing about whether the routing table's paths are usable;
the start is where that is decided. With `FLOWHUB_DISPATCH=1` a bad entry is a
refusal (see §5.3); with dispatch off the receiver starts and records even if the
mapping is incomplete — but then nothing will ever be dispatched, so fix the
refusals before Phase 2.

Bind loopback behind a reverse proxy, or the VPN address — never a wildcard. A
wildcard (`0.0.0.0:8080`) **refuses to start** unless
`FLOWHUB_ALLOW_WILDCARD_LISTEN=1`, which exists for containers only.

### 7.3 Send one delivery by hand

```bash
KEY=<FLOWHUB_YOUTRACK_HOOK_KEY>
TOKEN=<FLOWHUB_YOUTRACK_TOKEN>
TS=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)

curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' \
  -X POST "http://127.0.0.1:8080/hooks/youtrack/$KEY" \
  -H "X-YouTrack-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"event":"issueCreated","timestamp":"'"$TS"'","id":"TEST-99","summary":"probe",
       "project":{"key":"TEST","name":"Test","shortName":"TEST"},
       "reporter":{"login":"you"}}'
```

Expect `202` in well under a second. **Every rejection also returns `202` with an
empty body** — deliberately, so a scanner learns nothing. A `202` therefore proves
nothing on its own; the audit log is the truth.

### 7.4 Read the audit log

```bash
jq -c '{ts,accepted,reason,event,issue_id,project_key}' \
  /var/lib/flowhub/webhook-$(date -u +%F).jsonl | tail -5
```

For the human-readable view (masked headers, lock verdicts, the payload schema),
read `/var/lib/flowhub/payload-$(date -u +%F).log`. The `token` line tells you
whether the app sent your real token:

```
token     X-YouTrack-Token present=true len=64 sha256:a0d4eef3eaef matches_configured=true literal_secret=false
```

`literal_secret=true` means YouTrack sent the literal string `secret` instead of
your token — a known platform bug. Lock 2 is then worthless; do not rely on it.

### 7.5 Wire up YouTrack and watch a real delivery

Create a throwaway issue in the project. Then:

```bash
tail -f /var/lib/flowhub/payload-$(date -u +%F).log
```

Confirm, in this order:

1. a block appears at all — if not, you have a network or app-configuration
   problem, not a FlowHub problem;
2. `locks url_key=ok header_token=ok source_ip=ok`;
3. the `project=` line shows the key you put in the routing table;
4. `source remote=` shows the IP you expected — **this is where you learn the real
   `FLOWHUB_YOUTRACK_ALLOWED_SOURCES` value**; lock 3 was probably failing until now.

Only after all four are true should you consider Phase 2.

---

## 8. Phase 2: turn the dispatcher on

### 8.1 Preconditions

The dispatcher **refuses to start** unless all of these hold. Check them now:

* `FLOWHUB_OPENCODE_URL` (or the runtime's `url`) answers `/global/health`;
* every routable entry has a `default_branch`;
* every routable entry has a `worktrees` directory or `FLOWHUB_WORKTREE_BASE`;
* every `repo.path` is a git work tree whose `origin` matches `repo.remote`.

A hub that accepts deliveries and fails every one of them is worse than one that
refuses to start — that is why these are refusals and not warnings.

### 8.2 Start with dispatch on

```bash
FLOWHUB_DISPATCH=1 \
FLOWHUB_OPENCODE_URL=http://127.0.0.1:4096 \
FLOWHUB_DISPATCH_AGENT=devops \
FLOWHUB_PAUSE_FILE=/var/lib/flowhub/DISPATCH_OFF \
FLOWHUB_DATA_DIR=/var/lib/flowhub \
./bin/flowhub
```

**Create the pause file before the first real event** (`touch
/var/lib/flowhub/DISPATCH_OFF`). Deliveries are then still audited and nothing
runs; remove it when you are ready. A kill switch that needs a restart is not a
kill switch.

### 8.3 The workflow a maintainer sees

| Step | What happens |
| --- | --- |
| Issue created | One **read-only** turn: the agent inspects a fresh worktree and posts one comment with findings, risks and open questions. It cannot edit or commit. |
| `/opencode start` comment, or the state moves to `start_states` — and no plan yet | A **plan** turn: the plan plus its blocking questions. |
| The same trigger with a plan on file | An **execute** turn: edits, runs the narrowest test it can find, commits locally on `flowhub/<ISSUE-KEY>`. **It never pushes.** |
| The agent's own comment comes back | Ignored, by three independent layers (the `<!-- flowhub-auto -->` marker, a probe against the task's last reply, and the anchored trigger). |

Every reply ends with a blockquote saying opencode generated it and **what
triggered the turn**, with `<!-- flowhub-auto -->` as the last line. The branch is
what a human reviews and pushes.

### 8.4 Budget and runaway guards

| Variable | Default | Effect |
| --- | --- | --- |
| `FLOWHUB_MAX_TURNS` | `8` | A task that triggers more often stops and asks for a human |
| `FLOWHUB_TASK_DEADLINE` | `15m` | One turn's budget. Hitting it is **not** a failure: the session keeps running and the task is marked `executing` |
| `FLOWHUB_FIRST_RESPONSE` | `90s` | A turn that produces no assistant message in this window is **failed** — the signature of a prompt the server rejected |
| `FLOWHUB_TASK_MAX_COST` | `0` | Stop a task whose accumulated cost passes this many dollars. `0` disables it |
| `FLOWHUB_DISPATCH_QUEUE` | `32` | Deliveries waiting per runtime; a full queue **drops** rather than blocking the publisher |

Set `FLOWHUB_TASK_MAX_COST` to something small for the first weeks. A one-file
change measures around half a cent; a runaway loop costs far more.

### 8.5 Watch the first turns

```bash
tail -f /var/lib/flowhub/flowhub.log
curl -s http://127.0.0.1:8080/healthz | jq '.dispatch'
```

Useful log lines: `runtime chosen` (with the numbers it was chosen from),
`task baseline resolved`, and one line per permission decision. A rejection
carries a reason, and the model reads it and rephrases — rejections are a normal,
safe default, not a dead end.

Expected timings from the reference runs: a read-only analysis turn ~11–19 s, a
one-file implementation ~22–26 s.

---

## 9. Multi-host: enrol a runtime

Only needed for model C. A host becomes a runtime in three steps.

### 9.1 Turn on the control API (service host)

```
FLOWHUB_ADMIN_ADDR=127.0.0.1:8081
FLOWHUB_ADMIN_TOKEN=<a strong random value>
```

`FLOWHUB_ADMIN_ADDR` unset disables the listener; set without
`FLOWHUB_ADMIN_TOKEN` refuses to start. The admin API returns **real errors**,
unlike the webhook entry — they have opposite semantics on purpose.

### 9.2 Invite the host

```bash
flowhub runtime invite builder-a --projects <PROJECT_KEY> --ttl 30m
```

Prints a one-time token and the exact command to run on the other machine. The
service stores only the token's SHA-256.

### 9.3 Install and register (builder host)

```bash
flowhub runtime init --check \
  --repo git@<your-git-host>:owner/repo.git=<ABSOLUTE_CLONE_PATH>

flowhub runtime init --server http://<SERVICE_HOST>:8081 --name builder-a \
  --token <ONE_TIME_TOKEN> --advertise http://<THIS_HOST_IP>:4096
```

`--advertise` is what the service probes, so a NAT or firewall mistake fails at
enrolment rather than at the first task. The probe checks more than liveness: the
agent profile the host claimed must exist on **that** server, and the model it
reported must be one the server offers.

The host keeps its returned secret at `~/.config/flowhub/runtime.json` (mode
`0600`). Verify:

```bash
flowhub runtime list
flowhub runtime show builder-a
```

`last_seen` only ever records a host that **passed** its check. A report that
fails is deliberately not pushed, so an expired credential surfaces while you are
looking rather than at the first task.

### 9.4 Keep the report current

```bash
flowhub runtime doctor --push      # on the builder host, periodically
```

### 9.5 Binding and removal

**A task is bound to the runtime that prepared its worktree, for life.** Removing
a runtime refuses while non-terminal tasks are bound to it; `--force` overrides
and those tasks are then refused one by one, with the reason, rather than silently
moving to another host.

```bash
flowhub runtime remove builder-a            # refuses if tasks are bound
flowhub runtime rotate builder-a            # re-issue the runtime secret
```

---

## 10. Deploy as a service

There is no Dockerfile or systemd unit in the repository; supply your own. The
binary is self-contained (`CGO_ENABLED=0`) and can be copied anywhere.

### 10.1 systemd (Linux)

```ini
# /etc/systemd/system/flowhub.service
[Unit]
Description=FlowHub YouTrack webhook receiver and opencode dispatcher
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=flowhub
Group=flowhub
EnvironmentFile=/etc/flowhub/flowhub.env
Environment=FLOWHUB_ADDR=127.0.0.1:8080
Environment=FLOWHUB_DATA_DIR=/var/lib/flowhub
Environment=FLOWHUB_CONFIG_FILE=/etc/flowhub/config.json
Environment=FLOWHUB_DISPATCH=1
Environment=FLOWHUB_OPENCODE_URL=http://127.0.0.1:4096
ExecStart=/usr/local/bin/flowhub
Restart=on-failure
RestartSec=3
TimeoutStopSec=20

# The agent runs arbitrary build/test commands; the service account must be
# unprivileged and hold no SSH key of its own beyond the one it needs to push.
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo install -m 600 -o flowhub -g flowhub flowhub.env /etc/flowhub/flowhub.env
sudo install -m 600 -o flowhub -g flowhub config.json /etc/flowhub/config.json
sudo systemctl daemon-reload && sudo systemctl enable --now flowhub
journalctl -u flowhub -f
```

`PrivateTmp=true` is fine: worktree paths are real directories, not `/tmp`.

### 10.2 launchd (macOS)

```xml
<!-- ~/Library/LaunchAgents/cn.yusiwen.flowhub.plist -->
<plist version="1.0"><dict>
  <key>Label</key><string>cn.yusiwen.flowhub</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/bin/flowhub</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>FLOWHUB_ADDR</key><string>127.0.0.1:8080</string>
    <key>FLOWHUB_DATA_DIR</key><string>/Users/<YOU>/Library/Application Support/flowhub</string>
    <key>FLOWHUB_CONFIG_FILE</key><string>/Users/<YOU>/.config/flowhub/config.json</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
```

A laptop sleeping is the classic failure mode here: the receiver is down, the
app has already given up, and the event is lost. If that matters, run the receiver
on an always-on host and keep only the opencode turns on the laptop.

### 10.3 nginx in front (recommended)

```nginx
# http{} level
limit_req_zone $binary_remote_addr zone=yt_hook:10m rate=5r/s;

server {
    listen 443 ssl http2;
    server_name <YOUR_ENTRY_HOST>;

    allow <YOUTRACK_HOST_IP>;      # the only client that may reach this
    deny all;

    client_max_body_size 256k;
    limit_req zone=yt_hook burst=10 nodelay;

    location = /hooks/youtrack/<YOUR_HOOK_KEY> {
        if ($request_method != POST) { return 405; }
        access_log off;                          # the key lives in this URL
        proxy_pass http://127.0.0.1:8080;
        proxy_read_timeout 5s;                   # the app gives up at 5s
        proxy_connect_timeout 2s;
        proxy_set_header X-Real-IP $remote_addr;
    }
    location / { return 404; }
}
```

* Do **not** validate the token in nginx: it may be the literal string `secret`,
  and the check belongs in FlowHub where it is constant-time and audited.
* Do **not** log the request body: it contains issue text.
* Do not put a CDN in front — it hides the real source IP and defeats the allowlist.
* `GET /healthz` is local only; do not proxy it publicly.

---

## 11. Day-2 operations

### 11.1 Health

```bash
curl -s http://127.0.0.1:8080/healthz | jq
```

One call answers: queue depth, active locks, current audit file, dispatch counters
(`queued`, `handled`, `ignored`, `dropped`, `paused`, `agent`) and a per-runtime
view (`queued`, `running`, `tasks`, `busy`, `in_flight`, `max_concurrent`,
`last_error`).

### 11.2 Pause and resume

```bash
touch /var/lib/flowhub/DISPATCH_OFF    # audit only, nothing runs
rm    /var/lib/flowhub/DISPATCH_OFF    # resume
```

No restart, no API, no credential. Checked before every delivery.

### 11.3 Inspect and replay

```bash
# Rejections by reason, last 24h
jq -r 'select(.accepted|not) | .reason' /var/lib/flowhub/*.jsonl | sort | uniq -c | sort -rn

# The slowest deliveries (the app times out at 5s)
jq -r '.handled_ms' /var/lib/flowhub/*.jsonl | sort -n | tail -3

# Which project keys actually arrive
jq -r 'select(.accepted) | .project_key' /var/lib/flowhub/*.jsonl | sort -u

# Re-post a recorded delivery against your own receiver
jq -c 'select(.accepted) | .raw_body' /var/lib/flowhub/webhook-*.jsonl | head -1
```

The last one is how you test a routing change without waiting for a real event:
every record keeps the full raw body.

### 11.4 The task registry

`/var/lib/flowhub/registry.jsonl` is append-only and indexed by
`(source, issue key)`. Per task it records the repository, worktree, base commit,
session id, agent, state (`analyzing → awaiting_input → executing → done`,
`failed` on a broken turn), plan state, turn count, cost and last reply.

```bash
jq -c '{issue,repo,runtime,state,turns,cost,worktree}' \
  /var/lib/flowhub/registry.jsonl | tail -10
```

The worktrees themselves are what a human reviews:

```bash
git -C /path/to/worktrees/<ISSUE-KEY> log --oneline -5
git -C /path/to/worktrees/<ISSUE-KEY> diff <BASE_COMMIT>..HEAD
```

### 11.5 Log rotation

The application log rotates by size (`FLOWHUB_LOG_MAX_BYTES`, default 32 MiB,
`FLOWHUB_LOG_MAX_FILES`, default 5). The **audit** files rotate by UTC day and are
never deleted by FlowHub. Add your own retention:

```bash
find /var/lib/flowhub -name 'webhook-*.jsonl' -mtime +90 -delete
find /var/lib/flowhub -name 'payload-*.log'   -mtime +30 -delete
```

### 11.6 Secret rotation

1. Generate a new pair (`make secrets`).
2. Update only `FLOWHUB_YOUTRACK_HOOK_KEY` (and the URL in YouTrack) **or** only
   the token, not both at once — during the gap, deliveries are rejected but
   answered `202`, so check the audit for `bad_url_key` / `bad_header_token`.
3. Runtime secrets: `flowhub runtime rotate <name>`.
4. Redaction is positional and applies even to a key that does not match, so a
   rotated key never lands in the audit file.

### 11.7 Upgrading

```bash
git pull && make test && make build
sudo install -m 755 bin/flowhub /usr/local/bin/flowhub
sudo systemctl restart flowhub
```

The routing file is validated at startup; a bad edit refuses the start rather
than routing wrongly. Task worktrees and the registry survive a restart.

---

## 12. Troubleshooting playbook

Start here — it separates "the event never arrived" from "we rejected it".

```bash
tail -5 /var/lib/flowhub/webhook-$(date -u +%F).jsonl | jq -c '{accepted,reason,event}'
```

### 12.1 Nothing arrives at all

| Check | Command | Interpretation |
| --- | --- | --- |
| Is the process up? | `systemctl status flowhub` | |
| Is it listening where nginx points? | `lsof -iTCP:8080 -sTCP:LISTEN` | a mismatch is the usual cause |
| Does nginx deliver? | `tail -f /var/log/nginx/access.log` | no line at all → YouTrack side |
| Is the app configured? | project → Settings → Apps → Webhook Triggers | and check its technical log: `Administration → Apps → Webhook Triggers → Technical Details` for `[webhooks] Blocked webhook to …` |
| Is the URL rejected? | app log | private IP literal, or a comma in the URL |

### 12.2 Arrives but rejected

| `reason` | Cause | Fix |
| --- | --- | --- |
| `bad_url_key` | wrong or absent key in the URL | the URL must end in `/hooks/youtrack/<FLOWHUB_YOUTRACK_HOOK_KEY>`; if you changed the key, update YouTrack |
| `bad_header_token` | header missing or wrong | `headerName` in the app and `FLOWHUB_TOKEN_HEADER` must match; token values must match. If `literal_secret=true`, YouTrack is sending the literal string `secret` |
| `source_not_allowed` | your source IP is not in `FLOWHUB_YOUTRACK_ALLOWED_SOURCES` | read the `source remote=` line in the payload log and put **that** value in the allowlist |
| `outside_replay_window` | clock skew beyond `FLOWHUB_REPLAY_WINDOW` between YouTrack and this host | fix NTP, or set the window to `0` for the very first delivery while diagnosing |
| `duplicate` | the same delivery seen inside `FLOWHUB_DEDUPE_TTL` | normal (the app can retry across a proxy); raise the TTL only if genuine events collide |
| `unknown_event` | an event type FlowHub does not implement | check the 11 accepted values; disable the extra field in the app |
| `unexpected_content_type` | a proxy rewrote the header | must be `application/json` (a missing one is tolerated) |
| `body_too_large` | body above `FLOWHUB_MAX_BODY_BYTES` | raise it, and nginx's `client_max_body_size` to match |

### 12.3 Accepted but nothing runs

| Symptom | Cause | Fix |
| --- | --- | --- |
| No turn, log says `no repository is mapped` | the project key is not in the routing table | add the `(source, project)` entry; check `project_key` in the audit record against the file, case-insensitively |
| No turn, log says `no rule matched this event` | the event is not one that starts work | expected for field edits; use `/opencode start` or the configured state |
| No turn, `dispatch.paused > 0` | the pause file exists | `rm <FLOWHUB_PAUSE_FILE>` |
| No turn, `dropped > 0` | the dispatch queue is full | raise `FLOWHUB_DISPATCH_QUEUE`; investigate why turns are slow |
| `no runtime answered` | the opencode server is down | `curl <runtime_url>/global/health` |
| Task refused: *bound to runtime X, which is no longer configured* | the runtime was removed or renamed | restore it under the same name; the task cannot move |
| Task refused: *mapping now points at …* | `repo.path` changed for an existing task | un-bind deliberately (new issue, or prune the registry row) rather than editing the path |

### 12.4 A turn starts and fails

| Symptom | Cause | Fix |
| --- | --- | --- |
| Failed at `FLOWHUB_FIRST_RESPONSE` with no message | the agent server rejected the prompt — usually a model the provider no longer offers | re-run `flowhub runtime init --check`; the profile must name a model the server lists |
| Every turn runs the wrong behaviour | the agent registry is cached; a *new* agent file is only picked up by a new server process | restart `opencode serve` |
| The agent asks for a permission forever | a request the server keeps listing | see `README.md`; a 404 is treated as already resolved, and a handled request is never listed again |
| Push fails on the first real task | `runtime init --check` ran as a different user than the service | re-run the check as the service user; it reports the identity it ran as |

### 12.5 Escalation data to collect

Before asking for help, capture: the audit record for the delivery
(`jq 'select(.issue_id=="X")' …`), the full payload-log block, the matching lines
from `flowhub.log`, the registry row for the task, and the output of
`flowhub -print-config` (secrets are masked). `curl /healthz | jq` completes the
picture.

---

## 13. Appendix: file, port and secret inventory

### 13.1 Paths

| Path | Written by | Contents |
| --- | --- | --- |
| `~/.config/flowhub/config.json` | you | routing table, sources, runtimes |
| `/var/lib/flowhub/webhook-YYYY-MM-DD.jsonl` | FlowHub | one JSON record per delivery, raw body included |
| `/var/lib/flowhub/payload-YYYY-MM-DD.log` | FlowHub | human-readable per-delivery blocks |
| `/var/lib/flowhub/flowhub.log` | FlowHub | application log, size-rotated |
| `/var/lib/flowhub/registry.jsonl` | FlowHub | task registry (append-only) |
| `/var/lib/flowhub/runtimes.json` | FlowHub | runtime inventory (control plane only) |
| `/var/lib/flowhub/DISPATCH_OFF` | you | kill switch |
| `~/.config/flowhub/manifest.json` | `runtime init` | installed files with SHA-256 |
| `~/.config/opencode/agents/devops.md` | `runtime init` | the agent definition |
| `~/.config/opencode/opencode.json` | you (+ `runtime init` prints a snippet) | MCP servers; holds `{env:...}` references, never values |
| `~/.config/flowhub/runtime.json` | `runtime init --token` | this host's runtime identity (mode 0600) |

### 13.2 Ports

| Port | Default | Purpose | Exposure |
| --- | --- | --- | --- |
| 8080 | `FLOWHUB_ADDR` | webhook entry + `/healthz` | your reverse proxy or VPN only |
| 8081 | `FLOWHUB_ADMIN_ADDR` | control API (`/control/v1`) | loopback or VPN only; needs `FLOWHUB_ADMIN_TOKEN` |
| 4096 | `opencode serve` | agent server | loopback, or the network FlowHub reaches it on |
| 443 | nginx | TLS entry | public, IP-allowlisted to YouTrack |

### 13.3 Secrets and where they live

| Secret | Variable | Where the other side holds it |
| --- | --- | --- |
| URL key (lock 1) | `FLOWHUB_YOUTRACK_HOOK_KEY` | inside the webhook URL in the YouTrack app |
| Header token (lock 2) | `FLOWHUB_YOUTRACK_TOKEN` | the app's `webhookToken` |
| Source allowlist (lock 3) | `FLOWHUB_YOUTRACK_ALLOWED_SOURCES` | not a secret |
| Control API | `FLOWHUB_ADMIN_TOKEN` | the CLI's `--token` |
| Runtime enrolment | one-time invite token | printed once by `runtime invite` |
| Runtime secret | stored in `runtime.json` | the inventory keeps only its SHA-256 |
| YouTrack token for the agent | `YOUTRACK_TOKEN` | the MCP snippet references `{env:YOUTRACK_TOKEN}` |
| opencode Basic Auth | `FLOWHUB_OPENCODE_USER` / `_PASSWORD` | the agent server's `OPENCODE_SERVER_PASSWORD` |

No secret is ever written into the routing table, the MCP snippet, or the audit
log. The audit records a length, a SHA-256 prefix and a `matches_configured` flag
instead of the value.

### 13.4 Sequential checklist

- [ ] opencode installed; `opencode serve` healthy; provider works
- [ ] `flowhub runtime init --check` → exit `0`
- [ ] `flowhub runtime init`; MCP snippet merged; `YOUTRACK_TOKEN` exported
- [ ] `curl /agent` lists `devops`
- [ ] YouTrack: Webhook Triggers installed, attached to the project, URL is a domain, no comma
- [ ] `~/.config/flowhub/config.json` written; `prompt_file` lines removed or files created
- [ ] `flowhub -print-config` shows no `problem:`
- [ ] `make build && make test && make smoke` → `SMOKE PASSED`
- [ ] **Phase 1**: receiver running, one `curl` delivery accepted, audit shows `url_key=ok header_token=ok source_ip=ok`
- [ ] a real YouTrack event recorded; `project_key` matches the routing table
- [ ] **Phase 2**: `DISPATCH_OFF` created, `FLOWHUB_DISPATCH=1`, pause removed deliberately
- [ ] first analysis turn posts exactly one comment with the marker
- [ ] `/opencode start` produces a plan, then an implementation on `flowhub/<ISSUE-KEY>`
- [ ] the agent's own comment is ignored (turn count unchanged)
- [ ] service unit installed; logs and rotation verified; cost cap and `MAX_TURNS` set
