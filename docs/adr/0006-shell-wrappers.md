# ADR 0006 — Trusting a command prefix a host's plugin rewrites

**Status:** Accepted, **implemented** (2026-10-09), in the same change:
`internal/agent/opencode/wrapper.go` (the stripping, the grammar and the diagnostic),
the re-judge in `phase.go`, `RuntimeOptions.ShellWrappers`, the
`FLOWHUB_SHELL_WRAPPERS` default and the `runtimes.<name>.shell_wrappers` block, the
startup refusal and log line in `main`, plus the README, the operations manual,
`AGENTS.md`, `CODEBASE.md` and `config/config.example.json`.

## Context

`internal/agent/opencode` answers opencode's `ask` requests with an allowlist over
shell **segments**, anchored at the command name:

```
^(?:git\s+(status|log|diff|show|branch|rev-parse|…)(\s.*)?)$
```

That is a statement about the command *the model wrote*, and the model's own tool input
is not what the permission request carries. opencode's `tool.execute.before` plugin hook
runs between the two: a plugin can rewrite `args.command` and the request that reaches
FlowHub holds the rewritten text.

Measured on 2026-10-09 with issue `BEAP_BE-46`, session `ses_ee1c39202ffehbqg4ItVHA3B8X`.
The host has `~/.config/opencode/plugins/rtk.ts`, a thin plugin that calls
`rtk rewrite <command>` and writes the result back into `args.command`. `rtk` 0.51.0
rewrites every read-only git subcommand the allowlist covers, so `git status` arrived as
`rtk git status`, `git log --oneline -5` as `rtk git log --oneline -5`, and `git branch
--show-current` as `rtk git branch --show-current`, while `git rev-parse HEAD` (no rtk
rule) passed through. The result:

* every read-only git inspection a read-only turn could want was refused, so the
  allowlist's git entries were unreachable on that host;
* the refusal said `command segment "rtk git status" is not on the read-only
  allowlist`, quoting a command the model had never written, and the model responded by
  rephrasing into another git command that was rewritten the same way. Three bash calls,
  three rejections, no assistant text, and the turn ended without posting a comment —
  the issue had no reply at all.

Two separable defects are visible there, and they have different fixes: the *decision*
(a legitimate, allowlisted command was refused) and the *diagnosis* (the reason the model
received could not be acted on, and named a command it did not write).

## Decision

**Two mechanisms, both opt-in at the point where trust must be given.**

1. **The refusal names the wrapper, with no configuration at all.** When a segment fails
   the allowlist but dropping its first word leaves a segment the *same* policy would
   have allowed, the refusal says so, names the prefix, names the command it hides, and
   tells the model that retrying will be rewritten identically and that the fact should be
   reported instead. This is the half that turns a dead turn into a report, and it can
   never widen anything: the command is still refused.
2. **A prefix the operator names is stripped and the remainder judged by the same
   lists.** `FLOWHUB_SHELL_WRAPPERS` (comma separated) is the process-wide default and a
   `shell_wrappers` list inside `runtimes.<name>` overrides it per host, following the
   same precedence shape as `deadline`. The deny list and the allowlist are applied to
   what remains, so trusting a wrapper is a *relabelling* and never a widening:
   `rtk rm -rf /` strips to `rm -rf /` and is refused, `rtk git push` is refused, `git add`
   is still execution-phase-only, and a wrapper in front of `curl`, `python`, `chmod` or an
   absolute path changes nothing. The default is empty: no prefix is stripped, and
   `&`-style removal of the first token is never done implicitly.

Three further rules make the feature refuse rather than degrade:

* **A wrapper name must be a bare command name** (letters, digits, dot, dash, underscore,
  plus; no leading dash, no path separator, no metacharacter), verified by the package
  that will trust it, at both levels. An entry that cannot match a command's first word
  would silently do nothing, and a silently inert policy entry is what this project
  refuses everywhere.
* **Trusting a wrapper is never silent.** Startup logs one line per host that has one,
  `-print-config` renders the block's list with its level, and a bad name refuses the
  start with `shell_wrapper_problem:` while `-print-config` still works.
* **The strip is bounded** by the number of configured wrappers, and a segment that is
  only a wrapper is never stripped to nothing: an empty command must never be the thing
  that gets judged.

## Alternatives rejected

* **Bake `rtk` into the allowlist.** Fixes one host's plugin for everyone, puts a vendor
  name in the core (rule 14), and leaves the next rewriting plugin — a different tool, a
  shell function, a wrapper script — failing exactly the same way.
* **Strip the first word unconditionally and judge the rest.** The tempting
  one-line version, and it breaks the anchor the allowlist depends on: any prefix
  whatsoever would be tolerated, and the policy would stop being a statement about the
  command that runs. `rm -rf /` refuses nothing today only because it is judged as a
  whole segment; a blanket strip changes the meaning of every entry.
* **Ask the model to report the rewrite, with no operator knob.** Correct and cheap, but
  it leaves every host that genuinely wants the plugin unable to work at all. The request
  that produced this ADR was "the agent should be able to check git status", not "the
  agent should be able to explain why it cannot".
* **Reject the rewrite and require the plugin to be removed.** Safe, and it makes FlowHub
  the arbiter of which editor/agent plugins an operator may install. The prefix is the
  operator's statement about their own host; the shell policy's job is to judge what
  remains, not to forbid the plugin.
* **Read the model's original command from somewhere and compare.** There is nowhere to
  read it: the permission request carries the rewritten text only, the session's tool
  part carries the original but is not part of the request, and the two arrive on
  different endpoints at different times. The diagnostic therefore has to be inferred
  from the shape of the refused command, which is what `looksWrapped` does.
* **A per-project or per-source wrapper list.** The rewriting plugin is installed on the
  *host* and applies to every directory that host serves; keying it to a tracker project
  would invite two answers for one machine.
* **Deduplicating a repeated entry silently.** A duplicate refuses, for the same reason
  the environment file refuses one: which entry is in force must not be a guess.

## Consequences

* **The git allowlist is reachable again on a host with a rewriting plugin**, which is a
  fix for the whole class and not for `rtk`: any future prefix is one configuration line.
* **The deny list is re-judged on the stripped text**, so `Trust` can never be read as
  "this command is safe". The refusal for a stripped payload says that the wrapper is
  what made the command reachable and the payload is what refused it, so the operator is
  not invited to "just trust it harder".
* **A rewrite is now visible in the log**, which is the part that generalises: a wrapper
  in a refusal reason is a fact about the host that no log line previously recorded.
* **The diagnostic can misfire on an unrelated two-word command.** `time git status`
  (if `time` were not itself denied) is reported as a wrapper candidate. That is a
  deliberate trade: the message is advice attached to a refusal that already stands, and
  a false mention costs a sentence where a missing one costs a turn.
* **The wrapper list is a new policy surface.** It is bounded
  (`MaxShellWrappers`), grammar-checked, logged at startup, refused when malformed, and
  defaulted to empty. Nothing strips until an operator writes a name down.
* **The model-facing refusal got longer.** It is written for a model that cannot see the
  rewrite and for an operator reading the log afterwards, which is the tension this ADR
  exists to resolve; the tests assert the four facts it must carry (the wrapper, the
  hidden command, that retrying is futile, and the knob) so a later edit cannot quietly
  drop one.
