# Source instructions (example)

This file is what `sources.youtrack.prompt_file` points at in
`config/config.example.json`. Its content is **appended** to the adapter's built-in
prompt, under `## Operator instructions` → `### This source`, and it applies to
every turn this source produces, for every project.

Replace it with your own text, or delete the `prompt_file` line from your
configuration file to keep only the built-in prompt. A missing, empty, directory or
larger-than-32-KiB file refuses the start.

## What belongs here

Standing guidance for the tracker as a whole: the vocabulary this team uses, which
workflow states mean what, how a plan is approved, which repositories are connected,
who to ask when the issue is ambiguous.

## What does not belong here

Anything that would disable the contract. The untrusted-input warning, the reply
tool, the sign-off naming the trigger, and the `<!-- flowhub-auto -->` marker stay in
`internal/source/youtrack/prompt.go`, and the marker is what stops FlowHub from
answering its own reply.

## Example

- Issues here are written in Chinese; answer in the language the issue uses and keep
  the reply short enough to read on one screen.
- `In Progress` means "start implementing this"; a comment `/opencode start` means the
  same thing.
- If an issue names a service rather than a repository, ask which repository before
  planning.
