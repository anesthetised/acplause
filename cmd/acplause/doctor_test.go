package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorLocalChecksAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]string
		valid    bool
		want     string
	}{
		{"no key", nil, true, "API key: INFO (not set"},
		{"key", map[string]string{"ACPLAUSE_API_KEY": "secret-marker"}, true, "API key: PASS (set; redacted)"},
		{"other key", map[string]string{"OPENROUTER_API_KEY": "secret-marker"}, true, "API key: INFO (not set"},
		{"bad URL", map[string]string{"ACPLAUSE_BASE_URL": "https://secret-marker:%xx"}, false, "API URL: FAIL"},
		{"URL credentials", map[string]string{"ACPLAUSE_BASE_URL": "https://user:secret-marker@example.com/api"}, false, "API URL: FAIL"},
		{"URL query", map[string]string{"ACPLAUSE_BASE_URL": "http://localhost/api?key=secret-marker#secret-marker"}, false, "API URL: FAIL"},
		{"bad setting", map[string]string{"ACPLAUSE_MAX_ROUNDS": "secret-marker"}, false, "ACPLAUSE_MAX_ROUNDS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{"ACPLAUSE_DATA_DIR": t.TempDir()}
			for k, v := range tc.settings {
				settings[k] = v
			}
			var report bytes.Buffer
			err := doctor(env(settings), &report, os.UserHomeDir, os.CreateTemp, false)
			if (err == nil) != tc.valid || !strings.Contains(report.String(), tc.want) || strings.Contains(report.String(), "secret-marker") || strings.Contains(report.String(), settings["ACPLAUSE_DATA_DIR"]) {
				t.Fatalf("unsafe or wrong report: %s / %v", &report, err)
			}
			entries, e := os.ReadDir(settings["ACPLAUSE_DATA_DIR"])
			if e != nil || len(entries) != 0 {
				t.Fatalf("doctor left files: %v / %v", entries, e)
			}
		})
	}
}

func TestDoctorProbeCleanupAndDefaultDirectory(t *testing.T) {
	for _, failure := range []string{"", "write", "create", "file", "home"} {
		t.Run(failure, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".acplause")
			homeDir := func() (string, error) {
				if failure == "home" {
					return "", errors.New("secret-marker")
				}
				return home, nil
			}
			create := os.CreateTemp
			if failure == "create" {
				create = func(string, string) (*os.File, error) { return nil, errors.New("secret-marker") }
			}
			if failure == "write" {
				create = func(dir, pattern string) (*os.File, error) {
					f, err := os.CreateTemp(dir, pattern)
					if err == nil {
						err = f.Close()
					}
					return f, err
				}
			}
			if failure == "file" {
				if err := os.WriteFile(dir, []byte("existing database marker"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var report bytes.Buffer
			err := doctor(env(nil), &report, homeDir, create, false)
			if (err == nil) != (failure == "") || strings.Contains(report.String(), "secret-marker") || strings.Contains(report.String(), home) {
				t.Fatalf("probe status: %s / %v", &report, err)
			}
			if failure == "home" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("unresolved directory created")
				}
				return
			}
			if failure == "file" {
				got, _ := os.ReadFile(dir)
				if string(got) != "existing database marker" {
					t.Fatal("existing file changed")
				}
				return
			}
			if !strings.Contains(report.String(), "created directory") {
				t.Fatal("directory creation not explained")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("probe not removed: %v / %v", entries, err)
			}
		})
	}
}

func TestDoctorDispatcherDoesNotEnterServer(t *testing.T) {
	for _, args := range [][]string{nil, {"doctor"}, {"secret-marker"}, {"doctor", "--unknown"}, {"doctor", "secret-marker"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			called := false
			var report bytes.Buffer
			err := dispatch(args, env(map[string]string{"ACPLAUSE_DATA_DIR": t.TempDir()}), &report, func() error { called = true; return nil })
			if called != (len(args) == 0) || (err == nil) != (len(args) <= 1 && (len(args) == 0 || args[0] == "doctor")) {
				t.Fatalf("dispatcher called=%v err=%v", called, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-marker") {
				t.Fatal("argument echoed")
			}
		})
	}
}

func TestDoctorSanitizesEveryParsedSetting(t *testing.T) {
	for _, key := range []string{"ACPLAUSE_READ_CONCURRENCY", "ACPLAUSE_MAX_TURN_TOKENS", "ACPLAUSE_MAX_TURN_COST_USD", "ACPLAUSE_TURN_TIMEOUT", "ACPLAUSE_MCP_TIMEOUT", "ACPLAUSE_MAX_ROUNDS", "ACPLAUSE_BASH_TIMEOUT", "ACPLAUSE_LOG_LEVEL"} {
		t.Run(key, func(t *testing.T) {
			var report bytes.Buffer
			err := doctor(env(map[string]string{key: "secret-marker", "ACPLAUSE_DATA_DIR": t.TempDir()}), &report, os.UserHomeDir, os.CreateTemp, false)
			if err == nil || !strings.Contains(report.String(), key) || strings.Contains(report.String(), "secret-marker") {
				t.Fatalf("setting redaction: %s", &report)
			}
		})
	}
}

func TestDoctorURLStructure(t *testing.T) {
	for _, raw := range []string{"http://localhost:1234/v1", "https://example.com/api/v1/", "http://[::1]:80/v1"} {
		if !validAPIURL(raw) {
			t.Errorf("rejected API root %q", raw)
		}
	}
	for _, raw := range []string{"", "://bad", "/relative", "http:///path", "file:///tmp/provider", "http://:80/v1", "http://example.com:65536", "http://example.com:0", "https://example.com?", "https://example.com/#", "https://example.com/#secret", "https://user:password@example.com"} {
		if validAPIURL(raw) {
			t.Errorf("accepted invalid API root %q", raw)
		}
	}
}

func TestDoctorDirectoryInspectionFailuresAreSafe(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret-parent")
	if err := os.WriteFile(file, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(file, "child"), "secret-marker\x00invalid"} {
		var report bytes.Buffer
		err := doctor(env(map[string]string{"ACPLAUSE_DATA_DIR": dir}), &report, os.UserHomeDir, os.CreateTemp, false)
		if err == nil || !strings.Contains(report.String(), "data directory: FAIL") || strings.Contains(report.String(), "secret-") {
			t.Fatalf("unsafe inspect failure: %s", &report)
		}
	}
}
