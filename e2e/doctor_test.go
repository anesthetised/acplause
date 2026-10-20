package e2e

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/store"
)

func runDoctorBinary(t *testing.T, args, settings []string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(cleanEnv(), settings...)
	// No protocol client or interactive input exists on this path.
	cmd.Stdin = strings.NewReader("not a protocol message\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exit, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if ctx.Err() != nil || stdout.Len() != 0 {
		t.Fatalf("doctor hung or wrote stdout: %q / %v", &stdout, ctx.Err())
	}
	return code, stderr.String()
}

func TestDoctorLocalProcessRedactionNoNetworkOrDatabase(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Error(w, "secret-response", 500) }))
	defer server.Close()
	for _, tc := range []struct {
		name           string
		args, settings []string
		code           int
		want           string
	}{
		{"missing key", []string{"doctor"}, nil, 0, "API key: INFO"},
		{"key", []string{"doctor"}, []string{"ACPLAUSE_API_KEY=secret-key"}, 0, "API key: PASS"},
		{"invalid config", []string{"doctor"}, []string{"ACPLAUSE_BASH_TIMEOUT=secret-config", "ACPLAUSE_LOG_LEVEL=secret-level"}, 1, "ACPLAUSE_BASH_TIMEOUT, ACPLAUSE_LOG_LEVEL"},
		{"URL credentials", []string{"doctor"}, []string{"ACPLAUSE_BASE_URL=http://user:secret-url@localhost/api?key=secret-query#secret-fragment"}, 1, "API URL: FAIL"},
		{"unknown argument", []string{"secret-argument"}, nil, 1, "usage:"},
		{"unknown doctor flag", []string{"doctor", "--unknown=secret-flag"}, nil, 1, "usage:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Invalid SQLite contents make accidental open/migration observable.
			// WAL/SHM sentinels must also survive byte-for-byte, with their modes.
			files := map[string][]byte{}
			modes := map[string]os.FileMode{}
			for _, name := range []string{"acplause.db", "acplause.db-wal", "acplause.db-shm"} {
				files[name] = []byte("private store sentinel " + name)
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, files[name], 0o640); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				modes[name] = info.Mode()
			}
			settings := append([]string{"ACPLAUSE_BASE_URL=" + server.URL, "ACPLAUSE_DATA_DIR=" + dir}, tc.settings...)
			code, report := runDoctorBinary(t, tc.args, settings)
			if code != tc.code || !strings.Contains(report, tc.want) || strings.Contains(report, "secret-") || strings.Contains(report, dir) || strings.Contains(report, server.URL) {
				t.Fatalf("doctor code=%d report=%s", code, report)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 3 {
				t.Fatal("doctor left a probe or changed store files")
			}
			for name, want := range files {
				path := filepath.Join(dir, name)
				got, err := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if err != nil || statErr != nil || !bytes.Equal(got, want) || info.Mode() != modes[name] {
					t.Fatal("doctor changed a database file or its permissions")
				}
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("offline doctor contacted provider")
	}
}

func TestDoctorProcessDefaultDirectoryAndFailures(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	code, report := runDoctorBinary(t, []string{"doctor"}, []string{"HOME=" + home, "USERPROFILE=" + home})
	dir := filepath.Join(home, ".acplause")
	if code != 0 || !strings.Contains(report, "created directory") {
		t.Fatalf("default probe: %s", report)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("default probe left files or opened store")
	}
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, report = runDoctorBinary(t, []string{"doctor"}, []string{"ACPLAUSE_DATA_DIR=" + path})
	if code != 1 || !strings.Contains(report, "not a directory") || strings.Contains(report, path) {
		t.Fatalf("directory failure: %s", report)
	}
}

func TestDoctorPreservesStoredSessionLease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, err := store.Open(dir, "doctor-existing-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(t.Context(), store.Session{ID: "doctor-existing-session", Cwd: t.TempDir(), Model: "m", State: "running", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		before[entry.Name()] = raw
	}
	code, report := runDoctorBinary(t, []string{"doctor"}, []string{"ACPLAUSE_DATA_DIR=" + dir})
	if code != 0 {
		t.Fatalf("doctor on existing store: %s", report)
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(after) != len(before) {
		t.Fatal("doctor changed store files")
	}
	for _, entry := range after {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil || !bytes.Equal(raw, before[entry.Name()]) {
			t.Fatal("doctor changed stored session, model selection or owner lease")
		}
	}
}
