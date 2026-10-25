package tools

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// issue53Entry retains metadata from before a file grew or was replaced.
type issue53Entry struct {
	fs.DirEntry
	info fs.FileInfo
}

func (d issue53Entry) Info() (fs.FileInfo, error) { return d.info, nil }

func TestIssue53SearchBoundsChangedFile(t *testing.T) {
	w := newWorkspace(t, Options{})
	p := w.write("growing.txt", "needle\n")
	entries, err := os.ReadDir(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	w.write("growing.txt", "needle\n"+strings.Repeat("x", maxReadSize))
	s := searcher{pattern: []byte("needle"), show: func(name string) string { return name }}
	if err := s.visit(context.Background(), w.tb.roots.within(w.tb.roots.roots[0], "growing.txt"), issue53Entry{entries[0], info}); err != nil {
		t.Fatal(err)
	}
	if s.matches != 0 {
		t.Fatalf("searched oversized file %s: %q", p, s.out.String())
	}
}

func TestIssue53SearchCancelledRead(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("cached.txt", "needle\n")
	entries, err := os.ReadDir(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := searcher{pattern: []byte("needle"), show: func(name string) string { return name }}
	if err := s.visit(ctx, w.tb.roots.within(w.tb.roots.roots[0], "cached.txt"), entries[0]); err != context.Canceled {
		t.Fatalf("cancelled search: %v", err)
	}
	if s.matches != 0 {
		t.Fatalf("searched after cancellation: %q", s.out.String())
	}
}

func TestIssue53SearchRejectsReplacedEscapingSymlink(t *testing.T) {
	w := newWorkspace(t, Options{})
	p := w.write("cached.txt", "needle\n")
	outside := w.write(w.extra+"/outside.txt", "needle\n")
	entries, err := os.ReadDir(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := testSymlink(t, outside, p); err != nil {
		t.Fatal(err)
	}
	s := searcher{pattern: []byte("needle"), show: func(name string) string { return name }}
	if err := s.visit(context.Background(), w.tb.roots.within(w.tb.roots.roots[0], "cached.txt"), entries[0]); err != nil {
		t.Fatal(err)
	}
	if s.matches != 0 {
		t.Fatalf("searched outside root: %q", s.out.String())
	}
}
