package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/llm"
)

func TestDoctorCatalogOutcomesUseExistingClient(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		count    int
		reason   string
		requests int
	}{
		{"mixed", 200, `{"data":[{"id":"secret-model","name":"secret-description","supported_parameters":["tools"]},{"id":"secret-model","supported_parameters":["temperature"]},{"id":"other"},{"id":"deny","supported_parameters":[]},{"id":"","supported_parameters":["tools"]}]}`, 2, "", 1},
		{"empty", 200, `{"data":[]}`, 0, "no selectable", 1},
		{"no tools", 200, `{"data":[{"id":"plain","supported_parameters":["temperature"],"architecture":{"input_modalities":["text"]}}]}`, 0, "no selectable", 1},
		{"explicit empty parameters", 200, `{"data":[{"id":"plain","supported_parameters":[]}]}`, 0, "no selectable", 1},
		{"bad JSON", 200, `{"secret-body":`, 0, "malformed catalog", 1},
		{"bad field type", 200, `{"data":"secret-body"}`, 0, "malformed catalog", 1},
		{"unauthorized", 401, `{"error":{"message":"secret-body"}}`, 0, "authentication", 1},
		{"forbidden", 403, `secret-body`, 0, "authentication", 1},
		{"rate limit", 429, `secret-body`, 0, "rate limit", 4},
		{"server error", 503, `secret-body`, 0, "provider unavailable", 4},
		{"timeout status", 408, `secret-body`, 0, "timeout", 4},
		{"endpoint missing", 404, `secret-body`, 0, "catalog endpoint unavailable", 1},
		{"billing", 402, `secret-body`, 0, "billing", 1},
		{"bad request", 400, `secret-body`, 0, "rejected", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/models" || r.Header.Get("Authorization") != "Bearer secret-key" {
					t.Error("not the configured catalog request")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client := llm.New(llm.Config{BaseURL: server.URL + "/api", APIKey: "secret-key", RetryDelay: time.Millisecond})
			count, reason := checkCatalog(t.Context(), client.Models)
			if count != tc.count || tc.reason == "" && reason != "" || tc.reason != "" && !strings.Contains(reason, tc.reason) || strings.Contains(reason, "secret-") || requests.Load() != int32(tc.requests) {
				t.Fatalf("catalog count=%d reason=%s requests=%d", count, reason, requests.Load())
			}
		})
	}
}

func TestDoctorCatalogDeadlineCoversRetriesAndCancellation(t *testing.T) {
	// A shared deadline is derived once around the whole existing client call.
	_, reason := checkCatalog(t.Context(), func(ctx context.Context) ([]llm.Model, error) {
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining <= 0 || remaining > 15*time.Second {
			t.Fatalf("catalog deadline=%v", remaining)
		}
		return []llm.Model{{ID: "usable"}}, nil
	})
	if reason != "" {
		t.Fatal(reason)
	}
	for _, mode := range []string{"cancel", "deadline", "retry"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				if mode == "retry" {
					http.Error(w, "secret-body", http.StatusServiceUnavailable)
					return
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "deadline" {
				var stop context.CancelFunc
				parent, stop = context.WithTimeout(parent, time.Second)
				defer stop()
			}
			client := llm.New(llm.Config{BaseURL: server.URL, RetryDelay: time.Hour})
			done := make(chan string, 1)
			go func() { _, reason := checkCatalog(parent, client.Models); done <- reason }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("catalog not requested")
			}
			if mode != "deadline" {
				cancel()
			}
			select {
			case reason := <-done:
				want := "cancelled"
				if mode == "deadline" {
					want = "timeout"
				}
				if !strings.Contains(reason, want) || strings.Contains(reason, "secret-") {
					t.Fatalf("catalog cancellation: %s", reason)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("catalog did not cancel retries/request")
			}
		})
	}
}

func TestDoctorOnlineDispatcherAndLocalFailureGate(t *testing.T) {
	for _, mode := range []string{"offline", "online", "bad config", "bad URL", "bad directory"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" || r.URL.Path != "/models" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected diagnostic request")
				}
				fmt.Fprint(w, `{"data":[{"id":"secret-model","name":"secret-description","supported_parameters":["tools"]}]}`)
			}))
			defer server.Close()
			settings := map[string]string{"ACPLAUSE_DATA_DIR": t.TempDir(), "ACPLAUSE_BASE_URL": server.URL}
			args := []string{"doctor", "--online"}
			switch mode {
			case "offline":
				args = []string{"doctor"}
			case "bad config":
				settings["ACPLAUSE_MAX_ROUNDS"] = "secret-config"
			case "bad URL":
				settings["ACPLAUSE_BASE_URL"] += "?key=secret-query"
			case "bad directory":
				settings["ACPLAUSE_DATA_DIR"] += "\x00secret-path"
			}
			var report bytes.Buffer
			served := false
			err := dispatch(args, env(settings), &report, func() error { served = true; return nil })
			good := mode == "online" || mode == "offline"
			wantRequests := int32(0)
			if mode == "online" {
				wantRequests = 1
			}
			if served || (err == nil) != good || requests.Load() != wantRequests || strings.Contains(report.String(), "secret-") || strings.Contains(report.String(), server.URL) {
				t.Fatalf("dispatcher report=%s err=%v requests=%d", &report, err, requests.Load())
			}
			if mode == "online" && !strings.Contains(report.String(), "1 selectable tool-capable models") {
				t.Fatal("missing safe count")
			}
			if !good && !strings.Contains(report.String(), "skipped until local checks pass") {
				t.Fatal("local failure did not gate online check")
			}
		})
	}
}

func TestDoctorCatalogConnectivityAndUnknownErrorsAreSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("closed server received request") }))
	url := server.URL
	server.Close()
	client := llm.New(llm.Config{BaseURL: url, RetryDelay: time.Millisecond})
	_, reason := checkCatalog(t.Context(), client.Models)
	if reason != "connectivity failed" || strings.Contains(reason, url) {
		t.Fatalf("connectivity report: %s", reason)
	}
	if got := catalogFailure(errors.New("secret-error")); got != "catalog request failed" {
		t.Fatalf("raw error leaked: %s", got)
	}
	if got := catalogFailure(&llm.Error{Kind: llm.KindUnknown, Message: "secret-provider"}); got != "catalog request failed" {
		t.Fatalf("unknown provider error leaked: %s", got)
	}
}
