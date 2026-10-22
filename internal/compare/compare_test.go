package compare_test

import (
	"testing"

	"github.com/yusiwen/flowhub/internal/compare"
)

func TestAMatchingPairIsOneComparison(t *testing.T) {
	store, err := compare.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	store.ObserveDispatch(compare.Decision{EventID: "evt_1", Action: "analyze", Reason: "issue created", Routable: true})
	store.ObserveFlow(compare.Decision{EventID: "evt_1", Action: "analyze", Reason: "issue created", Routable: true})
	comparison, ok := store.Get("evt_1")
	if !ok || !comparison.Match || len(comparison.Differences) != 0 {
		t.Fatalf("comparison = %+v, ok = %v", comparison, ok)
	}
	compared, mismatches := store.Counts()
	if compared != 1 || mismatches != 0 {
		t.Fatalf("counts = %d/%d", compared, mismatches)
	}
	if store.Pending() != 0 {
		t.Fatalf("pending = %d", store.Pending())
	}
}

func TestAnActionDifferenceIsAMismatch(t *testing.T) {
	store, _ := compare.Open(t.TempDir())
	defer func() { _ = store.Close() }()
	store.ObserveDispatch(compare.Decision{EventID: "evt_2", Action: "execute", Reason: "comment", Routable: true})
	store.ObserveFlow(compare.Decision{EventID: "evt_2", Action: "ignore", Reason: "own comment", Routable: true})
	comparison, _ := store.Get("evt_2")
	if comparison.Match || len(comparison.Differences) == 0 {
		t.Fatalf("comparison = %+v", comparison)
	}
	if _, mismatches := store.Counts(); mismatches != 1 {
		t.Fatalf("mismatches = %d", mismatches)
	}
}

func TestBothSidesUnroutableMatch(t *testing.T) {
	store, _ := compare.Open(t.TempDir())
	defer func() { _ = store.Close() }()
	store.ObserveDispatch(compare.Decision{EventID: "evt_3", Action: "ignore", Reason: "no routing entry", Routable: false})
	store.ObserveFlow(compare.Decision{EventID: "evt_3", Action: "ignore", Reason: "routing", Routable: false})
	comparison, _ := store.Get("evt_3")
	if !comparison.Match {
		t.Fatalf("comparison = %+v", comparison)
	}
}

func TestComparisonsSurviveAReopen(t *testing.T) {
	dir := t.TempDir()
	store, _ := compare.Open(dir)
	store.ObserveDispatch(compare.Decision{EventID: "evt_4", Action: "analyze", Reason: "r", Routable: true})
	store.ObserveFlow(compare.Decision{EventID: "evt_4", Action: "plan", Reason: "r", Routable: true})
	_ = store.Close()

	reopened, err := compare.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	comparison, ok := reopened.Get("evt_4")
	if !ok || comparison.Match {
		t.Fatalf("comparison = %+v, ok = %v", comparison, ok)
	}
	if compared, mismatches := reopened.Counts(); compared != 1 || mismatches != 1 {
		t.Fatalf("counts = %d/%d", compared, mismatches)
	}
}

func TestOneSidedEventStaysPending(t *testing.T) {
	store, _ := compare.Open(t.TempDir())
	defer func() { _ = store.Close() }()
	store.ObserveFlow(compare.Decision{EventID: "evt_5", Action: "analyze", Routable: true})
	if store.Pending() != 1 {
		t.Fatalf("pending = %d", store.Pending())
	}
	if _, ok := store.Get("evt_5"); ok {
		t.Fatal("a one-sided event must not be a comparison")
	}
}
