package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var ctx = context.Background()

func open(t *testing.T, dir, owner string) *Store {
	t.Helper()
	s, err := Open(dir, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func session(id, state string) Session {
	return Session{ID: id, Cwd: "/w", Dirs: []string{"/x"}, Title: "t", UpdatedAt: time.Unix(100, 5), Model: "m", Effort: "default", State: state}
}

func rows(data ...string) []Row {
	var out []Row
	for i, d := range data {
		out = append(out, Row{Seq: i, Data: []byte(d)})
	}
	return out
}

func strs(b [][]byte) []string {
	var out []string
	for _, x := range b {
		out = append(out, string(x))
	}
	return out
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	open(t, dir, "a")
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, File): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}

func TestSaveAndAcquireRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := open(t, dir, "a")
	if err := a.Create(ctx, session("s1", "idle")); err != nil {
		t.Fatal(err)
	}
	sess := session("s1", "running")
	sess.Title, sess.Cost, sess.Dirs = "renamed", 0.25, nil
	if err := a.Save(ctx, sess, rows(`"h0"`, `"h1"`), rows(`"t0"`)); err != nil {
		t.Fatal(err)
	}
	// Later saves replace entries at the same position.
	if err := a.Save(ctx, sess, nil, []Row{{Seq: 0, Data: []byte(`"t0'"`)}, {Seq: 1, Data: []byte(`"t1"`)}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ctx, "s1"); err != nil {
		t.Fatal(err)
	}

	l, err := open(t, dir, "b").Acquire(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if l.Title != "renamed" || l.Cost != 0.25 || l.State != "running" || len(l.Dirs) != 0 || l.Dirs == nil ||
		!l.UpdatedAt.Equal(time.Unix(100, 5)) || l.Cwd != "/w" || l.Model != "m" {
		t.Fatalf("session: %+v", l.Session)
	}
	if !slices.Equal(strs(l.History), []string{`"h0"`, `"h1"`}) || !slices.Equal(strs(l.Transcript), []string{`"t0'"`, `"t1"`}) {
		t.Fatalf("entries: %q %q", l.History, l.Transcript)
	}
	if _, err := a.Acquire(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}

func TestSaveReplacesHistorySuffix(t *testing.T) {
	s := open(t, t.TempDir(), "a")
	sess := session("s", "idle")
	if err := s.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, sess, rows(`"old"`, `"answer"`, `"prompt"`, `"result"`), rows(`"original replay"`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, sess, rows(`"summary"`, `"latest prompt"`), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, sess, []Row{{Seq: 2, Data: []byte(`"new answer"`)}}, nil); err != nil {
		t.Fatal(err)
	}
	l, err := s.Acquire(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(strs(l.History), []string{`"summary"`, `"latest prompt"`, `"new answer"`}) ||
		!slices.Equal(strs(l.Transcript), []string{`"original replay"`}) {
		t.Fatalf("stale history or damaged replay: %q, %q", l.History, l.Transcript)
	}
}

func TestOwnershipBetweenProcesses(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir, "a"), open(t, dir, "b")
	if err := a.Create(ctx, session("s1", "running")); err != nil {
		t.Fatal(err)
	}

	// Busy in a: b can neither take nor delete it.
	if _, err := b.Acquire(ctx, "s1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("acquire busy: %v", err)
	}
	if err := b.Delete(ctx, "s1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete busy: %v", err)
	}

	// Idle in a: b takes it over, and a finds out.
	if err := a.Save(ctx, session("s1", "idle"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(ctx, "s1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if err := a.Save(ctx, session("s1", "running"), rows(`"late"`), nil); !errors.Is(err, ErrLost) {
		t.Fatalf("save after takeover: %v", err)
	}
	if lost, err := a.Renew(ctx, []string{"s1"}); err != nil || !slices.Equal(lost, []string{"s1"}) {
		t.Fatalf("renew after takeover: %v %v", lost, err)
	}
	if lost, err := b.Renew(ctx, []string{"s1"}); err != nil || len(lost) != 0 {
		t.Fatalf("renew by owner: %v %v", lost, err)
	}
	if err := a.Release(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if lost, _ := b.Renew(ctx, []string{"s1"}); len(lost) != 0 {
		t.Fatal("release by a non-owner dropped b's lease")
	}

	// A lease left to expire is taken in any state.
	if err := b.Save(ctx, session("s1", "running"), nil, nil); err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Now().Add(leaseTTL + time.Second) }
	l, err := a.Acquire(ctx, "s1")
	if err != nil || l.State != "running" {
		t.Fatalf("expired lease: %+v %v", l, err)
	}
	if l, _ := a.Acquire(ctx, "s1"); l == nil {
		t.Fatal("owner cannot reacquire its own session")
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	a := open(t, dir, "a")
	if err := a.Delete(ctx, "never"); err != nil {
		t.Fatalf("unknown: %v", err)
	}
	if err := a.Create(ctx, session("s1", "running")); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(ctx, session("s1", "running"), rows(`1`), rows(`2`)); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, "s1"); err != nil {
		t.Fatalf("own session: %v", err)
	}
	if err := a.Delete(ctx, "s1"); err != nil {
		t.Fatalf("already deleted: %v", err)
	}
	if lost, err := a.Renew(ctx, []string{"s1"}); err != nil || !slices.Equal(lost, []string{"s1"}) {
		t.Fatalf("renew after delete: %v %v", lost, err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM history) + (SELECT count(*) FROM transcript)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("entries left: %d %v", n, err)
	}
}

func TestList(t *testing.T) {
	a := open(t, t.TempDir(), "a")
	for i, id := range []string{"old", "mid", "new"} {
		s := session(id, "idle")
		s.UpdatedAt = time.Unix(int64(i), 0)
		if id == "mid" {
			s.Cwd = "/other"
		}
		if err := a.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(ss []Session) (out []string) {
		for _, s := range ss {
			out = append(out, s.ID)
		}
		return out
	}
	page, more, err := a.List(ctx, "", 0, 2)
	if err != nil || !more || !slices.Equal(ids(page), []string{"new", "mid"}) {
		t.Fatalf("first page: %v %v %v", ids(page), more, err)
	}
	page, more, _ = a.List(ctx, "", 2, 2)
	if more || !slices.Equal(ids(page), []string{"old"}) || !slices.Equal(page[0].Dirs, []string{"/x"}) {
		t.Fatalf("last page: %+v %v", page, more)
	}
	if page, _, _ = a.List(ctx, "/w", 0, 10); !slices.Equal(ids(page), []string{"new", "old"}) {
		t.Fatalf("cwd filter: %v", ids(page))
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	if err := open(t, dir, "a").Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(dir, "a"); err == nil {
		t.Fatal("opened a newer schema")
	}
}

func TestSelectionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a := open(t, dir, "a")
	if model, effort, err := a.Selection(ctx); err != nil || model != "" || effort != "" {
		t.Fatalf("initial selection: %q %q %v", model, effort, err)
	}
	if err := a.SetSelection(ctx, "m", "high"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := open(t, dir, "b")
	if model, effort, err := b.Selection(ctx); err != nil || model != "m" || effort != "high" {
		t.Fatalf("reopened selection: %q %q %v", model, effort, err)
	}
	if err := b.SetSelection(ctx, "other", "default"); err != nil {
		t.Fatal(err)
	}
	if model, effort, err := b.Selection(ctx); err != nil || model != "other" || effort != "default" {
		t.Fatalf("updated selection: %q %q %v", model, effort, err)
	}
}

func TestMigrateSessionDatabaseAddsSettings(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db, owner: "old", now: time.Now}
	if err := old.Create(ctx, session("existing", "idle")); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded := open(t, dir, "new")
	if _, err := upgraded.Acquire(ctx, "existing"); err != nil {
		t.Fatalf("existing session after migration: %v", err)
	}
	if model, effort, err := upgraded.Selection(ctx); err != nil || model != "" || effort != "" {
		t.Fatalf("migration invented a selection: %q %q %v", model, effort, err)
	}
	if err := upgraded.SetSelection(ctx, "m", "default"); err != nil {
		t.Fatalf("settings after migration: %v", err)
	}
}

// A new turn and a takeover racing for an idle session: exactly one wins.
func TestTakeoverRacesNewTurn(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir, "a"), open(t, dir, "b")
	if err := a.Create(ctx, session("s1", "idle")); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		turn, takeover := make(chan error, 1), make(chan error, 1)
		go func() { turn <- a.Save(ctx, session("s1", "running"), nil, nil) }()
		go func() {
			_, err := b.Acquire(ctx, "s1")
			takeover <- err
		}()
		errTurn, errTakeover := <-turn, <-takeover
		switch {
		case errTurn == nil && errors.Is(errTakeover, ErrBusy):
		case errors.Is(errTurn, ErrLost) && errTakeover == nil:
		default:
			t.Fatalf("round %d: turn %v, takeover %v", i, errTurn, errTakeover)
		}
		// Hand the idle session back to a for the next round.
		if errTakeover == nil {
			if err := b.Release(ctx, "s1"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Acquire(ctx, "s1"); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.Save(ctx, session("s1", "idle"), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}
