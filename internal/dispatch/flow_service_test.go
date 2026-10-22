package dispatch

import (
	"context"
	"testing"

	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/source"
	tasksvc "github.com/yusiwen/flowhub/internal/tasks"
)

func TestTheTaskServiceRunsTheSameTurnAsDispatch(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, registry := newScheduledDispatcher(t, fake, server, "")
	rec := delivery("TEST-90", "issueCreated", issueCreatedBody("TEST-90"))
	decoded, err := dispatcher.opts.Source.Parse(&source.Request{
		Method: "POST", Path: "/hooks/youtrack", Body: []byte(rec.RawBody),
	})
	if err != nil || decoded.Event == nil {
		t.Fatalf("decode: %v", err)
	}
	match, ok := dispatcher.opts.Projects.Match("youtrack", rec.ProjectKey)
	if !ok {
		t.Fatal("project is not routed")
	}
	ctx := context.Background()
	host, err := dispatcher.TaskSelector().Select(ctx, rec, match.Entry)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	service := dispatcher.TaskService()
	task, err := service.Ensure(ctx, tasksvc.EnsureRequest{
		Record: rec, Delivery: *decoded.Event, Entry: match.Entry, Host: host,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if task.Worktree == "" || task.Runtime == "" {
		t.Fatalf("task = %+v", task)
	}
	result, err := service.Turn(ctx, tasksvc.TurnRequest{
		Record: rec, Delivery: *decoded.Event, Task: task, Entry: match.Entry, Host: host,
		Action: rules.ActionAnalyze, RequireReply: true, Basis: "test",
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if !result.Finished || !result.Replied || result.SessionID == "" {
		t.Fatalf("result = %+v", result)
	}
	updated, ok := registry.Get("youtrack", rec.IssueID)
	if !ok || updated.Turns != 1 || updated.SessionID == "" {
		t.Fatalf("registry = %+v", updated)
	}
	if result.Task.Turns == 0 {
		t.Fatalf("result task = %+v", result.Task)
	}
}
