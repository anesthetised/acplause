package agent

import (
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

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
		})
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
