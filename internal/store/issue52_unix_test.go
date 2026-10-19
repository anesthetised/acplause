//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIssue52RelativeDataDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	s := open(t, "relative ?#% data", "owner")
	if err := s.Create(ctx, session("relative", "idle")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, "relative ?#% data", "restarted")
	if _, err := s.Acquire(ctx, "relative"); err != nil {
		t.Fatal(err)
	}
}

func TestIssue52SpecialPathsStayDistinctAcrossReopen(t *testing.T) {
	parent := t.TempDir()
	for i, name := range []string{"a?b", "a?c", "hash#", "percent%", "space name", "雪"} {
		dir := filepath.Join(parent, name)
		s := open(t, dir, "owner")
		info, err := os.Stat(filepath.Join(dir, File))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("database permissions at %q: info=%v err=%v", dir, info, err)
		}
		var journal string
		var busyTimeout, foreignKeys int
		if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
			t.Fatalf("journal_mode at %q: %q, %v", dir, journal, err)
		}
		if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil || busyTimeout != 5000 {
			t.Fatalf("busy_timeout at %q: %d, %v", dir, busyTimeout, err)
		}
		if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
			t.Fatalf("foreign_keys at %q: %d, %v", dir, foreignKeys, err)
		}
		if err := s.Create(ctx, session(name, "idle")); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(dir, "restarted")
		if err != nil {
			t.Fatalf("reopen %q: %v", name, err)
		}
		loaded, err := s.Acquire(ctx, name)
		if err != nil || loaded.ID != name {
			t.Fatalf("reopen %q: loaded=%+v err=%v", name, loaded, err)
		}
		listed, more, err := s.List(ctx, "", 0, 10)
		if err != nil || more || len(listed) != 1 || listed[0].ID != name {
			t.Fatalf("directory %q shares sessions: %+v, more=%v, err=%v", name, listed, more, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := os.Stat(filepath.Join(parent, "a")); !os.IsNotExist(err) {
				t.Fatalf("created sibling collision artifact: stat error %v", err)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, File)); err != nil {
			t.Fatalf("database missing from %q: %v", dir, err)
		}
	}
}
