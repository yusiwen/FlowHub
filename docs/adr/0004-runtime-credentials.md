# ADR 0004 — Runtime credentials: a shared credentials file, per-runtime files, and an inline password

**Status:** Accepted, **implemented** (2026-10-08). Recorded with the code, in the same
change: `internal/projectmap/credentials.go` (the loader, the four sources, the
permission gate and `ResolveCredential`), the `credentials_file` field and the two new
`auth` fields in `internal/projectmap/format.go`, the precedence and refusal rules in
`cmd/flowhub/main.go` (`declaredAuthProblems`, `configFileSecretProblems`,
`newRuntimeFactory`, `opencodeProber`), and `internal/dispatch` unchanged.

## Context

An opencode server can require HTTP Basic Auth, and it does whenever
`OPENCODE_SERVER_PASSWORD` is set on it: `/global/health` then answers `401` with
`www-authenticate: Basic`. FlowHub has to send that pair on every call — the activation
probe, the agent-registry and model-catalogue reads, the session creation, the permission
loop — or the runtime is unusable.

Before this decision there was exactly one way to configure the password:
`FLOWHUB_OPENCODE_USER` / `FLOWHUB_OPENCODE_PASSWORD` as the process-wide pair, and
`runtimes.<name>.auth.password_env` to name a per-host variable. `declaredAuthProblems`
refused the start when the named variable was empty, so a missing pair failed at startup
rather than at the first turn — the parts that existed were sound.

Two things were missing.

1. **There was no file-based way to hold the credentials.** An operator with several
   builders had to export one variable per host, or duplicate the secrets in a service
   unit's environment. Environment variables are the right *default* (a file the process
   reads can be copied by accident, and `systemctl show-environment` is auditable), but a
   deployment that already manages its secrets in files had nowhere to put them.
2. **Nothing checked the permissions of a file that does hold a secret.** The
   configuration file was documented as safe to copy, diff and commit because it held no
   secret; the moment a password can be written into it, that statement stops being true
   on its own, and a `0644` file would expose it while the operator believes otherwise.

The constraint that must survive: **the routing table stays free of secrets by default.**
Every deployment before this change relies on the file being shareable, and a feature
that quietly reverses that would be a security regression dressed as convenience.

## Decision

Four password sources, one precedence order, and an enforced permission floor.

| Source | Spelling | Secret lives in |
| --- | --- | --- |
| shared file | top-level `credentials_file`, entries keyed by runtime name | that file (`0600`) |
| per-runtime file | `auth.password_file` | that file (`0600`) |
| environment | `auth.password_env`, or `FLOWHUB_OPENCODE_PASSWORD` | the process environment |
| inline | `auth.password` | the configuration file (which must then be `0600`) |

**Precedence, highest first:** the runtime's own `auth` block, the shared
`credentials_file` entry for that runtime, the process-wide environment pair. A
runtime-specific declaration losing to a process-wide default is the silent override this
project refuses elsewhere, so the order is deliberate and pinned by a test.

**Mutual exclusion:** a block names at most one password source. Two is the mistake that
survives every later reading — the operator rotates the source that is not in force and
nothing happens — so it is a load-time refusal, not a warning.

**The permission floor:** a `credentials_file`, a `password_file`, and the configuration
file *when it carries an inline `password`* are refused at startup if
`mode & 0o077 != 0`, with the `chmod 600` fix in the message. See the consequence below
for what this check can and cannot prove.

**One resolution function, two callers.** `projectmap.ResolveCredential(name)` answers for
the dispatcher's runtime factory and for the activation prober. Before this change those
two read the environment independently, which is how a probe passes against one pair while
the first turn fails against another.

## Alternatives rejected

* **A per-runtime `password_file` only** (one secret per file, no shared file). Simpler,
  and it fits Docker-style secrets, but it gives an operator with five hosts five files and
  no single place to see the estate. The shared file is the shape the operator asked for;
  the per-runtime file stays available for machines that receive one secret and nothing
  else.
* **Putting the credentials map inside `config.json` under `runtimes.<name>.secrets`.**
  No new file, no new path resolution — and it makes the routing table a secret store,
  which is exactly the property the earlier design protected. Rejected outright.
* **A blanket `0600` requirement on `config.json`.** Consistent and simple, and it breaks
  every existing `0644` deployment that holds no secret at all, for no security gain. The
  requirement is therefore conditional on the file actually carrying a `password`.
* **Hot-reloading the credentials file.** Convenient, and it needs a filesystem watch plus
  a definition of what happens to turns already in flight. Environment variables never
  reloaded; keeping the same rule (load once, restart to rotate) is one fewer moving part
  and no surprise for an operator who already rotates by restarting.

## Consequences

* **Every path that talks to an agent server resolves credentials by runtime name, and
  the name is derived from the declaration, not assumed.** The runtime factory and the
  activation prober always did; the startup reachability probe did not, and on
  2026-10-08 that produced a live 401: a table declaring `local` with
  `auth.password_env` and no process-wide pair was refused with
  `opencode at http://127.0.0.1:4096 is not answering: … HTTP 401`, while the turns it
  would have authorised were configured correctly. Two things had to be true for that:
  the probe built its client from `FLOWHUB_OPENCODE_USER`/`PASSWORD` (the environment
  pair only), and it probed under the fallback name `default` rather than under the
  declaration that owns the address. It now resolves through `ResolveCredential` named
  `defaultRuntimeNameFor(cfg, projects)` — the first declared block whose URL is
  `FLOWHUB_OPENCODE_URL`, else `default` — so the probe, the activation check and the
  turn all present the same pair. A test asserts both that the right pair is accepted
  and that a wrong one is still refused, so the probe cannot "pass" by dropping the
  header.
* **Rotating a password needs a restart.** The file is read once, when the table is
  loaded, exactly as the environment pair always was. Stated in the README and the
  operations manual; the alternative was rejected above.
* **The permission check is a floor, not a proof.** It reads POSIX permission bits. On
  macOS an ACL that *grants* access does not appear in `FileMode`, so a file can pass this
  check and still be readable by another principal; conversely an ACL that *denies* access
  on a `0644` file is refused. The message says "group and other must have no access",
  which is what is actually enforced. On Windows the bits are not meaningful and the check
  is skipped rather than reporting a refusal no `chmod` can fix.
* **An unreadable file is reported as unreadable, never as "no secret".** The two need
  different fixes, and the project's rule is that a check which could not run must not be
  reported as a negative finding.
* **A credentials entry naming an undeclared runtime is an error.** A typo there silently
  disables a credential; refusing it costs nothing legitimate, because the file exists to
  serve declared runtimes.
* **`config.json` is no longer unconditionally shareable.** It still is whenever the
  password comes from `password_env`, `password_file` or `credentials_file` — which is the
  recommended shape and what the template ships — but an inline `password` makes the file
  a secret, and the README now says so.
* **`-print-config` names the source, never the value** (`password_env:VAR`,
  `password_file:/etc/flowhub/a.pass`, `credentials_file:…/credentials.json#local`,
  `password`), and a test asserts that all four secrets stay out of the rendered report.
