package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

func TestGroupedModelOptions(t *testing.T) {
	c := newCatalog([]llm.Model{
		{ID: "z/model", Name: "Zed"}, {ID: "a/second"}, {ID: "a/first", Name: "First"},
		{ID: "local"}, {ID: "/leading"}, {ID: "trailing/"},
		{ID: "filtered/model", SupportedParameters: []string{"temperature"}},
	})
	for _, selected := range []string{"", "a/first"} {
		t.Run("selected="+selected, func(t *testing.T) {
			data, err := jsonrpc.Marshal(c.options(selected, effortDefault, modeBuild))
			if err != nil {
				t.Fatal(err)
			}
			var options []struct {
				ConfigID, CurrentValue string
				Options                []struct {
					GroupID, Name string
					Options       []acp.ConfigOptionValue
				}
			}
			if err := json.Unmarshal(data, &options); err != nil {
				t.Fatal(err)
			}
			model := options[0]
			if model.CurrentValue != selected {
				t.Fatalf("selection changed: %s", data)
			}
			var ids, values []string
			for _, group := range model.Options {
				if group.GroupID == "" || group.Name == "" || len(group.Options) == 0 {
					t.Fatalf("invalid group: %s", data)
				}
				ids = append(ids, group.GroupID)
				for _, option := range group.Options {
					values = append(values, option.Value)
				}
			}
			wantIDs := []string{"provider:a", "provider:z", "unqualified"}
			wantValues := []string{"a/first", "a/second", "z/model", "/leading", "local", "trailing/"}
			if selected == "" {
				wantIDs = append([]string{"selection"}, wantIDs...)
				wantValues = append([]string{""}, wantValues...)
			}
			if !slices.Equal(ids, wantIDs) || !slices.Equal(values, wantValues) {
				t.Fatalf("groups=%v values=%v, want %v %v", ids, values, wantIDs, wantValues)
			}
		})
	}
}

func TestSingleProviderModelOptionsStayFlat(t *testing.T) {
	for _, ids := range [][]string{{"one/a", "one/b"}, {"local", "another"}} {
		c := newCatalog([]llm.Model{{ID: ids[0]}, {ID: ids[1]}})
		data, _ := jsonrpc.Marshal(c.options("", effortDefault, modeBuild))
		var options []struct{ Options []struct{ Value *string } }
		if err := json.Unmarshal(data, &options); err != nil || len(options[0].Options) != 3 || options[0].Options[0].Value == nil || *options[0].Options[0].Value != "" {
			t.Fatalf("flat options changed: %s, %v", data, err)
		}
	}
}

func TestCacheCatalogSharesAttemptsAndRetriesFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan error)
		var calls atomic.Int32
		get := cacheCatalog(func() (*catalog, error) {
			calls.Add(1)
			if err := <-release; err != nil {
				return nil, err
			}
			return newCatalog(testModels), nil
		}, time.Hour, slog.New(slog.DiscardHandler))
		results := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := get()
				results <- err
			}()
		}
		synctest.Wait() // both callers wait on one load
		release <- errors.New("down")
		for range 2 {
			if err := <-results; err == nil {
				t.Fatal("shared attempt succeeded")
			}
		}
		go func() { release <- nil }()
		first, err := get()
		if err != nil {
			t.Fatal(err)
		}
		if again, err := get(); err != nil || again != first || calls.Load() != 2 {
			t.Fatalf("cached catalog: %p, %p, %v, %d loads", first, again, err, calls.Load())
		}
	})
}

// refreshing drives cacheCatalog with a scripted loader: each load returns the
// next result and counts itself.
type refreshing struct {
	calls   atomic.Int32
	results chan *catalog // nil makes the load fail
	log     bytes.Buffer
	get     func() (*catalog, error)
}

func newRefreshing(ttl time.Duration) *refreshing {
	r := &refreshing{results: make(chan *catalog, 4)}
	r.get = cacheCatalog(func() (*catalog, error) {
		r.calls.Add(1)
		if c := <-r.results; c != nil {
			return c, nil
		}
		return nil, errors.New("down")
	}, ttl, slog.New(slog.NewTextHandler(&r.log, nil)))
	return r
}

func TestCacheCatalogReloadsAfterTTLInBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRefreshing(time.Hour)
		oldCat, newCat := newCatalog([]llm.Model{{ID: "old"}}), newCatalog([]llm.Model{{ID: "new"}})
		r.results <- oldCat
		if c, err := r.get(); err != nil || c != oldCat {
			t.Fatalf("first load: %p, %v", c, err)
		}
		time.Sleep(time.Hour - time.Second)
		if c, _ := r.get(); c != oldCat {
			t.Fatal("catalog changed before the TTL")
		}
		synctest.Wait()
		if r.calls.Load() != 1 {
			t.Fatalf("reloaded before the TTL: %d loads", r.calls.Load())
		}
		time.Sleep(time.Second)
		// The reload is blocked on the network; the caller must not wait for it.
		if c, err := r.get(); err != nil || c != oldCat {
			t.Fatalf("stale call: %p, %v", c, err)
		}
		synctest.Wait()
		if r.calls.Load() != 2 {
			t.Fatalf("no reload after the TTL: %d loads", r.calls.Load())
		}
		r.results <- newCat
		synctest.Wait()
		if c, _ := r.get(); c != newCat {
			t.Fatal("later caller did not see the reloaded catalog")
		}
		if r.calls.Load() != 2 {
			t.Fatalf("fresh catalog reloaded again: %d loads", r.calls.Load())
		}
	})
}

func TestCacheCatalogKeepsPreviousListWhenReloadFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRefreshing(time.Hour)
		oldCat := newCatalog([]llm.Model{{ID: "old"}})
		r.results <- oldCat
		r.get()
		time.Sleep(time.Hour)
		r.results <- nil
		r.get()
		synctest.Wait()
		if c, err := r.get(); err != nil || c != oldCat {
			t.Fatalf("after failed reload: %p, %v", c, err)
		}
		if !strings.Contains(r.log.String(), "level=WARN") || !strings.Contains(r.log.String(), "down") {
			t.Fatalf("no warning logged: %q", r.log.String())
		}
		// The stale list stays eligible for a retry.
		newCat := newCatalog([]llm.Model{{ID: "new"}})
		synctest.Wait()
		r.results <- newCat
		synctest.Wait()
		if c, _ := r.get(); c != newCat {
			t.Fatal("retry after a failed reload did not refresh")
		}
	})
}

func TestCacheCatalogReloadsAtMostOnceConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRefreshing(time.Hour)
		r.results <- newCatalog([]llm.Model{{ID: "old"}})
		r.get()
		time.Sleep(time.Hour)
		for range 10 {
			go r.get()
		}
		synctest.Wait()
		if r.calls.Load() != 2 {
			t.Fatalf("%d loads for concurrent stale callers, want 1 reload", r.calls.Load()-1)
		}
		r.results <- newCatalog([]llm.Model{{ID: "new"}})
		synctest.Wait()
	})
}
