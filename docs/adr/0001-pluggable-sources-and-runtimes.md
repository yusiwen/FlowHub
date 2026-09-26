# ADR 0001 — Pluggable event sources and agent runtimes

**Status:** Proposed, **partly implemented ahead of the plan** (2026-09-26).
Migration **steps 1, 2 and 3 landed** (the source seam; the runtime and workspace
seams; the configuration format), plus most of step 5, because step 5 does not depend
on them: the
projects file accepts `runtime`, `runtimes` and `runtime_policy` (per project, with
a table-wide default of `spread`), the dispatcher ranks the eligible runtimes by the
rules below and logs the numbers it chose by, startup validates the declared names
against the inventory, `/healthz` reports the in-flight counters, and the baseline
is pinned — `localworktree.Provider.Resolve` asks the **origin** for the commit,
`Prepare` produces exactly it (fetching it when a stale clone has never seen it, and
refusing to attach to a same-named branch that is not its descendant), and
`registry.Task.BaseCommit` records it once and never re-resolves it. **One queue and
one worker per runtime** landed as well: an intake loop decides which runtime takes a
delivery and hands it to that runtime's own queue, so different runtimes run turns in
parallel while one runtime still runs them one at a time. Step 4 is open, so a second source
does not exist yet; the v2 format exists (step 3a) and the addressing and the baseline
live behind the `Workspace` seam, where the `remote` provider (step 6) will answer the
same interface. One measured consequence of the local
provider: a repository with
no `origin` remote has no shared truth to pin, so `Resolve` answers from the local
ref and **says so** (a warning on every task that creates its worktree), because the
guarantee this section describes only exists once there is an origin.

Step 1 is in the tree: `internal/event` is the neutral IR, `internal/source` is the
seam (`Request`, `Decoded`, `Source`, and the data-only `ToolPolicy`), and
`internal/source/youtrack` owns everything YouTrack-shaped — the payload model it
was moved into, the schema report, the tool allowlist, the reply-tool names, the
download host and prefix, the per-turn prompt, and the workflow-state
normalisation. `internal/rules` decides on `event.Event` and no longer knows a
changed-field name; `internal/webhook` keeps the transport locks and the redaction
and hands the pipeline to `Source.Parse`; `internal/dispatch` imports no vendor
package (`go list -deps` shows no `internal/webhook` edge), takes its policy from
`Source.Policy()` instead of a second `Policy` field, and gets the tool allowlist,
the reply check and the download policy from `Source.Tools()`. The audit record is
unchanged by design (the `envelope` + `source_facts` split is still deferred to the
second source), which is why `Decoded` carries the source-shaped facts the record
keeps. Two deviations from the letter of the step, both deliberate: the twelve
existing rule/prompt test functions were **moved** to the adapter rather than
rewritten, and four dispatch constructors gained a `Source` argument — no
assertion was dropped, and the raw timestamp spelling the refactor could have
silently re-rendered is now pinned by a new receiver test. The refactor was also
verified live: a real TEST-30 delivery produced one analysis turn and its reply,
and an echo of that reply was recognised as FlowHub's own and not dispatched.

Step 2 is in the tree as well, which is what makes the topology claim real:
`internal/agent` holds the runtime seam (`Runtime{Name, Health, Run}`, the neutral
`Turn`/`Result`/`ToolCall`/`PermissionDecision`/`Tokens`, `Phase`, the attachment
`Downloads` policy, and the `provider/model-id` spelling the configuration also
validates against); `internal/agent/opencode` is the opencode implementation of it,
with `runtime.go` as the thin adapter and the session ruleset and shell policy moved
in from `dispatch`; `internal/workspace` holds the workspace seam
(`Entry`/`Request`/`Handle`/`Base`, `Workspace{Resolve, Prepare, Check, Remove}`, and
`Validator`/`ValidateEntries`); `internal/workspace/localworktree` is the co-located
provider, and the filesystem checks that used to live in `projectmap` now live in its
`Validator`. `internal/dispatch` imports the seams and neither implementation —
`go list -deps ./internal/dispatch/` shows no `internal/agent/opencode` and no
`internal/workspace/localworktree` edge — because `main` injects a runtime factory, a
default runtime and a workspace factory. `FLOWHUB_OPENCODE_USER`/`PASSWORD` are wired
into both client builders (the runtime factory and the activation prober), which is
what makes a runtime that is not on loopback usable at all.

Four deviations from the sketch, all deliberate:

* **`Turn` carries the source's tool allowlist, not a ruleset.** The sketch had
  `Ruleset []PermissionRule`; a `PermissionRule` is opencode's spelling, so passing
  one would have kept a product type in the dispatcher. The runtime builds the
  ruleset from `Turn.AllowedTools` and the phase, which is also what makes "which
  permissions a session starts with" genuinely the runtime's.
* **`Resolve` is on the interface.** The sketch lists only `Prepare`/`Check`/`Remove`,
  but step 5's pinned baseline landed before this seam existed and the baseline is a
  workspace fact — the origin may not even be reachable from the control plane. The
  `Handle` in the sketch already carries `BaseCommit`, so this closes the loop rather
  than adding a new idea.
