//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIssue52SpecialDataDirectorySurvivesRestart(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	data := filepath.Join(parent, "data ?#% café")
	llm := newMockLLM(t)
	a := start(t, llm, withDataDir(data))
	a.initialize()
	sid := a.newSession(t.TempDir()).SessionID
	a.stop()

	if _, err := os.Stat(filepath.Join(data, "acplause.db")); err != nil {
		t.Fatalf("database not created in configured directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "data ")); !os.IsNotExist(err) {
		t.Fatalf("created sibling database artifact: stat error %v", err)
	}

	b := start(t, llm, withDataDir(data))
	b.initialize()
	var sessions listResult
	b.ok("session/list", map[string]any{}, &sessions)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].SessionID != sid {
		t.Fatalf("sessions after restart: %+v", sessions.Sessions)
	}
}
