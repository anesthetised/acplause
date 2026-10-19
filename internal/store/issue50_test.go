package store

import (
	"testing"
	"time"
)

func TestIssue50AcquireReportsPreviousOwnership(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir, "a"), open(t, dir, "b")
	if err := a.Create(ctx, session("s", "idle")); err != nil {
		t.Fatal(err)
	}
	acquire := func(s *Store, owned bool) *Loaded {
		t.Helper()
		loaded, err := s.Acquire(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		if loaded.AlreadyOwned != owned {
			t.Fatalf("AlreadyOwned = %v, want %v", loaded.AlreadyOwned, owned)
		}
		return loaded
	}
	acquire(a, true)
	acquire(b, false)
	if err := b.Save(ctx, session("s", "idle"), rows(`"B retained history"`), nil); err != nil {
		t.Fatal(err)
	}
	loaded := acquire(a, false)
	if len(loaded.History) != 1 || string(loaded.History[0]) != `"B retained history"` {
		t.Fatalf("ownership evidence did not accompany the latest snapshot: %q", loaded.History)
	}
	acquire(a, true)
	if err := a.Release(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	acquire(a, false)
	// Expiry alone does not imply a different owner intervened.
	a.now = func() time.Time { return time.Now().Add(2 * leaseTTL) }
	acquire(a, true)
}