* **Entry validation stays a startup gate.** The filesystem checks moved behind the
  provider as `Validator` instead of disappearing: invariant 7 ("routing never guesses
  a repository") must keep refusing at startup, before any file exists. Two refusal
  *messages* changed wording (they now name the provider's own vocabulary) while the
  set of refusals is unchanged.
* **Two places still name the product on purpose.** `main` wires it; and
  `internal/provision` plus this binary's activation prober install and verify
  opencode's own files and read its agent registry and model catalogue. A
  runtime-neutral capability check would need a speculative interface for those two
  endpoints, so step 2 leaves them product-specific and says so.

**Revision:** 10 — recorded step 3a as landed: the v2 format, the v1 translation, the
`(source, project)` index, per-level precedence with provenance, the source registry
and the runtime/source blocks; plus the two deferrals (3b `prompt_file`, and the
location move waiting for the provider that needs it). Revision 9 — recorded step 2
as landed: the `agent` and `workspace` seams, the
`localworktree` provider owning the local checks, the credentials wiring, and the four
deviations above. Revision 8 — recorded step 1 as landed, with what it moved and the
two deviations (tests moved rather than left in place; the dispatcher's second policy
field removed rather than kept in step). Revision 7 — made the baseline an input:
resolve the base ref to a commit
through the origin once per task, pin it, require every host to produce exactly
that commit, and refuse to attach to a same-named branch that is not its
descendant. Worktrees are never synced between hosts; commits travel through the
origin. Revision 6 — pinned down what a `runtimes` list means: an eligibility set,
with `runtime_policy` (`spread` by default, `first-healthy` for strict order),
deterministic tie-breaks, failover only at task creation, and startup that refuses
only when no runtime answers; names rather than labels (decided in review).
Revision 5 — the target topology is a control plane plus one or more agent
hosts, so runtimes are addressed by name, a task binds to one for life, and each
runtime gets its own queue, worker, credentials and workspace provider; revision 4
made FlowHub a control plane: added the `Workspace` seam and the
topology section, split repository identity from workspace location in the config,
and recorded the three things the split changes (repository attestation, branch
delivery, cleanup ownership); revision 3 dropped the `locks` block; revision 2
added the configuration format
**Date:** 2026-09-21
**Scope:** the shape of the seam between "an event happened somewhere" and "an
agent works on it", including the configuration surface it needs. Step 1 of the
migration plan is implemented; the configuration change (step 3) is not, and the
audit record deliberately stays as it is.

## Context

FlowHub is positioned as a workflow hub that aggregates DevOps nodes and agent
runtimes (`opencode-devops-orchestration-design.md`). Today it implements exactly
one of each: YouTrack as the event source, `opencode serve` as the agent runtime.
The other two planned sources (Gitea, Drone) are named in `AGENTS.md`.

The question this ADR answers: **can adding a source, or swapping the agent
runtime, avoid touching the orchestration core — and if so, where exactly is the
seam?**

Measured coupling today (non-test `.go` files, lines mentioning `youtrack`):

| Package | Lines | What the coupling actually is |
| --- | --- | --- |
| `internal/rules` | 13 | Field *semantics* (`Delivery.States`, `CommentText`) and the prose in `prompt.go`. Imports only `registry`. |
| `internal/opencode` | 4 (1 comment) | Nothing structural: client, runner, arbiter and phase logic are source-agnostic. |
| `internal/worktree` | 1 | A comment. |
| `internal/registry` | 2 | Field names and comments. |
| `internal/webhook` | 5 | The payload model is YouTrack-shaped; the three entry locks are transport-level and source-agnostic. |
| `internal/projectmap` | 18 | `youtrack_key`, `also_keys`, the issue-ID prefix fallback. Genuinely source-specific. |
| `internal/dispatch` | 14 | `webhook.Parse` + `describe()` (the translation), `AllowedMCPTools` (`youtrack_*` tool allowlist) and the reply check `call.Name == "youtrack_add_issue_comment"`. |
| `internal/agent` | — | Does not exist yet; `sessionRuleset()` currently lives in `dispatch`. |

Two facts make the seam cheap to introduce:

1. **There is exactly one translation point.** `dispatch.describe(rec,
   *webhook.Payload) → rules.Delivery` (`internal/dispatch/dispatch.go:512`),
   fed by a single `webhook.Parse` call (`dispatch.go:198`). Everything
   downstream already consumes `rules.Delivery` / `registry.Task`, not
   `webhook.Payload`.
2. **The receiving half already has a seam.** `webhook.Dispatcher` is an
   interface, so the receiver does not know what happens next.

What is *not* clean is the ownership of knowledge: source knowledge
(`AllowedMCPTools`, the reply tool name) and runtime knowledge (`sessionRuleset`,
`AllowedMCPTools`'s relationship to `bash` rules) both live in `dispatch`.

## Decision drivers

1. Adding a source must not require editing `dispatch`, `opencode`, `worktree` or
   `registry`.
2. The security boundary must survive the refactor. A source may declare **which
   tools** the agent may call; a source must never be able to widen the **shell**
   allowlist or the external-directory rules (`AGENTS.md` invariant 8).
3. The audit trail must stay greppable and replayable by the `jq` recipes in
   `README.md`.
4. Standard library only, one binary, one process, two users (see `AGENTS.md`).
5. Decisions that are still moving (structured output, the confirmation gate,
   reconciliation polling) must not be frozen into interfaces.
6. Distinguish the axes. A source, a workspace and a runtime are three
   independent things, and a design that conflates them will be wrong for all
   three.
7. **Do not assume FlowHub and the runtime share a filesystem.** They are
   co-located during development only; the target deployment has FlowHub on a
   small always-on host and the agent runtime where the repositories live.

## Considered options

### A. In-process interfaces with a compile-time registry — **chosen**

`internal/event` holds a neutral IR; `internal/source` holds the `Source`
interface plus one adapter per source, registered in `main` (or by blank import).
`internal/agent` holds a `Runtime` interface that `opencode` implements.

* Cost: one refactor (~1 day) plus 150–300 lines per new source (signature
  verification, payload parsing, one prompt).
* Buys: new sources and new runtimes are additive; the core never learns their
  names.
* Cost we accept: adding a source requires a rebuild. For a single-binary,
  two-user deployment that is the same as editing config.

### B. Configuration and prompt templates only (no code) — rejected as the primary form

Declare event names, trigger words, state fields and the prompt text in files.

* Buys: prompt and trigger changes without a rebuild.
* Why not primary: signature verification (YouTrack header token vs Gitea
  `X-Hub-Signature-256` HMAC vs Drone HMAC) and payload parsing cannot be
  expressed as configuration without inventing a small language, and a debugging
  session that turns into "guess the template" is exactly what this project's
  log-first discipline exists to avoid.
* Kept as a **complement** (migration step 3): policy and prompt text become
  per-source files once a second source exists to prove the shape is right.

### C. Go plugins (`.so`) — rejected

* Requires identical toolchain, dependency versions and build flags between host
  and plugin; failure mode is a load error at startup or a crash mid-turn.
* Needs cgo on macOS, breaking the current `CGO_ENABLED=0` static build.
* The security model would have to be re-drawn: a `.so` runs in-process with full
  authority, so "a source can only declare tool allowlists" stops being
  enforceable.

### D. Out-of-process plugins over IPC — rejected

* Would give real isolation and independent releases, at the cost of a protocol,
  supervision, a second credential store and a much larger failure surface
  (`nginx`-style restart semantics, message loss on restart, versioning).
* Solves a problem this deployment does not have: there is no third party
  writing plugins, and no plan to publish one.

## Decision

Adopt **A**, with the following shape.

### Three axes, three interfaces

```
                                                        control plane
YouTrack ─┐                                  ┌──────────────────────────────┐
Gitea    ─┼→ [Source] → event.Event → [dispatcher] → [Workspace] ─┐        │
Drone    ─┘     ↑                            ↑          ↑         │        │
          verify/parse/policy/prompt    rules/registry/audit     │        │
                                                                 │  data plane
                                               [agent.Runtime] ←─┘        │
                                                     │  opencode serve      │
                                                     └→ a per-task working directory
```

Three seams, not two:

| Seam | Owns | First implementation |
| --- | --- | --- |
| `Source` | decoding an event, its policy, its prompt, its tool allowlist | `youtrack` |
| `Workspace` | producing a dedicated working directory for a task, on *its* filesystem | `localworktree` (co-located) |
| `agent.Runtime` | driving one turn inside that directory | `opencode` |

Revision 1 collapsed `Workspace` into "FlowHub runs `git worktree add`", which
silently assumed one machine. The consequences of that assumption, and the
`Workspace` contract that replaces it, are in the next section.

### `internal/event` — the neutral IR

```go
type Kind string // created | commented | state_set | updated | deleted | other

type Attachment struct{ Name, URL, MIME string; Bytes int64 }

type Subject struct {
    Project     string // routing key as the source spells it
    Key         string // stable task key, e.g. "TEST-17"
    Title       string
    Body        string
    URL         string
    Attachments []Attachment
}

type Event struct {
    Source   string    // "youtrack" | "gitea" | "drone"
    Kind     Kind
    Subject  Subject
    Actor    string
    State    string    // new workflow state, already normalised by the adapter
    StateField string  // which field carried it, for the log only
    Comment  string    // the comment that triggered this turn, if any
    Occurred time.Time
    Raw      []byte    // audit only; never parsed twice
}
```

`rules.Delivery` becomes this type (or is replaced by it), so `rules` keeps its
current property of importing nothing but `registry`.

### `internal/source` — the source adapter

```go
type Request struct { // what the receiver hands over, after the entry locks
    Method, Path, Query string
    Headers             http.Header
    RemoteIP, UserAgent string
    Body                []byte
}

// ToolPolicy is data, not behaviour: the dispatcher performs the checks, so a
// source cannot smuggle in logic.
type ToolPolicy struct {
    Allowed  []string // MCP/plugin tools the agent may call unattended
    Reply    []string // a completed call to any of these means "the turn replied"
    Download struct{ Hosts []string; Prefix string }
}

type Source interface {
    Name() string
    Parse(req *Request) (Decoded, error)      // verify signature + decode, plus the source's audit facts
    Policy() rules.Policy                     // trigger, start states, self markers, budget
    Tools() ToolPolicy
    Prompt(action rules.Action, e *event.Event, ctx rules.PromptContext) string
}
```

Decisions inside this section:

* **`Parse` returns `Decoded`, not a bare event** (as implemented in step 1). The
  receiver's audit record still carries source-shaped fields — the issue-id form,
  the payload key set, the flat schema report, the comment ids — and those cannot
  live on `event.Event` without making the IR a YouTrack union. `Decoded` is the
  event plus those facts; the deferred `envelope` + `source_facts` split is what
  eventually moves them into the record generically.
* **The three entry locks stay in the receiver.** They are transport-level (URL
  key, header token, source-IP allowlist) and every source deserves the same
  ones. `Source.Parse` only verifies *source* signatures (for example a Gitea
  HMAC), which the receiver cannot know about.
* **`ToolPolicy` is data.** The current hard-coded reply check
  (`dispatch.go:420`) and `AllowedMCPTools` (`dispatch.go:44`) become fields of
  the YouTrack adapter. `dispatch` keeps the generic loop
  (`anyCompleted(result.Tools, spec.Tools.Reply)`), so the source never gains the
  ability to widen the shell allowlist.
* **`Policy.StateFields` disappears.** The adapter knows which field carries the
  workflow state and normalises it into `Event.State`; the policy only lists the
  *values* that mean "start implementing".
* **Attachment downloads stay constrained** by `ToolPolicy.Download` and the
  existing prefix, which the arbiter already enforces (`opencode.CurlHosts`,
  `CurlOutputPrefix`). `opencode` should receive that policy as a parameter
  rather than reading a constant.

### `internal/agent` — the runtime

```go
type Turn struct {
    Directory, Prompt, Agent, Model, Title, SessionID string
    Phase        Phase      // analysis | execution
    AllowedTools []string   // the source's allowlist; the runtime builds its own ruleset
    Downloads    Downloads  // the source's attachment policy, enforced by the runtime
    Metadata     map[string]any
    Deadline     time.Duration
}

type Result struct {
    SessionID string
    Text      string
    Finished  bool
    TimedOut  bool
    Cost      float64
    Tokens    Tokens
    Tools     []ToolCall // {Name, Status}
    Permissions []PermissionDecision
}

type Runtime interface {
    Name() string
    Health(ctx context.Context) (version string, err error)
    Run(ctx context.Context, turn Turn) (Result, error)
}
```

`opencode.Runner` already matches this shape; the adapter is a thin wrapper.
`sessionRuleset()` moves out of `dispatch` into the runtime, because "which
permissions the session starts with" is a runtime property, while "which tools a
source needs" is the source's. As implemented, `Phase` and `Downloads` travel on the
`Turn` and the ruleset is *not* one of its fields: a ruleset is this product's
spelling, and building it in the dispatcher would have kept a product type in the
core.

### Deployment topology: FlowHub is a control plane

FlowHub and the agent runtime are separate hosts in the target deployment, even
though they run side by side during development. Four things in the current code
assume otherwise, and all four are about the *workspace*, not about the protocol:

| Assumption today | Where | Whose fact it really is |
| --- | --- | --- |
| `repo.path` exists, is a git work tree, and its `origin` matches `repo.remote` | was `internal/projectmap/validate.go`; now `localworktree.Validator` | the runtime host |
| the worktrees base exists and sits outside the repository | was `internal/projectmap/validate.go` + `dispatch.Problems`; now `localworktree.Validator` | the runtime host |
| `git worktree add` runs as a child process | `internal/workspace/localworktree` | the runtime host |
| the registry stores a *local* absolute path as the task's directory | `internal/registry` (the path is already treated as opaque) | the runtime host |

Everything else is already transport-safe, which is why this is a seam and not a
rewrite: `?directory=` is an opaque HTTP query parameter that FlowHub never reads
(`internal/opencode/client.go`), the permission loop is pure HTTP and the commands
it judges run on the runtime side, the attachment prefix is relative
(`.flowhub/attachments`), and the client already speaks Basic Auth for
`OPENCODE_SERVER_PASSWORD` — which the configuration does **not** wire up yet, a
gap to close before the link leaves loopback.

```go
package workspace

// Entry is the part of a routing entry a provider can verify before anything is
// created. Plain data: the configuration layer builds it, so the seam never learns
// a configuration format.
type Entry struct {
    Label         string
    Repo          string // local path, or the remote URL for a provider that clones
    Remote        string // the repository identity the entry declares
    DefaultBranch string
    Base          string // checkout root, the process-wide fallback already applied
}

// Request describes the workspace one task needs.
type Request struct {
    TaskKey       string // stable, human readable: "TEST-17"
    Repo          string
    Remote        string
    DefaultBranch string
    BaseRef       string // the branch to start from
    BaseCommit    string // pinned by Resolve; every host must produce exactly this
}

// Handle is what FlowHub keeps in the registry and hands to the runtime. Path is
// opaque: FlowHub never stats, joins or globs it, because it names a directory on
// somebody else's machine.
type Handle struct {
    Provider   string // "localworktree", "remote"
    Path       string // as the runtime spells it
    Repo       string // the provider's attestation of which repository it used
    BaseCommit string // pinned at creation; every host must produce exactly this
    Reused     bool
}

type Workspace interface {
    Name() string
    // Resolve reports the commit BaseRef points at as the shared origin sees it, so
    // that every host pins the same baseline. Step 5 landed before this seam existed
    // and the baseline is a workspace fact — the origin may not even be reachable
    // from the control plane — so it belongs here.
    Resolve(ctx context.Context, req Request) (Base, error)
    Prepare(ctx context.Context, req Request) (Handle, error)
    // Check reports drift: the workspace is gone, or it no longer belongs to the
    // repository the task was created against.
    Check(ctx context.Context, h Handle) error
    Remove(ctx context.Context, h Handle) error
}

// Validator is what a provider implements when it can check an entry before any
// provider instance exists — startup refuses a bad entry before a directory is
// created, so the check cannot depend on a constructed base.
type Validator interface {
    Validate(entry Entry) []string
}

func ValidateEntries(v Validator, entries []Entry) []string
```

* `localworktree` is today's `internal/worktree`, renamed and behind the
  interface. It keeps the `git worktree` strategy, because on a host with a
  persistent clone that is still the cheapest isolation: one object store, one
  directory and branch per task, milliseconds to create. It is also the `Validator`
  that owns the checks `projectmap` used to run — the repository exists, is a git
  work tree, has the declared origin, and the checkout root sits outside it.
* `remote` is a later provider. It does **not** mean FlowHub holds SSH keys to the
  runtime host: that would make FlowHub a remote executor, which this design
  avoids. It means the runtime side runs a small helper that FlowHub calls over
  the same kind of authenticated HTTP link it already uses for opencode, and that
  helper owns cloning, worktrees and disk. The provider may equally be "a
  container volume per task" — the interface deliberately says workspace, not
  worktree.

Three consequences of the split that the ADR treats as first-class, because each
one weakens or changes something the current design gets for free:

1. **Repository verification becomes a handshake.** "Never route to a guessed
   repository" is enforced today by reading the local `.git/config` at startup.
   Across hosts FlowHub cannot read anything, so `Handle.Repo` — the provider's
   own statement of which repository it prepared — is what gets compared against
   the routing entry, and recorded in the audit. If the provider cannot attest to
   it, the task is refused. This must not degrade into "the string matched once, so
   trust the path".
2. **The branch has to reach the reviewer.** Co-located, the agent's commits land
   in the same clone the human already has, so `git log flowhub/TEST-17` just
   works and nothing is pushed. Split, the commits are on the runtime host, and
   the workflow needs a transport: allow the agent to push its own task branch to
   the shared origin (which means relaxing the current "never push" invariant to
   "never push outside `flowhub/<task key>`"), or have the reviewer fetch from the
   runtime host, or have the provider export a patch. This is a product decision,
   not an implementation detail, and it is open question 8 below.
3. **Cleanup belongs to the provider.** `Remove` currently has no caller, so
   directories accumulate silently. Once the disk is somebody else's, FlowHub must
   be able to ask for removal and report what it found (`list` with size and dirty
   state) rather than reaching for `os.RemoveAll` on a path it cannot see.

The topology also decides what "one runtime" means: with more than one runtime
host, `runtime` is no longer a process-global block but a per-project override,
which is why the configuration sketch below puts it at the top level *and* allows
it per project.

### Addressing multiple runtimes

Confirmed topology: FlowHub runs on the small always-on host (gateway/aliyun),
`opencode` runs on one or more performance hosts, and there will be **more than
one**. That turns "the runtime" from a process-global fact into an addressed
resource, and it changes three things: who picks a runtime, where the choice is
remembered, and how work is queued.

**Identity and configuration.** A runtime is a named block. Everything that
differs between hosts hangs off that name: endpoint, credentials, agent and model
defaults, concurrency, and the workspace provider — because the workspace is the
far side's filesystem.

```json
{
  "_comment": "An excerpt: only the runtimes block of the configuration below.",
    "runtimes": {
      "local": {
      "url": "http://127.0.0.1:4096",
      "agent": "devops",
      "workspace": {
        "provider": "localworktree",
        "base": "/Users/yusiwen/git/mine/test-worktrees",
        "clones": { "youtrack:TEST": "/Users/yusiwen/git/mine/test" }
      }
    },
    "builder-a": {
      "url": "https://builder-a.lan:4096",
      "auth": { "user": "opencode", "password_env": "FLOWHUB_RUNTIME_BUILDER_A_PASSWORD" },
      "agent": "devops",
      "model": "deepseek/deepseek-v4-flash",
      "deadline": "15m",
      "max_concurrent": 1,
      "workspace": { "provider": "remote", "runner": "https://builder-a.lan:8443", "base": "/srv/flowhub-worktrees" }
    }
  }
}
```

Physical paths appear **exactly once** in FlowHub's configuration, under the
runtime that owns that filesystem — the same rule that removed the `locks` block.
A `remote` provider keeps its own copy of the repo→path mapping, so FlowHub sends
identities (`remote`, `base_ref`, `task_key`) and never a path. `workspace.clones`
is keyed by `<source>:<project>`, the same key the routing table uses, so a project
and its checkout cannot drift apart silently.

**Selection is a routing decision, not an event decision.** A project names one
runtime, or a list of them for capacity:

```json
{ "source": "youtrack", "project": "BEAP_BE",
  "repo": { "remote": "git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git", "default_branch": "master" },
  "runtimes": ["builder-a", "builder-b"], "runtime_policy": "spread" }
```

A list means **"any of these may serve this project"** — an eligibility set, not
an ordered preference list and not a promise to spread evenly. Two different
questions follow from it, and conflating them is what makes a scheduler
unpredictable:

| Question | Answer |
| --- | --- |
| Which runtime takes this **new** task? | A policy over the eligible, currently reachable runtimes |
| Which runtime takes the **next turn** of an existing task? | The one already bound to it, always, even if it is now unhealthy |

`runtime_policy` chooses the first, with two values:

* `spread` (default) — balance by task, so a second machine is actually used.
* `first-healthy` — strict list order, for when one host should take everything
  and the rest exist only as failover.

`spread` resolves in this order, and every tie-break is deterministic:

1. Keep the runtimes that are configured, healthy, and able to prepare a workspace
   for this project.
2. Prefer the fewest turns **in flight or queued** right now (live counters, the
   same ones `/healthz` reports).
3. Then prefer the fewest **active** tasks in the registry for that runtime —
   tasks whose state is not terminal — so two idle hosts do not both look empty.
4. Then list order, which is what makes a fresh registry behave predictably.

The chosen runtime and the reason are logged in one line
(`runtime chosen: builder-b (in_flight 0, active 1, eligible 2)`), so "why did
this land there" is answerable from the log without re-deriving it.

Failover happens at **creation** only, and it falls out of step 1: if the first
candidate does not answer, or cannot prepare a workspace, the next eligible one is
tried, and the task binds to whichever succeeded. Afterwards there is no failover:
a task whose bound runtime is unreachable is refused with that reason, because a
new session on another host is a fresh context that has lost the analysis and the
plan. A consequence worth stating: after an outage, tasks created during it stay
on the fallback host while new tasks go back to the preferred one under
`first-healthy`, or are spread again under `spread`. That is intended — the
binding is per task, not per host.

Startup follows the same shape: every configured runtime is probed, each
unreachable one is a warning naming it, and startup is refused only when **no**
runtime answers, since the point of having several is that one may be down.

**Never from event content**: which machine runs code decides which network, which
credentials and which repositories are reachable, so it is an authorization
decision. An issue author who could name a runtime could ask for the host with the
production kubeconfig on it. If a per-task hint is ever wanted (a YouTrack custom
field, say), it may only *narrow* the project's eligible set, and it is refused
when it names anything outside it — the same shape as the existing "the routing
table never guesses a repository" rule. Names, not labels, for now (decided in
review): a label indirection adds a failure mode ("no runtime satisfies this
project") that two or three machines do not yet justify.

Startup also validates the set itself, so a typo fails immediately instead of
making a project quietly unroutable: every name in `runtime`/`runtimes` must exist
in `runtimes`, every named runtime must be able to serve that project (for
`localworktree`, that means a `clones` entry for `<source>:<project>`), and a
`remote` provider must be able to answer for it.

**The choice is sticky, and recorded.** A task binds to the runtime that created
its workspace and session, for the rest of its life:

* `registry.Task` gains `Runtime` (the name) next to the workspace handle.
* A later event for a bound task goes to that runtime even if the project's
  `runtimes` list changed. A session cannot move between hosts: a new session
  elsewhere is a fresh context that has lost the plan and the analysis.
* If the bound runtime is no longer configured, or does not answer, the delivery
  is refused with that reason in the log and the task's state, rather than being
  silently re-homed. This is the same shape as today's refusal to reuse a session
  when the routing table now points at a different repository.
* Binding happens once, after a workspace was prepared and a session created; if
  every candidate fails, nothing is bound and the next delivery may try again.
* Rows written before this change have no runtime name. They are read as the
  configured default runtime if one is marked `"default": true`, and refused
  loudly otherwise rather than guessed.

**One queue per runtime.** Today one worker serializes everything because a prompt
sent to a busy session is silently swallowed. That reason is per *session*, not
per server, so the unit of serialization becomes the runtime: each runtime gets
its own queue and its own single worker, and different runtimes run turns in
parallel. `max_concurrent` exists in the sketch but stays at 1 until per-task
locking is in place; the field is there so raising it later is configuration, not
a redesign. `/healthz` reports per runtime: healthy, queued, in flight, and the
last error, so "which host is stuck" is one HTTP call.

**Credentials stay in the environment, named per runtime.** The configuration may
say *which* variable holds a password (`password_env`), never the password
itself — same rule as the URL key and the token. The client already speaks Basic
Auth against `OPENCODE_SERVER_PASSWORD`; what is missing is the wiring, and it is
needed before the link leaves loopback. Over a private WireGuard network plain
HTTP is acceptable; anything crossing the internet needs TLS and an authenticating
front end.

**Where the runtime name is recorded.** In the registry and in the dispatch log
lines (`slog`), not in the webhook audit record: audit is written on the receiving
path, which knows nothing about runtimes, and keeping it that way is what lets the
receiver stay I/O free and transport-only.

### Baseline consistency across runtimes

Once there is more than one runtime host there is more than one clone, and
"which commit does this task start from?" stops being a detail of the local
repository. It is not a failover-only problem: with `spread`, two tasks for the
same project can land on two hosts whose clones were fetched at different times,
so the *normal* case produces two different baselines. The same issue would then
produce patches that apply on one host and not the other, and tests that pass on
one and fail on the other.

**The worktree must not be synced between hosts.** A linked worktree is not a
portable directory: its `.git` is a file containing an absolute path to a
host-local object store, and its index, `HEAD` and untracked files are host-local
state. Copying it either breaks or silently diverges, and it duplicates the state
that git already knows how to share. The only thing worth moving between hosts is
a commit, and the way to move a commit is through the origin.

**So the baseline becomes an input, not something the host decides.** Today
`git worktree add -b flowhub/<key> <path> <default_branch>` resolves the branch on
whichever host runs it, at whatever that host's clone last fetched. Instead:

```go
type Workspace interface {
    Name() string
    // Resolve reports the commit that baseRef points at *as the origin sees it*,
    // not as this host's clone sees it. Asked once per task, by whichever runtime
    // is about to take it.
    Resolve(ctx context.Context, repo RepoRef, baseRef string) (commit string, err error)
    Prepare(ctx context.Context, req Request) (Handle, error)
    Check(ctx context.Context, h Handle) error
    Remove(ctx context.Context, h Handle) error
}

type Request struct {
    TaskKey    string
    Remote     string
    BaseRef    string // "master"
    BaseCommit string // pinned; Prepare must produce exactly this commit
    Base       string
}
```

`Resolve` is `git ls-remote origin <baseRef>` on the runtime side: the origin is
the only shared truth, so the answer is the same no matter which host is asked,
and a stale clone cannot change it. `Prepare` then creates the worktree **at that
commit** — fetching it only when the object is missing locally
(`git cat-file -e <commit>^{commit}`), so a stale clone heals on demand without
network traffic on every task, and a host that cannot obtain the commit fails
that candidate instead of quietly using an older one.

The pinned values are what make the workspace reconstructible, so they are
recorded next to the task:

| `registry.Task` field | Meaning |
| --- | --- |
| `runtime` | the host that owns the workspace, for life |
| `repo` | the repository identity, as attested by that host |
| `worktree` | the provider's opaque path on that host |
| `base_commit` | the commit the task started from, resolved once at creation and **never re-resolved** |

`base_commit` is resolved once, when the task is created, and reused for every
later turn. That is what keeps a long-running task's patches reviewable against a
fixed base even if the origin moves, and it is also the check that makes
"attach to an existing branch" safe: if `flowhub/<task key>` already exists on the
host, `Prepare` verifies it is a descendant of `base_commit` before attaching,
and refuses otherwise. Without that check a branch with the same name from an
unrelated run would be adopted silently.

Four consequences worth stating plainly:

1. **Unpushed local work is invisible to the agent.** The shared baseline is what
   the origin has, not what your working clone has. This is the price of
   deterministic multi-host behaviour, and it is also the fix: to have the agent
   build on your local commits, push them to a branch and point `default_branch`
   (or a per-project `base_ref`) at it.
2. **The guarantee covers the code, not the toolchain.** Two hosts with the same
   `base_commit` can still differ in Go, Node or JDK versions, so a change may
   verify on one and not the other. That drift is not something a commit pin can
   fix: keep the hosts provisioned identically, or use `runtime_policy:
   "first-healthy"` so a project always runs where it was verified. A provider
   could report a toolchain fingerprint in `Check` later; out of scope here.
3. **Tasks stay isolated from each other.** Task B's workspace never contains
   task A's unmerged branch, on one host or across hosts. To build on another
   task's work, merge it into the branch that `default_branch` points at — the next
   task's resolved `base_commit` then includes it — or keep the work in one task.
   Spreading tasks across hosts does not change this; it only makes it visible.
4. **Review and hand-off go through the origin too.** Because the branch lives on
   the host that made it, the reviewer needs a transport (open question 10). If a
   task is ever re-homed by an operator command, the protocol is: push the branch
   from the old host, fetch it on the new host, check out at the same
   `base_commit` — and accept that the session context is still lost.

### The configuration format (v2)

The routing table cannot carry a second source as it stands, for three
mechanical reasons:

1. **The entry names its source in the field name.** `youtrack_key`; a Gitea
   tracker key would have to be spelled `youtrack_key` or the loader could not
   read it.
2. **The key index is global.** `Map.byKey` is keyed by the project key alone
   (`internal/projectmap/projectmap.go`), so YouTrack `TEST` and Gitea `TEST`
   collide and `register` fails with "claimed by both".
3. **The trigger policy is process-global.** `FLOWHUB_TRIGGER`,
   `FLOWHUB_START_STATES`, `FLOWHUB_SKIP_ANALYZE_ON_CREATE` and
   `FLOWHUB_MAX_TURNS` are read once into `config.Config`, so two sources cannot
   disagree about what "start implementing" means.

The file also has to absorb the runtime, because `FLOWHUB_OPENCODE_URL` and
`FLOWHUB_DISPATCH_AGENT` are runtime facts sitting in the same flat environment as
the source facts.

**Locks and key material stay in the environment**; the file holds non-secret
policy and routing only, so it stays safe to copy, diff, back up and review. Both
sides become per-source: `FLOWHUB_YOUTRACK_HOOK_KEY`,
`FLOWHUB_YOUTRACK_TOKEN`, `FLOWHUB_YOUTRACK_ALLOWED_SOURCES`,
`FLOWHUB_GITEA_SECRET`, with today's unsuffixed names kept as aliases for YouTrack.

Revision 2 sketched a per-source `locks` block in this file (`url_key`,
`token_header`, `source_ip`). It was removed for two reasons. First, it splits a
lock's *enablement* from its *secret*, which creates a contradiction the format
then has to resolve by rule: `"url_key": true` with no
`FLOWHUB_YOUTRACK_HOOK_KEY` in the environment can only be "refuse to start",
because the project's invariant is that a silently disabled lock is the worst
outcome — today's `unset` means "disabled, loudly" only because it cannot tell
"I meant to turn this off" from "I forgot to configure it", and an explicit
declaration in a file removes that excuse rather than needing it. Second, a lock
is a *deployment* fact (which address the gateway forwards from, which URL is
published, what the shared secret is), not a repository fact, and this file's
subject is repositories. Keeping both the switch and the value in the environment
leaves exactly one place to be wrong. What the file does keep is the per-source
statement that the source exists at all (`enabled`) and what it means to start
work (`policy`).

```json
{
  "version": 2,

  "_comment": "Event sources, agent runtimes, and the project -> repository table.",

  "sources": {
    "youtrack": {
      "enabled": true,
      "policy": {
        "trigger": "/opencode start",
        "start_states": ["In Progress"],
        "skip_analyze_on_create": false,
        "max_turns": 8,
        "self_markers": ["<!-- flowhub-auto -->", "generated automatically by opencode"]
      },
      "prompt_file": "prompts/youtrack.md",
      "authors": ["yusiwen"]
    }
  },

  "runtimes": {
    "local": {
      "_comment": "Development: FlowHub and opencode are the same machine.",
      "url": "http://127.0.0.1:4096",
      "agent": "devops",
      "workspace": {
        "provider": "localworktree",
        "base": "/Users/yusiwen/git/mine/test-worktrees",
        "clones": { "youtrack:TEST": "/Users/yusiwen/git/mine/test" }
      }
    },
    "builder-a": {
      "_comment": "Target: the agent runtime, and the filesystem it works on.",
      "url": "https://builder-a.lan:4096",
      "auth": { "user": "opencode", "password_env": "FLOWHUB_RUNTIME_BUILDER_A_PASSWORD" },
      "agent": "devops",
      "model": "deepseek/deepseek-v4-flash",
      "deadline": "15m",
      "max_concurrent": 1,
      "workspace": { "provider": "remote", "runner": "https://builder-a.lan:8443", "base": "/srv/flowhub-worktrees" }
    }
  },

  "projects": [
    {
      "source": "youtrack",
      "project": "BEAP_BE",
      "also": ["BEAP"],
      "repo": {
        "_comment": "Identity only. default_branch is resolved through the origin to a commit when a task is created, and that commit is pinned for the task's life.",
        "remote": "git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git",
        "default_branch": "master"
      },
      "runtimes": ["builder-a", "builder-b"],
      "runtime_policy": "spread",
      "agent": "devops",
      "enabled": true
    },
    {
      "source": "youtrack",
      "project": "TEST",
      "repo": { "remote": "", "default_branch": "main" },
      "runtime": "local"
    }
  ]
}
```

`repo` is the *logical identity* of a repository (its remote and the branch a task
starts from). The *physical* facts moved under `runtimes`, because they belong to
whichever host owns that filesystem, and a project now names `runtime` or
`runtimes` instead of carrying paths. `TEST` shows one wrinkle: a local repository
with no remote has no identity to attest to, so `remote` is empty and only a local
workspace provider can serve it.

Mapping from v1, which the live file uses today:

| v1 | v2 |
| --- | --- |
| `projects[].youtrack_key: "TEST"` | `projects[].source: "youtrack"` + `projects[].project: "TEST"` |
| `projects[].also_keys` | `projects[].also` |
| `repo.path` + `worktrees` | `repo` keeps only `remote`/`default_branch` (identity); the paths move under `runtimes.<name>.workspace` (location), which is the host that owns them |
| (no equivalent) | `runtimes` + `projects[].runtime`/`runtimes`, so a task can be addressed to one of several agent hosts |
| `repo.remote`, `agent`, `model`, `authors`, `enabled` | unchanged |
| `FLOWHUB_TRIGGER`, `FLOWHUB_START_STATES`, `FLOWHUB_SKIP_ANALYZE_ON_CREATE`, `FLOWHUB_MAX_TURNS` | `sources.<name>.policy.*`; the environment variables become the outermost default |
| `FLOWHUB_OPENCODE_URL`, `FLOWHUB_DISPATCH_AGENT`, `FLOWHUB_TASK_DEADLINE` | the `runtimes.<name>` block (the environment variables become the default for a runtime named `default`) |
| (missing) | `FLOWHUB_OPENCODE_USER` / `FLOWHUB_OPENCODE_PASSWORD` must be wired: the client supports Basic Auth but `main.go` builds it with a URL only, which is fine for loopback and not for a network link |
| `FLOWHUB_ALLOWED_SOURCES` | stays in the environment, renamed per source (`FLOWHUB_YOUTRACK_ALLOWED_SOURCES`) |
| `FLOWHUB_HOOK_KEY`, `FLOWHUB_TOKEN` | stay in the environment, renamed per source (`FLOWHUB_YOUTRACK_HOOK_KEY`, `FLOWHUB_YOUTRACK_TOKEN`); the current names remain accepted as aliases for YouTrack |
| `FLOWHUB_PROJECTS_FILE` | accepted, with `FLOWHUB_CONFIG_FILE` as the preferred spelling (the file is no longer only a routing table) |
| (absent) | `version`; absent means 1 |

**Precedence, outermost to innermost:** environment default → `sources.<name>` →
`projects[]`. `-print-config` prints the effective value *and* which level
supplied it, which also answers open question 2 below.

**Validation stays strict and fail-closed**, because a config mistake should stop
the process rather than route work to the wrong repository:

* `DisallowUnknownFields` stays. `_`-prefixed keys remain the only comments.
* `version` must be 1 or 2; anything else is refused by name.
* `source` must name a *registered* adapter; the error lists what this binary
  knows, so "I built a plugin but forgot to register it" is one line, not a
  mystery.
* The index becomes `(source, project)`, so the same project key in two sources is
  legal and a duplicate *within* one source is still an error.
* The issue-ID-prefix fallback disappears from the router. Splitting `TEST-17` on
  its last `-` is a YouTrack convention, not a routing rule; the adapter fills
  `event.Subject.Project` from the payload, and a payload that omits the project
  is the adapter's problem (it may use the prefix itself).
* `prompt_file` is resolved relative to the config file's directory, must exist,
  and is refused if it is unreadable. Absent means the adapter's built-in prompt.
* A `sources.<name>` block for a source that is registered but built *out* of this
  binary (no adapter) is refused, not ignored.

**Compatibility.** The binary must accept v1 with a warning rather than refuse to
start, because the receiver is live and an upgrade that stops it silently stops
every event. A file without `version` is translated (v1 could only mean YouTrack)
and reported as `config format: v1 (translated; migrate to version 2)`. The
compatibility branch is a few lines plus a test, and is deleted in the commit that
drops v1 support — after this host's file has been migrated.

### What `dispatch` looks like afterwards

It keeps: the queue, the pause file, the routing lookup, the cost and turn
budgets, the workspace hand-off, the registry update and the audit log. It loses:
`webhook.Parse`, `describe`, `AllowedMCPTools`, the reply-tool name, the session
ruleset and every `opencode.*` type — all of which is now true, in the sense that
matters: `go list -deps ./internal/dispatch/` reaches `internal/agent` and
`internal/workspace` and neither implementation. Its inputs are `(*store.Record,
*event.Event, source.Source, agent.Runtime)` plus the injected workspace provider,
with `main` as the only place that names a product.

### Consequences

* Positive: a new source is one new package plus a line in `main`; a new runtime
  is one adapter; the core stops naming vendors; the security boundary becomes
  explicit (tool policy is data owned by the source, shell policy is code owned
  by the runtime).
* Negative: one more indirection in the hot path (negligible — this path is
  already asynchronous and dominated by the model); the interface must be
  designed against two real sources eventually, not one.
* Accepted cost, recorded deliberately: **the audit schema is not made generic
  yet.** `store.Record` keeps its current YouTrack-derived fields
  (`issue_id_form`, `has_number_in_project`, `comment_ids`, `payload_schema`,
  `token_header`). Splitting it into `envelope` + `source_facts map[string]any`
  is deferred to the commit that adds the second source, so the shape is derived
  from two real cases instead of one. Until then a second source's audit records
  simply leave those fields empty, which is already how a payload without
  `numberInProject` looks today.

## Non-goals

1. No plugin SDK, no public extension API, no out-of-tree plugins. Adding a
   source means editing this repository and rebuilding.
2. No `.so` loading and no out-of-process sources (options C and D).
3. No generic audit schema before a second source exists.
4. No multi-tenant, multi-user or per-team isolation.
5. No configuration language for event parsing. Policy and prompt text may become
   files (step 3); payload decoding stays Go.

## Migration plan

Each step is independently committable and must leave `make fmt-check lint
staticcheck test test-race smoke` green. Behaviour must not change before step 3.

| Step | Content | Verification |
| --- | --- | --- |
| 0 | This ADR | reviewed and accepted |
| 1 | Add `internal/event` and `internal/source`; move `webhook.Parse`, `describe`, `AllowedMCPTools`, the reply check and `rules.Prompt` behind a `youtrack` adapter; `dispatch` consumes only the IR. The config file is **not** touched in this step. **Landed:** `internal/event` (the IR), `internal/source` (`Request`, `Decoded`, `Source`, `ToolPolicy`), `internal/source/youtrack` (payload model, schema report, tool allowlist, prompt, state normalisation); `internal/webhook` and `internal/dispatch` no longer name a vendor, and `dispatch` takes its policy from `Source.Policy()` rather than a second field | Existing tests unchanged and green; `make smoke` unchanged; `dispatch` no longer imports `webhook`. **Measured:** `gofmt -l` clean, `go vet` clean, `staticcheck` clean, `go test ./...` and `go test -race ./...` all 15 packages ok, `make smoke` PASSED; `go list -deps ./internal/dispatch/` has no `internal/webhook` edge. The twelve rule/prompt test functions were moved to `internal/source/youtrack` (no assertion dropped) and four dispatch constructors gained a `Source` argument; a new receiver test pins the raw timestamp spelling the refactor could have re-rendered. **Live:** one real `issueCreated` delivery for TEST-30 was accepted in 0.8 ms, chose `builder-tmp` under `spread`, ran one analysis turn (`replied=true`, 16.4 s, 4 permission requests), and the reply landed on the issue with the sign-off `> This comment was generated automatically by opencode, from the creation of this issue.` — the sentence `Policy.Basis` derives from the neutral event. A second delivery echoing that reply without the marker was audited and **not** dispatched (`reason="our own comment (repeats our previous reply)"`), so loop prevention survived the move |
| 2 | Add `internal/agent` and `internal/workspace`; make `opencode` the runtime and today's `worktree` package the `localworktree` provider, with its local-filesystem checks moved behind it; `dispatch` imports neither vendor; wire `FLOWHUB_OPENCODE_USER`/`PASSWORD`. **Landed:** `internal/agent` (`Runtime`, `Turn`/`Result`/`Phase`/`Downloads`, the model spelling), `internal/agent/opencode` (`git mv` of `internal/opencode` plus `runtime.go`, which owns the session ruleset and the phase policy), `internal/workspace` (`Workspace`, `Entry`/`Request`/`Handle`/`Base`, `Validator`), `internal/workspace/localworktree` (`git mv` of `internal/worktree`, now also the `Validator` that owns the filesystem checks `projectmap` used to run); `dispatch` takes injected runtime and workspace factories; both client builders send the configured Basic Auth | `go list -deps` shows both cuts; the live path is re-verified with one real webhook turn. **Measured:** `go list -deps ./internal/dispatch/` has no `internal/agent/opencode` and no `internal/workspace/localworktree` edge while listing `internal/agent` and `internal/workspace`; the implementations are imported only by `cmd/flowhub` (and by `internal/provision` for the product's own host tooling); `gofmt`/`go vet`/`staticcheck` clean, all 15 packages green under `go test` and `go test -race`, `make smoke` PASSED. Live after the refactor: a real `issueCreated` delivery for TEST-31 was accepted in 1 ms, chose `builder-b` under `spread`, resolved its baseline through the provider (`source=local` — the origin-less scratch repository, said out loud), prepared the worktree through `localworktree`, ran one analysis turn (`replied=true`, 14.3 s, 2 permissions answered by the adapter's own arbiter) and posted the reply with the same sign-off; the registry row carries the provider's attested worktree path and the pinned commit |
| 3 | Config format v2 as specified above: `sources`, `runtime`, `(source, project)` entries, per-level policy precedence, and the v1 translation branch. **Landed** as 3a: `version` with the v1 translation branch, `sources` (enabled, policy, authors), `runtimes` (url, auth with `password_env`, agent, model, deadline, max_concurrent), `projects[].source`/`project`/`also` in a `(source, project)` index, `SourcePolicy` precedence with provenance, `FLOWHUB_CONFIG_FILE`/`FLOWHUB_SOURCE` and the per-source lock aliases, a compile-time source registry. **Deferred:** `prompt_file` is parsed and refused by name (3b, with the placeholder contract), and `repo.path`/`worktrees` stay on the project until the `remote` provider exists (step 6) — moving a location is only meaningful when some *other* host owns it | A v1 file and an equivalent v2 file produce the same routing and the same effective policy; `-print-config` names the level each value came from; a v2 file with `youtrack_key` is refused with the replacement named in the error. **Measured:** `TestV1AndV2RouteAndDecideIdentically` compares the two bodies key by key and value by value; `TestProvenanceNamesTheLevelOfEveryValue` asserts each level; `TestV2RefusesTheVersionOneSpellings` asserts the named replacement; `go test`/`-race` green for all 15 packages, `make smoke` PASSED, and `-print-config` on this host reports the live v1 file as `1 (translated; migrate the file to version 2)` while the committed v2 example loads as `2` with every level printed. **Live:** the same table was run twice against the enrolled runtimes — once as a v1 file, once as an equivalent v2 file — and both drove a real analysis turn to `replied=true` with the same repository, the same worktrees base, the same eligibility set and the same `spread` policy. The v2 run also exposed a real wiring bug the unit tests had missed: a `runtimes.<name>` block that carries policy but **no** `url` — which is exactly how an enrolled host gets its policy — was dropped before it reached the dispatcher, so the file's `deadline: 20m` never applied and the process default was used. The block is now passed through either way (a declared runtime is added as a candidate only when it has an address of its own), and `TestDeclaredPolicyAppliesToAnEnrolledRuntime` is the regression test; the re-run logs `deadline=20m0s` and `replied=true` |
| 4 | Gitea adapter as the acceptance test for the source seam (HMAC-SHA256, issue and PR text). **In progress.** Landed first, because the adapter's task key depends on it: the workspace provider now owns the key → directory/branch mapping, so a key like `owner/repo#42` is mapped (`owner-repo-42-<digest>`) instead of refused, and a key that is already a safe segment is used verbatim. **Prerequisites the ADR did not know it had, measured 2026-09-26:** (1) the native header is **`X-Gitea-Signature`** (lowercase hex HMAC-SHA256 of the raw body, no prefix); `X-Hub-Signature-256` carries the same digest with a `sha256=` prefix, and `X-Hub-Signature` is **SHA-1** and must be refused; with no secret configured Gitea still sends the signature headers **with empty values**, so an empty signature is a rejection whenever FlowHub holds a secret, and an unset `FLOWHUB_GITEA_SECRET` has to be a loud warning like every other disabled lock. (2) The reply needs a Gitea MCP: this host has none (opencode registers `ccc`, `youtrack`, `sonarqube`) and `tea` is not installed, so an agent turn on a Gitea event currently has no way to post a comment and `replied` could never become true. The allowlist must name the MCP's real tool names, which only exist once it is installed — guessing them would produce a reply path that silently never fires. (3) A real webhook delivery needs a repository webhook on `git.yusiwen.cn` (up, but `403` to anonymous callers) plus its secret; that is an operator action on the live instance, not something this process may do for itself. **Deliberately deferred until those three exist**, because an adapter whose reply path cannot be exercised would be committed on an unverifiable claim | A real Gitea webhook drives one analysis turn; the core packages show no diff beyond registration. **Ready to verify once (2) and (3) are done:** the delivery is signed with the webhook's secret, and the turn's permission log is what confirms the MCP's real tool names before they are written into `ToolPolicy` |
| 5 | Addressed runtimes **and a pinned baseline**: `runtimes.<name>`, `projects[].runtime`/`runtimes` + `runtime_policy`, `Workspace.Resolve`, `Request.BaseCommit`, `registry.Task.{Runtime,BaseCommit}`, one queue and worker per runtime, per-runtime startup probing, set validation. **Landed: the addressing and its policy, the deterministic ranking and its log line, set validation, the per-runtime counters and queues, the pinned baseline (resolved through the origin, fetched on demand, descendant-checked, recorded), and one intake loop plus one queue and worker per runtime. `max_concurrent` stays 1 pending per-task locking** | Two runtimes configured: with `spread`, two consecutive tasks land on different hosts and the log says why; **both report the same `base_commit`** even when one clone is deliberately stale; with `first-healthy`, both land on the first; stopping one host makes the next task use the other and leaves bound tasks refused with that reason; an existing `flowhub/<key>` branch that is not a descendant of `base_commit` is refused |
| 6 | A `remote` workspace provider, once the deployment actually splits: the helper on the runtime host, the repository attestation handshake, and branch delivery | FlowHub runs on a host with **no** copy of the repository and still completes a full task end to end on the addressed remote runtime |

Step 4 is the point of the whole exercise: if adding Gitea requires touching
`dispatch`, the seam is wrong and should be revised rather than worked around.

## Open questions for review

1. **`Raw` in the IR, or the record?** Should `event.Event` carry the raw body
   (so the audit keeps writing exactly one record shape), or should the receiver
   keep owning the raw bytes and hand the parsed event plus a record id to the
   dispatcher? Recommendation: keep `Raw` in the event for now; revisit together
   with the `source_facts` split.
2. **One config file or one per source?** The v2 sketch puts every source, the
   runtime and the projects in one file. The alternative is a `sources/<name>.json`
   plus a top-level file that includes them. Recommendation: one file, because the
   projects table is shared and a project entry has to name its source anyway;
   revisit if a source's block grows past a screen.
3. **Are attachments first-class?** The prompt currently tells the agent to
   download to a fixed prefix, and the arbiter allows exactly that. Gitea carries
   attachments too. Recommendation: keep them as `Subject.Attachments` and let
   `ToolPolicy.Download` decide where they may land.
4. **Task key collisions.** `registry` keys tasks by `Subject.Key`. Two sources
   could produce the same key (`TEST-17` in YouTrack and in a Gitea tracker).
   Recommendation: qualify the registry key with the source (`youtrack:TEST-17`)
   from the start, and migrate existing rows on first read. Confirm, because it
   changes the registry format.
5. **One runtime per process, or per task?** The v2 sketch puts `runtime` at the
   top level, with `projects[].agent`/`model` as the per-project override.
   Recommendation: one runtime per process until a real need for two appears.
6. **Should `prompt_file` be per project as well as per source?** A repository may
   want its own instructions ("this repo is Java, run `mvn -q verify`"). The
   project entry already carries `agent`/`model`, so a per-project prompt is
   consistent. Recommendation: not yet — the prompt already receives the
   repository path and tells the agent to find the project's own tooling.
8. ~~**Runtime selection: explicit names, or labels?**~~ **Decided in review:
   names.** Labels stay out until a third runtime makes "which machine can run
   this" a question that names cannot answer, and if they arrive they may only
   narrow a project's eligible set.
9. **What should a bound-but-unreachable runtime do to the task?** The
   recommendation above is "refuse and say so". The alternative is an operator
   command to re-home a task explicitly (`flowhub task move TEST-17 builder-b`),
   which is honest because a human decides that losing the session context is
   acceptable. Not needed until a host actually dies.
10. **How does a finished branch reach the reviewer?** Co-located it is already
   there; split it is on the runtime host. Options: relax "never push" to "never
   push outside `flowhub/<task key>`", have the reviewer fetch from the runtime
   host, or have the provider export a patch. Recommendation: first, because it
   keeps the review in the tool the team already uses, with the branch name
   enforced by the arbiter's deny list rather than promised by the prompt.
9. **Where does the remote helper run?** A tiny sidecar next to `opencode serve`,
   or inside the same container image / systemd unit. Recommendation: sidecar on
   the same host, so "the workspace exists" and "the agent can reach it" cannot
   drift apart.
11. **Per-source environment variable names.** The proposal renames the lock and
   secret variables per source and keeps today's names as aliases. Confirm that the
   alias layer is wanted at all: dropping it is simpler and the migration is one
   shell profile, but it also breaks any script that exports the old names.
