package runner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yusiwen/flowhub/internal/runner"
	"github.com/yusiwen/flowhub/internal/workspace"
)

type fakeWorkspace struct {
	name string
}

func (f *fakeWorkspace) Name() string { return f.name }

func (f *fakeWorkspace) Resolve(context.Context, workspace.Request) (workspace.Base, error) {
	return workspace.Base{}, nil
}

func (f *fakeWorkspace) Prepare(context.Context, workspace.Request) (workspace.Handle, error) {
	return workspace.Handle{}, nil
}

func (f *fakeWorkspace) Check(context.Context, workspace.Handle) error  { return nil }
func (f *fakeWorkspace) Remove(context.Context, workspace.Handle) error { return nil }

func TestWorkspacesCachesByBase(t *testing.T) {
	calls := 0
	cache := runner.NewWorkspaces(func(base string) (workspace.Workspace, error) {
		calls++
		return &fakeWorkspace{name: base}, nil
	})
	first, err := cache.For("/repos/a")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := cache.For("/repos/a")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatal("the same base returned two providers")
	}
	if calls != 1 {
		t.Fatalf("factory calls = %d, want 1", calls)
	}
	if _, err := cache.For("/repos/b"); err != nil {
		t.Fatalf("other base: %v", err)
	}
	if calls != 2 {
		t.Fatalf("factory calls = %d, want 2", calls)
	}
}

func TestWorkspacesSerializesPerRepository(t *testing.T) {
	cache := runner.NewWorkspaces(func(string) (workspace.Workspace, error) {
		return &fakeWorkspace{}, nil
	})
	first := cache.Lock("/repos/a")
	second := cache.Lock("/repos/a")
	other := cache.Lock("/repos/b")
	if first != second {
		t.Fatal("the same repository returned two locks")
	}
	if first == other {
		t.Fatal("different repositories shared a lock")
	}
}

func TestWorkspacesReportsFactoryErrors(t *testing.T) {
	cache := runner.NewWorkspaces(func(string) (workspace.Workspace, error) {
		return nil, errors.New("cannot open")
	})
	if _, err := cache.For("/repos/a"); err == nil {
		t.Fatal("expected the factory error")
	}
	empty := runner.NewWorkspaces(nil)
	if _, err := empty.For("/repos/a"); err == nil {
		t.Fatal("expected a missing-factory error")
	}
}
