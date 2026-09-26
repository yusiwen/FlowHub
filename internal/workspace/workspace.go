// Package workspace is the seam that turns "this task needs a checkout" into a
// directory on whichever filesystem the work actually happens on.
//
// The distinction matters because FlowHub is a control plane: the routing table
// names a repository, but the repository, the disk the task is checked out on and
// the runtime that works in it may all live on another host. Four facts that used
// to be read straight off the control plane's local filesystem are therefore the
// provider's to answer (ADR 0001):
//
//   - the repository exists, is a git work tree, and its origin is the one the
//     routing entry declares;
//   - the directory that holds task checkouts exists and sits outside the
//     repository;
//   - the checkout is produced by whatever means the provider has;
//   - the task's directory is recorded as an opaque string, never as a local path
//     this process can assume it can reach.
//
// Everything else was already transport-safe: the runtime takes its workspace as
// an opaque request parameter, the permission loop judges commands that run on the
// far side, and the attachment prefix is relative to the workspace.
package workspace

import "context"

// Entry is the part of a routing entry a provider can verify before any file is
// created. It is plain data on purpose: it is built by the configuration layer, so
// this package never learns a configuration format.
type Entry struct {
	// Label names the entry in the operator's words, so a problem reads as
	// "TEST: ..." rather than as a bare path.
	Label string
	// Repo is where the code comes from: the local path of the shared clone for a
	// provider that runs on the same filesystem, the remote URL for one that clones
	// for itself.
	Repo string
	// Remote is the repository identity the routing entry declares, used for the
	// provider's attestation.
	Remote string
	// DefaultBranch is the branch a task branches from. It must already exist: a
	// provider that guessed one would base the task on the wrong code.
	DefaultBranch string
	// Base is the directory that holds one checkout per task, already resolved
	// against any process-wide fallback.
	Base string
}

// Request describes the workspace one task needs.
type Request struct {
	// TaskKey is stable and human readable ("TEST-17"). It becomes a directory name
	// and a branch name, so a provider must keep it boring.
	TaskKey string
	// Repo, Remote and DefaultBranch mean what they mean in Entry.
	Repo          string
	Remote        string
	DefaultBranch string
	// BaseRef is the ref to start from, normally the entry's default branch.
	BaseRef string
	// BaseCommit pins the commit the task starts from. When set, the provider
	// produces exactly this commit and refuses to attach to an existing branch that
	// is not its descendant. Empty means "start from BaseRef as this host has it",
	// which is only correct for a single host.
	BaseCommit string
}

// Handle is what FlowHub records and hands to the runtime.
//
// Path is opaque: FlowHub never stats, joins or globs it, because it names a
// directory on somebody else's machine once the provider is remote.
type Handle struct {
	// Provider is the name of the provider that produced this, so a task prepared
	// by one provider is never silently continued by another.
	Provider string
	// Path is the workspace as the runtime spells it.
	Path string
	// Repo is the provider's own attestation of which repository it prepared. It is
	// compared against the routing entry: an unattested workspace cannot be trusted
	// just because a path matched once.
	Repo string
	// BaseCommit is the commit the workspace was created from.
	BaseCommit string
	// Reused reports that the workspace already existed and was not created now.
	Reused bool
}

// Base is a resolved baseline.
type Base struct {
	// Commit is the full commit id the base ref points at.
	Commit string
	// Ref is the fully qualified ref it was resolved from, e.g. refs/heads/main.
	Ref string
	// Source says where the answer came from: "origin" when the shared origin
	// answered, and "local" when there was no origin to ask. Only the first is the
	// same answer on every host, so a caller has to be able to tell them apart.
	Source string
}

// Workspace produces and owns one task's working directory.
type Workspace interface {
	// Name is the provider's name, recorded with the task.
	Name() string
	// Resolve reports the commit BaseRef points at as the shared origin sees it, so
	// that every host pins the same baseline. It is part of this seam rather than the
	// dispatcher's job because only the provider knows where the code comes from —
	// and with a local clone the origin may not be reachable from here at all.
	Resolve(ctx context.Context, req Request) (Base, error)
	// Prepare creates (or reuses) the task's workspace. It is idempotent: calling it
	// twice for one task returns the same directory, so a retried delivery can never
	// create a second checkout or a second branch.
	Prepare(ctx context.Context, req Request) (Handle, error)
	// Check reports drift: the workspace is gone, or it no longer belongs to the
	// repository the task was created against.
	Check(ctx context.Context, handle Handle) error
	// Remove drops the workspace. The branch is kept: it holds the agent's work.
	Remove(ctx context.Context, handle Handle) error
}

// Validator is the capability a provider implements when it can check a routing
// entry before anything is created.
//
// It is a separate interface, not part of Workspace, because validating an entry
// must not require a prepared instance: startup refuses a bad entry before any
// directory exists. A provider whose checks happen on another host answers with the
// host's own attestation instead of a local filesystem look.
type Validator interface {
	// Validate reports why this provider cannot serve an entry. An empty result
	// means it can.
	Validate(entry Entry) []string
}

// ValidateEntries runs a provider's own checks over the entries it will be asked to
// serve. A nil validator reports nothing, which is what a caller that has no
// provider yet (or a disabled routing table) wants.
//
// Problems are prefixed with the entry's label so that one message identifies both
// the problem and the project it belongs to.
func ValidateEntries(validator Validator, entries []Entry) []string {
	if validator == nil {
		return nil
	}
	var problems []string
	for _, entry := range entries {
		for _, problem := range validator.Validate(entry) {
			if entry.Label == "" {
				problems = append(problems, problem)
				continue
			}
			problems = append(problems, entry.Label+": "+problem)
		}
	}
	return problems
}
