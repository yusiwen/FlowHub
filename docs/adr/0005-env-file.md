# ADR 0005 — An environment file whose values are defaults

**Status:** Accepted, **implemented** (2026-10-08), in the same change:
`internal/config/envfile.go` (the resolver, the parser, the permission gate and
`ApplyEnvFile`), the call in `main` **before** the subcommand dispatch, the
`env_file` line in `-print-config` and the `environment file applied` log line, plus
the README, the operations manual, `AGENTS.md` and `CODEBASE.md`.

## Context

FlowHub reads its configuration from the process environment only. `config.Load` is
27 `os.Getenv`/`env(...)` calls and the package performed no file I/O at all. The
three documented layers — environment variable → `sources.<name>` /
`runtimes.<name>` block → `projects[]` entry — all start from the real environment.

That default has a cost on a development machine: every new shell needs
`FLOWHUB_HOOK_KEY`, `FLOWHUB_TOKEN`, `FLOWHUB_ALLOWED_SOURCES` … exported again, and a
forgotten export produces a receiver that starts with **all three locks disabled**
rather than an error. ADR 0004 solved this for one agent password; it did not solve it
for the lock material, the listen address or the dispatch switch.

The service case was already covered — systemd's `EnvironmentFile=` and containers'
`-e` — so the gap was specifically "run it from my shell". The request came with an
expectation about the path, `~/.config/flowhub/.env`, and about precedence, which is
the decision this record has to make explicit.

## Decision

**A dotenv-shaped file at `~/.config/flowhub/.env`, whose values are defaults.**

| Source | Wins over |
| --- | --- |
| the real process environment | the environment file |
| the environment file | the compiled defaults |

The file is applied in `main` **before** the subcommand dispatch, so
`flowhub runtime init --check` sees the same values the receiver would. It is read
once, at start.

The path resolves as: an explicit `EnvFileOptions.Path`, then `FLOWHUB_ENV_FILE`,
then `<config dir>/.env` — the same directory as the routing table, because both
describe this host. `FLOWHUB_ENV_FILE=-` (or `none`, `off`) disables the file, which
is how a deployment that configures through the environment says so on purpose. An
absent **default** path is fine; an absent path that was **named** refuses the start.

Parsing is a deliberate subset: `KEY=VALUE`, `#` comments, blank lines, an optional
`export ` prefix, and one layer of matching surrounding quotes stripped. Everything
else — `${VAR}` interpolation, includes, line continuations, shell evaluation — is
refused where it appears (a malformed line, a name that is not a name) or simply not
implemented. A duplicate key inside the file refuses rather than being resolved by
position, because which line the operator meant is not knowable.

The file carries the ADR 0004 permission floor: `mode & 0o077 != 0` refuses the start
with the `chmod 600` fix. Nothing from the file is ever logged: the log line and the
report name the path and the variable **names**, including the ones the environment
overrode.

## Alternatives rejected

* **The file overrides the environment.** The tempting direction — "the file is the
  thing I just edited, so it should win" — and the dangerous one:
  `FLOWHUB_ADDR=… ./bin/flowhub` would silently use a different address because of a
  file the operator forgot, which is the silent override the credentials surface
  already refuses. It would also make `systemd Environment=` and a stray file
  disagree invisibly.
* **Loading the file inside `config.Load`.** Fewer call sites, and it would leave
  `runtime init` — which runs before `config.Load` by rule 11 — unable to see a
  `YOUTRACK_TOKEN` the operator keeps in the file, forcing exactly the double export
  the feature exists to remove.
* **Reading `./.env` from the working directory.** That is a Node/opencode
  convention, not this project's: it would make behaviour depend on where the process
  was started, and the routing table already taught the opposite lesson (paths resolve
  against the configuration file, never the cwd).
* **Supporting `${VAR}` interpolation.** Attractive, and it turns the file into a
  program whose evaluation order has to be specified and tested. One substitution rule
  would be two, then defaults (`${VAR:-x}`), then quoting inside substitutions.
* **A `0600`-or-warn instead of `0600`-or-refuse.** A warning is invisible in a
  service unit, and this file holds the URL key and the header token; the credentials
  file already refuses, so a weaker rule here would be inconsistent as well as unsafe.
* **Watching the file for changes.** Like the credentials file, it is read once.
  Reloading means defining what happens to a running receiver whose address changed.

## Consequences

* **A forgotten file is impossible to hide.** The startup log says
  `environment file applied path=… applied=A,B skipped_already_set=C`, and
  `-print-config` prints `env_file:` with the same lists, so "I put it in the file and
  nothing happened" is answered by the report rather than by a debugging session. The
  `skipped_already_set` list is the part that explains a file entry that appears to do
  nothing.
* **A `0644` file refuses the start.** This is the one behaviour that will surprise
  someone who writes a file with a text editor and runs FlowHub immediately; the
  message names the path and the `chmod 600` fix. It is the behaviour ADR 0004 already
  established for the credentials file, so the surprise happens at most once.
* **The file is shell-like but not a shell.** `export ` and quotes work because a
  copied line looks like that; `A=b c` is the value `b c` (the shell would run `c`),
  and `#` only starts a comment at the beginning of a line, so a token may contain
  one. Every one of those choices is pinned by a test, which is what makes "minimal"
  mean "specified" rather than "whatever the regex did".
* **A malformed line refuses the start with its line number.** A file that is silently
  half-applied is the failure mode this project refuses everywhere; the cost is that a
  typo blocks the start rather than being skipped.
* **`FLOWHUB_ENV_FILE` is read from the real environment only.** A file cannot point at
  another file: that would make the path itself a matter of precedence and invite a
  chain. The variable is the operator's statement, so it comes from the operator's
  environment.
