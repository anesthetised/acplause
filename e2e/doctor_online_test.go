package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDoctorOnlineCatalogOnlyAndSafeFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		status            int
		body, reason, key string
		requests          int
	}{
		{"no key success", 200, `{"data":[{"id":"secret-model","name":"secret-description","supported_parameters":["tools"]},{"id":"unknown-metadata"},{"id":"plain","supported_parameters":["temperature"]}]}`, "2 selectable tool-capable models", "", 1},
		{"key success", 200, `{"data":[{"id":"secret-model"}]}`, "1 selectable tool-capable models", "secret-key", 1},
		{"auth", 401, `{"error":{"message":"secret-auth"}}`, "authentication", "secret-key", 1},
		{"forbidden", 403, `secret-body`, "authentication", "", 1},
		{"rate limit", 429, `secret-body`, "rate limited", "", 4},
		{"server error", 503, `secret-body`, "provider unavailable", "", 4},
		{"malformed", 200, `{"data":"secret-body"}`, "malformed catalog", "", 1},
		{"empty", 200, `{"data":[]}`, "no selectable", "", 1},
		{"no tools", 200, `{"data":[{"id":"secret-model","supported_parameters":[]}]}`, "no selectable", "", 1},
		{"deadline", 0, "", "timeout", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				auth := ""
				if tc.key != "" {
					auth = "Bearer " + tc.key
				}
				if r.Method != "GET" || r.URL.Path != "/api/models" || r.Header.Get("Authorization") != auth {
					t.Error("doctor made a non-catalog request or used the wrong credential")
				}
				if tc.status == 0 {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			dir := t.TempDir()
			db := filepath.Join(dir, "acplause.db")
			if err := os.WriteFile(db, []byte("unchanged database sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, report := runDoctorBinary(t, []string{"doctor", "--online"}, []string{"ACPLAUSE_BASE_URL=" + server.URL + "/api", "ACPLAUSE_API_KEY=" + tc.key, "ACPLAUSE_DATA_DIR=" + dir})
			good := strings.HasSuffix(tc.name, "success")
			wantCode := 1
			if good {
				wantCode = 0
			}
			if code != wantCode || !strings.Contains(report, tc.reason) || strings.Contains(report, "secret-") || strings.Contains(report, server.URL) || requests.Load() != int32(tc.requests) {
				t.Fatalf("online doctor code=%d requests=%d report=%s", code, requests.Load(), report)
			}
			entries, err := os.ReadDir(dir)
			raw, readErr := os.ReadFile(db)
			if err != nil || readErr != nil || len(entries) != 1 || string(raw) != "unchanged database sentinel" {
				t.Fatal("online doctor touched session storage or left a probe")
			}
		})
	}
}
