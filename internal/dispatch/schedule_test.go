package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/store"
)

// scheduled builds a delivery for the scheduler unit tests: the scheduler only needs
// the task identity out of the record.
func scheduled(issue string) routed {
	return routed{record: &store.Record{IssueID: issue}, binding: runtimeBinding{Name: "builder-a"}}
}

func testTask(key string) taskID { return taskID{source: "youtrack", key: key} }

// TestSchedulerRunsOneTurnPerTask is the invariant the breadth rests on: a task is
// handed to a worker once, and its next delivery waits until that turn finishes.
func TestSchedulerRunsOneTurnPerTask(t *testing.T) {
	scheduler := newRuntimeScheduler("builder-a", 4, 32)
	id := testTask("TEST-1")
	for i := 0; i < 3; i++ {
		if !scheduler.add(id, scheduled("TEST-1")) {
			t.Fatalf("the scheduler refused delivery %d well below its limit", i+1)
		}
	}

	first, ok := scheduler.take()
	if !ok || first.record.IssueID != "TEST-1" {
		t.Fatalf("take = %+v/%v, want the first delivery", first, ok)
	}
	if _, ok := scheduler.take(); ok {
		t.Fatal("the same task was handed to a second worker while its turn was running")
	}
	// A running task with deliveries waiting is one task, not two: /healthz counted it
	// as two until a live run showed it (2026-09-27).
	if got := scheduler.snapshot()["tasks"].(int); got != 1 {
		t.Fatalf("tasks = %d, want 1: one task has a turn running and deliveries waiting", got)
	}
	if got := scheduler.snapshot()["running"].(int); got != 1 {
		t.Fatalf("running = %d, want 1", got)
	}
	if got := scheduler.snapshot()["queued"].(int); got != 2 {
		t.Fatalf("queued = %d, want the two deliveries still waiting", got)
	}
	if drained := scheduler.finish(id); drained {
		t.Fatal("the task was reported drained with two deliveries still waiting")
	}
	if _, ok := scheduler.take(); !ok {
		t.Fatal("the task was not handed out again after its turn finished")
	}
	scheduler.finish(id)
	if _, ok := scheduler.take(); !ok {
		t.Fatal("the third delivery did not run")
	}
	if drained := scheduler.finish(id); !drained {
		t.Fatal("the task was not reported drained after its last delivery")
	}
	if _, ok := scheduler.take(); ok {
		t.Fatal("an empty scheduler handed out work")
	}
	if !scheduler.idle(id) {
		t.Fatal("a drained task is not idle")
	}
}

// TestSchedulerRunsDifferentTasksInParallel: the breadth counts tasks, so two tasks
// are in flight at once while a third waits for a worker.
func TestSchedulerRunsDifferentTasksInParallel(t *testing.T) {
	scheduler := newRuntimeScheduler("builder-a", 2, 32)
	for _, key := range []string{"TEST-1", "TEST-2", "TEST-3"} {
		scheduler.add(testTask(key), scheduled(key))
	}
	if got := scheduler.snapshot()["queued"].(int); got != 3 {
		t.Fatalf("queued = %d, want 3", got)
	}
	first, _ := scheduler.take()
	second, _ := scheduler.take()
	if first.record.IssueID == second.record.IssueID {
		t.Fatalf("both workers got %s", first.record.IssueID)
	}
	if got := scheduler.runningCount(); got != 2 {
		t.Fatalf("running = %d, want 2", got)
	}
	// The third distinct task is ready, but a two-task host hands out two at a time.
	// It stays queued until one of the first two finishes, which is what `busy` says.
	if got := scheduler.busy(); got != 3 {
		t.Fatalf("busy = %d, want the two running plus the one waiting", got)
	}
	scheduler.finish(testTask(first.record.IssueID))
	third, ok := scheduler.take()
	if !ok {
		t.Fatal("a worker was not given the waiting task after one finished")
	}
	if third.record.IssueID != "TEST-3" {
		t.Fatalf("third take = %s, want TEST-3, the only task still ready", third.record.IssueID)
	}
}

// TestSchedulerKeepsArrivalOrderPerTask: a burst for one task is answered in the order
// it arrived, so a later comment is judged against the state the earlier turn left.
func TestSchedulerKeepsArrivalOrderPerTask(t *testing.T) {
	scheduler := newRuntimeScheduler("builder-a", 1, 32)
	id := testTask("TEST-1")
	for _, body := range []string{"one", "two", "three"} {
		item := scheduled("TEST-1")
		item.record.RawBody = body
		scheduler.add(id, item)
	}
	var got []string
	for i := 0; i < 3; i++ {
		item, ok := scheduler.take()
		if !ok {
			t.Fatalf("delivery %d was not handed out", i+1)
		}
		got = append(got, item.record.RawBody)
		scheduler.finish(id)
	}
	order := got[0] + "," + got[1] + "," + got[2]
	if order != "one,two,three" {
		t.Fatalf("order = %s, want one,two,three", order)
	}
}

// TestSchedulerRefusesWorkAboveItsLimit: the runtime's queue bound is what keeps a
// flood from growing in memory, and a refusal is the caller's signal to count a drop.
func TestSchedulerRefusesWorkAboveItsLimit(t *testing.T) {
	scheduler := newRuntimeScheduler("builder-a", 1, 2)
	if !scheduler.add(testTask("TEST-1"), scheduled("TEST-1")) {
		t.Fatal("the first delivery was refused")
	}
	if !scheduler.add(testTask("TEST-2"), scheduled("TEST-2")) {
		t.Fatal("the second delivery was refused")
	}
	if scheduler.add(testTask("TEST-3"), scheduled("TEST-3")) {
		t.Fatal("a delivery above the limit was accepted")
	}
	// Handing a delivery to a worker frees its place in the bound, exactly as taking
	// it out of the channel this replaced did: the bound is on what is waiting, while
	// `busy` keeps counting the turn until it finishes.
	scheduler.take()
	if !scheduler.add(testTask("TEST-3"), scheduled("TEST-3")) {
		t.Fatal("the bound did not free the place of the delivery handed to a worker")
	}
	if scheduler.add(testTask("TEST-4"), scheduled("TEST-4")) {
		t.Fatal("a delivery above the limit was accepted after the queue was refilled")
	}
}

// TestSchedulerWakesAWaitingWorker: the token path. A worker that found nothing must
// be woken by the next delivery rather than waiting forever.
func TestSchedulerWakesAWaitingWorker(t *testing.T) {
	scheduler := newRuntimeScheduler("builder-a", 1, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	item := make(chan routed, 1)
	go func() {
		if got, ok := scheduler.next(ctx); ok {
			item <- got
		}
	}()

	// Let the worker reach its wait before the delivery arrives: the wake is what is
	// under test, not the fast path.
	time.Sleep(50 * time.Millisecond)
	scheduler.add(testTask("TEST-1"), scheduled("TEST-1"))
	select {
	case got := <-item:
		if got.record.IssueID != "TEST-1" {
			t.Fatalf("worker woke with %s", got.record.IssueID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker was never woken by the new delivery")
	}
}
