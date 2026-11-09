package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

// Benchmarks for #193. The workload is fixed by the issue: a synthetic
// 500-message history whose tool outputs run from 2 to 64 KiB, a fake model
// and no network. Compare runs with benchstat, for example
//
//	go test -run '^$' -bench 'Prepare|Summarize' -count=10 ./internal/agent/

// benchClient discards every session update.
type benchClient struct{}

func (benchClient) Notify(string, any) error { return nil }

func (benchClient) Call(context.Context, string, any, any) error { return nil }

func (benchClient) CallCancellable(context.Context, string, any, any) error { return nil }

// benchHistory builds the system message followed by 100 turns of a prompt and
// two tool rounds: 500 messages after the system prompt. Tool output sizes are uniform in
// [2 KiB, 64 KiB] from a fixed seed, and the text carries the characters JSON
// escapes so serialization is not a plain copy.
func benchHistory(system llm.Message) []llm.Message {
	const line = "func (s *Session) run(ctx context.Context) error { return fmt.Errorf(\"<%d> a & b\\n\", \"x\") }\n"
	source := strings.Repeat(line, 64<<10/len(line)+1)
	rng := rand.New(rand.NewPCG(193, 193))
	history := []llm.Message{system}
	for turn := range 100 {
		history = append(history, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("turn %d: look into the failing test and fix it", turn)})
		for round := range 2 {
			id := fmt.Sprintf("call-%d-%d", turn, round)
			history = append(history, callMessage(id), toolResult(id, source[:2<<10+rng.IntN(62<<10+1)]))
		}
	}
	return history
}

// benchSession is a session over the benchmark history. The calibration has
// seen one response at 3.5 bytes per token, the steady state of a long
// conversation (ADR 0031).
func benchSession(b *testing.B, model Model) (*Session, []llm.Message) {
	b.Helper()
	cal := new(calibration)
	cal.observe(textRequest("m", 35000), promptUsage(10000))
	s, err := newSession(context.Background(), deps{out: benchClient{}, model: model, maxRounds: 3, calibration: cal},
		newCatalog([]llm.Model{{ID: "m"}}), b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.Close)
	if err := s.SetConfigOption(configModel, acp.ConfigValueID, json.RawMessage(`"m"`)); err != nil {
		b.Fatal(err)
	}
	history := benchHistory(s.history[0])
	if len(history) != 501 {
		b.Fatalf("history of %d messages", len(history))
	}
	return s, history
}

// BenchmarkPrepareTrimToolOutput measures prepareModelRequest when only
// trimming old tool output makes the history fit: the window is sized so that
// 85% of the output has to go, and the model must not be asked for a summary.
func BenchmarkPrepareTrimToolOutput(b *testing.B) {
	s, history := benchSession(b, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		b.Fatal("trimming should avoid a summary request")
		return nil, nil
	}))
	s.history = slices.Clone(history)
	full := s.request().Request
	trimmed := full
	trimmed.Messages = slices.Clone(full.Messages)
	for i, m := range trimmed.Messages {
		if m.Role == llm.RoleTool {
			trimmed.Messages[i].Content = trimToolOutput(m.Content)
		}
	}
	lo, hi := s.deps.calibration.size(trimmed), s.deps.calibration.size(full)
	budget := lo + (hi-lo)*15/100
	s.catalog = newCatalog([]llm.Model{windowModel("m", budget+budget/3+1, 0)})

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		s.history = slices.Clone(history)
		b.StartTimer()
		if _, err := s.prepareRequest(context.Background(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSummarizeChunks measures summarize over the whole history with a
// budget that cuts it into chunks of about a sixteenth of the history each.
func BenchmarkSummarizeChunks(b *testing.B) {
	calls := 0
	s, history := benchSession(b, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		calls++
		return &llm.Completion{Content: "The user is fixing a failing test.", FinishReason: llm.FinishStop}, nil
	}))
	s.history = slices.Clone(history)
	req := s.request().Request
	prefix := req.Messages[1:]
	budget := s.deps.calibration.size(req) / 16

	b.ReportAllocs()
	for b.Loop() {
		calls = 0
		if _, err := s.summarize(context.Background(), req, prefix, budget, nil, 0); err != nil {
			b.Fatal(err)
		}
		if calls < 8 {
			b.Fatalf("only %d chunks", calls)
		}
	}
}
