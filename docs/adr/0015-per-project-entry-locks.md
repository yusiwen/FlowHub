# ADR 0015 — Entry locks belong to the project, not to the process

Status: accepted (2026-10-26, issue #171)

Revision 2026-10-26 (#173): the first version required the values inline. Each field
now names exactly one of three sources — inline, an environment variable, or a
one-secret file — because the routing table is the file an operator copies and
reviews, and every other secret in this project can already name a variable or a
file instead. The security properties below are unchanged; the `0600` rule now
applies only to a table that actually carries an inline value.

## Context

The two webhook entry locks were process-wide settings:

- `FLOWHUB_HOOK_KEY` (and its `FLOWHUB_YOUTRACK_HOOK_KEY` spelling) was the URL
  path key every delivery had to carry;
- `FLOWHUB_TOKEN` (and `FLOWHUB_YOUTRACK_TOKEN`) was the `X-YouTrack-Token` value
  every delivery had to present.

That was coherent while FlowHub served one repository. It stops being coherent
once the routing table holds several projects — which it does, and which the flow
engine makes normal:

- a delivery cannot be attributed to a project before its body is read, so the
  receiver cannot know which project's token to expect;
- rotating one project's key or token changes every project, and a mistake in one
  project's YouTrack settings silently affects the others;
- a key that leaks from one project's app settings authenticates a payload for
  **any** mapped project, because the receiver has no notion of "this key belongs
  to TEST".

The sender already has the right shape. YouTrack's Webhook Triggers app is
configured **per project** (`项目 → Settings → Apps → Webhook Triggers →
Settings`); each project attachment has its own `webhookToken` and per-event URLs
(security document §2.1). A receiver that keeps one process-wide pair is strictly
weaker than the thing that posts to it.

## Decision

1. **The locks live on the project's source binding.** `projects[].source` becomes
   an object:

   ```json
   {
     "source": {
       "name": "youtrack",
       "hook_key_env": "FLOWHUB_HOOK_KEY_TEST",
       "token_env": "FLOWHUB_TOKEN_TEST"
     },
     "project": "TEST",
     "repo": { "path": "/absolute/path" }
   }
   ```

   Each field names exactly one of: `hook_key`/`token` (inline, at least 32
   characters), `hook_key_env`/`token_env` (a variable, usually from the
   environment file that is applied before the table is read), or
   `hook_key_file`/`token_file` (a `0600` file holding one secret). Two sources for
   one field is a load error, and a named variable or file that is missing or empty
   refuses the load rather than becoming a silently disabled lock. The value is
   resolved once at load time, so rotation needs a restart.

   The bare string form (`"source": "youtrack"`) still parses — a disabled entry
   and a version 1 table need it — but an enabled entry of the source this process
   serves must resolve both values. `projectmap.LockProblems(source)` is the
   startup gate; it is scoped to the served source because the lock material is
   that source's addressing scheme, not a fact about the file.

2. **The key selects the entry before the body is read.** The receiver scans its
   configured entries with a constant-time comparison per key. An unknown key is
   `bad_url_key`. After the key matches, the entry's token is checked, then the
   source IP, and only then is the body read and decoded. Routing stays a
   comparison against configured secrets, never a parse of attacker-controlled
   JSON.

3. **The payload must belong to the entry.** After decoding, the event's
   `Subject.Project` must be one of the entry's keys (its `project` plus `also`),
   case-insensitively; otherwise the delivery is rejected as the new
   `project_mismatch` reason. Without this check a key for TEST could post a
   BEAP_BE event: dispatch and the flow engine route by the *payload's* project.

4. **Inline lock material makes the configuration file a secret.** A file that
   carries `source.hook_key`/`source.token` inline must be mode `0600`; the same
   check that protects an inline `auth.password` covers it, and it runs whether or
   not dispatch is on. A table that only names variables and files carries no
   secret and stays copyable. `-print-config` reports each lock's provenance
   (`inline`, `env NAME`, `file PATH`) and never its value.

5. **The global variables are removed.** `FLOWHUB_HOOK_KEY`, `FLOWHUB_TOKEN`,
   `FLOWHUB_YOUTRACK_HOOK_KEY` and `FLOWHUB_YOUTRACK_TOKEN` are no longer read.
   A process that still has one set logs a warning naming it, because a setting
   that silently does nothing is the failure this project refuses. The header
   *name* stays global (`FLOWHUB_TOKEN_HEADER`): it is part of the app's wire
   format, not a secret.

6. **Key hygiene is enforced at load.** Both values are at least 32 characters;
   the key must be URL-safe (`A-Z`, `a-z`, `0-9`, `.`, `_`, `~`, `-`) because the
   app treats a comma as a URL separator and a slash would change the route. A
   duplicate key within one source is refused: a key must select exactly one
   project. Errors name the projects, never the values.

## Alternatives rejected

- **Parse the body first and pick the token from the payload's project.** That
  reverses the order the entry locks exist for: unauthenticated, unbounded (up to
  the body cap) input would be parsed before it is authenticated. The URL key is
  the only piece of the request that can select a project without trust.
- **Keep the environment variables but rename them per project**
  (`FLOWHUB_HOOK_KEY_TEST`). It still leaves the binding outside the routing
  table, so "which project does this key belong to?" has two answers in two
  files, and the enablement of a lock can drift away from the project it protects.
- **A map keyed by the secret** for the lookup. It is faster than the scan but
  makes the configured secrets the hash input of a data structure; the scan is
  constant-time per comparison and the number of projects is public anyway.
- **Optional locks.** Allowing an enabled project without a key would mean a
  delivery that can never be attributed; allowing one without a token would mean
  a project reachable by its URL alone. Both fail at startup instead of at the
  first delivery.

## Consequences

- A record-only receiver now needs at least one project entry, because a delivery
  must be attributed before it can be accepted; the preferred form keeps the values
  in the environment, so that table can still be copied and reviewed.
- A configuration whose only secrets are references is not permission-checked;
  inlining either lock turns the `0600` requirement back on for that file. This is a deliberate change of the
  first-run story: configure the project first (the routing table is already
  required for anything interesting), then post.
- Version 1 files still translate, but they cannot express the locks; a served
  source's version 1 table is refused at startup with the migration named.
- `also` projects share their entry's lock. Several tracker projects that share
  one repository must be configured with the same app URL key and token in their
  own Webhook Triggers settings; the payload check accepts any of the entry's
  keys. If they need distinct secrets, they need distinct entries.
- Rotation is per project: change that project's `source` pair and its YouTrack
  settings; no other project is affected, and the audit shows `bad_url_key` /
  `bad_header_token` only for the project being rotated.
- `scripts/smoke.sh` and the E2E harness write the test project's locks into the
  config they generate; the E2E still reads `FLOWHUB_HOOK_KEY`/`FLOWHUB_TOKEN`
  from its own environment as *inputs*.
- The audit record's `active_locks` stays a lock-type summary; the project a
  delivery matched is visible through its `project_key`, and a mismatch keeps the
  payload's project in that field.
