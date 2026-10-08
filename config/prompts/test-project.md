# Project instructions (example)

This file is what `projects[].prompt_file` points at in
`config/config.example.json`. Its content is **appended** under
`## Operator instructions` → `### This project`, after the source's own text, and
only for turns whose routing entry names it.

Replace it with instructions for the repository that entry points at, or delete the
`prompt_file` line. A missing, empty, directory or larger-than-32-KiB file refuses
the start.

## What belongs here

Facts an agent cannot discover quickly by reading the repository:

- the build, test and lint commands that actually work, and how long they take;
- generated files it must not edit by hand;
- a module layout that is not obvious from the tree;
- the parts of the codebase that need extra care, and why.

## Example

- Build with `make build`; the full test suite is `make test` (about 90 s). Run the
  narrowest package first: `go test ./internal/<pkg>/`.
- `internal/generated/` is produced by `make generate` — never edit it by hand.
- The public API in `internal/api` is frozen for this release; a change there needs a
  human decision, so raise it as a question instead of implementing it.
