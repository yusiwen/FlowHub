package runner

import (
	"fmt"
	"sync"

	"github.com/yusiwen/flowhub/internal/workspace"
)

// Workspaces caches one workspace provider per checkout base and serializes
// preparation per repository.
//
// The cache matters because a provider may hold state (the local provider opens the
// clone), and the per-repository lock matters because `git worktree add` is not safe
// to run twice at once in one clone. Both orchestrators share one instance, so a
// flow and a dispatch cannot prepare the same repository concurrently.
type Workspaces struct {
	factory func(base string) (workspace.Workspace, error)

	mu        sync.Mutex
	providers map[string]workspace.Workspace
	locks     map[string]*sync.Mutex
}

// NewWorkspaces builds the cache from the provider factory `main` injects.
func NewWorkspaces(factory func(base string) (workspace.Workspace, error)) *Workspaces {
	return &Workspaces{
		factory:   factory,
		providers: map[string]workspace.Workspace{},
		locks:     map[string]*sync.Mutex{},
	}
}

// For returns the provider for base, creating it on first use.
func (w *Workspaces) For(base string) (workspace.Workspace, error) {
	if w == nil || w.factory == nil {
		return nil, fmt.Errorf("runner: no workspace provider factory")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if provider, ok := w.providers[base]; ok {
		return provider, nil
	}
	provider, err := w.factory(base)
	if err != nil {
		return nil, err
	}
	w.providers[base] = provider
	return provider, nil
}

// Lock returns the mutex that serializes preparation for one repository.
func (w *Workspaces) Lock(repo string) *sync.Mutex {
	w.mu.Lock()
	defer w.mu.Unlock()
	if lock, ok := w.locks[repo]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	w.locks[repo] = lock
	return lock
}
