package runner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/runner"
)

type acquired struct {
	release func()
	err     error
}

func TestCapacityBlocksAtItsLimit(t *testing.T) {
	capacity := runner.NewCapacity()
	capacity.SetLimit("builder", 1)
	first, err := capacity.Acquire(context.Background(), "builder")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if got := capacity.InFlight("builder"); got != 1 {
		t.Fatalf("in flight = %d, want 1", got)
	}

	second := make(chan acquired, 1)
	go func() {
		release, err := capacity.Acquire(context.Background(), "builder")
		second <- acquired{release: release, err: err}
	}()
	select {
	case result := <-second:
		t.Fatalf("second acquire did not block: %+v", result)
	case <-time.After(80 * time.Millisecond):
	}

	first()
	select {
	case result := <-second:
		if result.err != nil {
			t.Fatalf("second acquire: %v", result.err)
		}
		result.release()
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire stayed blocked after release")
	}
	if got := capacity.InFlight("builder"); got != 0 {
		t.Fatalf("in flight = %d, want 0", got)
	}
}

func TestCapacityReleaseIsIdempotent(t *testing.T) {
	capacity := runner.NewCapacity()
	capacity.SetLimit("builder", 1)
	release, err := capacity.Acquire(context.Background(), "builder")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release()
	if got := capacity.InFlight("builder"); got != 0 {
		t.Fatalf("in flight = %d, want 0", got)
	}
}

func TestCapacityUnlimitedStillCounts(t *testing.T) {
	capacity := runner.NewCapacity()
	first, err := capacity.Acquire(context.Background(), "builder")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := capacity.Acquire(context.Background(), "builder")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := capacity.InFlight("builder"); got != 2 {
		t.Fatalf("in flight = %d, want 2", got)
	}
	first()
	second()
	snapshot := capacity.Snapshot()
	if count, ok := snapshot["builder"]; ok && count != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestCapacityWaitHonoursTheContext(t *testing.T) {
	capacity := runner.NewCapacity()
	capacity.SetLimit("builder", 1)
	first, err := capacity.Acquire(context.Background(), "builder")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := capacity.Acquire(ctx, "builder")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not honour the cancel")
	}
	first()
}

func TestCapacityRaisingTheLimitWakesWaiters(t *testing.T) {
	capacity := runner.NewCapacity()
	capacity.SetLimit("builder", 1)
	first, err := capacity.Acquire(context.Background(), "builder")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second := make(chan acquired, 1)
	go func() {
		release, err := capacity.Acquire(context.Background(), "builder")
		second <- acquired{release: release, err: err}
	}()
	time.Sleep(20 * time.Millisecond)
	capacity.SetLimit("builder", 2)
	select {
	case result := <-second:
		if result.err != nil {
			t.Fatalf("second: %v", result.err)
		}
		result.release()
	case <-time.After(2 * time.Second):
		t.Fatal("raising the limit did not wake the waiter")
	}
	first()
}
