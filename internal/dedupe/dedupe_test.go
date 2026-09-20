package dedupe

import (
	"testing"
	"time"
)

func TestCheckAndAddRemembersUntilTTL(t *testing.T) {
	cache := New(time.Minute)
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return base }

	if cache.CheckAndAdd("key") {
		t.Fatal("first delivery must not be a duplicate")
	}
	if !cache.CheckAndAdd("key") {
		t.Fatal("second delivery must be a duplicate")
	}
	if cache.CheckAndAdd("other") {
		t.Fatal("unrelated key must not be a duplicate")
	}

	cache.now = func() time.Time { return base.Add(2 * time.Minute) }
	if cache.CheckAndAdd("key") {
		t.Fatal("key must expire after the TTL")
	}
}

func TestSweepRemovesOnlyExpiredKeys(t *testing.T) {
	cache := New(time.Hour)
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return base }
	cache.CheckAndAdd("old")

	cache.now = func() time.Time { return base.Add(30 * time.Minute) }
	cache.CheckAndAdd("new")

	cache.now = func() time.Time { return base.Add(2 * time.Hour) }
	if removed := cache.Sweep(); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if cache.Len() != 0 {
		t.Fatalf("len = %d, want 0", cache.Len())
	}
}

func TestZeroTTLDisablesDeduplication(t *testing.T) {
	cache := New(0)
	if cache.CheckAndAdd("key") {
		t.Fatal("deduplication must be disabled")
	}
	if cache.CheckAndAdd("key") {
		t.Fatal("deduplication must be disabled")
	}
	if cache.Len() != 0 {
		t.Fatalf("len = %d, want 0", cache.Len())
	}
}
