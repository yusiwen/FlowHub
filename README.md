# FlowHub — YouTrack webhook receiver (phase 1)

The first implemented slice of FlowHub: a Go service that receives YouTrack
webhook deliveries, authenticates and validates them, deduplicates, writes an
audit record, and answers `202 Accepted` in well under a millisecond.

It is intentionally a **pure receiver**. It never calls YouTrack, opencode or any
other network service on the request path. Wiring the events to `opencode serve`
is phase 2 (`opencode-devops-orchestration-design.md` §12.1).

The three documents in the repository root are the specification:

| Document | Role |
| --- | --- |
| `youtrack-webhook-and-flowhub-security.md` | YouTrack side, payload facts, layered security design (§8, §9, §12) |
| `opencode-devops-orchestration-design.md` | Overall FlowHub architecture, session ownership, event-driven model |
| `opencode-headless-automation-and-permissions.md` | opencode HTTP API and the permission loop used in phase 2 |

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
| `FLOWHUB_PROJECTS_FILE` | `./config/projects.json` | Routing table: YouTrack project → local repository. A missing file only warns; a file that exists but fails validation stops startup |

Disabled locks produce a loud startup warning, and `-print-config` lists the
active locks. All three locks are expected to be set once the public entry exists
(`youtrack-webhook-and-flowhub-security.md` §12.2).

## Routing: YouTrack project → repository

opencode operates on a directory, so every delivery has to be resolved to the
repository it belongs to. That resolution is **declared in a file, never
guessed**:

```bash
cp config/projects.example.json config/projects.json   # then edit it
./bin/flowhub -print-config                            # shows the table and any problem
```

```json
{
  "projects": [
    {
      "youtrack_key": "BEAP_BE",
      "repo": {
        "path": "/Users/yusiwen/git/work/pipechina/beap-be",
        "remote": "git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git",
        "default_branch": "master"
      },
      "worktrees": "/Users/yusiwen/git/work/pipechina/beap-be-worktrees",
      "agent": "devops"
    }
  ]
}
```

**Why it cannot come from YouTrack.** The webhook payload carries only
`{key, name, shortName}` for the project, and YouTrack exposes no REST field or
endpoint for the VCS-integration repository URL — so there is nothing to read
even though YouTrack knows the answer. Measured evidence:
`youtrack-webhook-and-flowhub-security.md` §5.6.

### Matching

| Rule | Behaviour |
| --- | --- |
| Primary key | `youtrack_key` is matched against the payload's `project.key`, **case-insensitively** |
| Aliases | `also_keys` lets several YouTrack projects share one repository |
| Fallback | Only when a payload carries **no** project object: the issue-ID prefix (`BEAP_BE-12` → `BEAP_BE`) |
| Unmapped project key | **Hard miss.** A payload that names a project you did not map is never routed via the prefix — that would be exactly the guess this layer exists to prevent |
| `enabled: false` | Never matched, and not validated (it may point at a checkout this host does not have) |

### Startup validation (fail-closed)

A mapping file that exists must be correct, so these all **stop the process**
before any file is created:

* `repo.path` missing, not a directory, or not a git work tree;
* `repo.path` not matching `repo.remote` — compared against `.git/config`, so a
  typo cannot route work into the wrong clone (leave `remote` empty to skip only
  this check);
* a relative path anywhere (it would depend on the caller's working directory);
* `worktrees` inside `repo.path` or equal to it;
* a duplicate key, or an `agent` name that is not a plain identifier;
* any unknown JSON member (a typo in a field name is an error, not a no-op).

Keys whose name starts with `_` are documentation and are ignored, which is how
the example carries its explanations in a format without comments.

A **missing** file is only a warning: phase 1 records deliveries without routing
them. `config/projects.json` is gitignored (its paths are host-specific) while
`config/projects.example.json` is committed.

> **Not wired up yet.** Today this layer loads, validates and reports the table;
> nothing dispatches to opencode. Turning a match into a session (worktree,
> `POST /session`, the `task_session` registry) is the next step.

## Endpoints

| Endpoint | Behaviour |
| --- | --- |
| `POST /hooks/youtrack/<key>` | The webhook entry point |
| `POST /hooks/youtrack?k=<key>` | Same, for deployments that prefer a query string (the app only requires a URL that is not comma separated) |
| `GET /healthz` | Local only; counters, active locks, queue depth, current audit file. nginx does not proxy this path |

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

## Phase 1 verification: closing the open questions

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
curl 192.168.8.135:18099/healthz -> 200     # the LAN address, no tunnel involved
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
| `internal/webhook` | Lenient payload model, payload schema report, the delivery pipeline, redaction |
| `internal/dedupe` | TTL idempotency cache |
| `internal/store` | Audit record, non-blocking queue, daily JSONL writer, human readable payload log |
| `internal/projectmap` | YouTrack project → repository routing table: strict loader, canonical paths, fail-closed validation |
| `internal/metrics` | Counters used by `/healthz` |
| `config/projects.example.json` | Committed template for the routing table |
| `scripts/smoke.sh` | The end-to-end check behind `make smoke` |

`AGENTS.md` records the project invariants that contributors and coding agents
must preserve; `CODEBASE.md` is the file-level map.

## License

MIT — see [`LICENSE`](./LICENSE). Copyright (c) 2026 Siwen Yu.

## Not implemented yet

* Phase 2: `task_key → session_id` registry, per-session serial queue, SSE
  subscription, the `devops` agent and the permission arbiter.
* Phase 2: YouTrack rules (author allowlist, `/opencode` trigger, project
  allowlist) and reply-back to the issue.
* Phase 3: alerting, key rotation, budget circuit breaker, optional HMAC signing
  through a custom workflow rule, and the always-on aliyun shim.
