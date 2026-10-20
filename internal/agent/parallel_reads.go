package agent

import (
	"context"
	"sync"

	"github.com/anesthetised/acplause/internal/llm"
)

// parallelRead admits built-in names only; an external binding always wins
// routing and remains a barrier even when it advertises readOnlyHint.
func parallelRead(call llm.ToolCall, round modelRound) bool {
	name := call.Function.Name
	if _, external := round.bindings[name]; external {
		return false
	}
	switch name {
	case "read_file", "list_dir", "search_code", "get_instructions":
		return true
	}
	return false
}

// runTools drains consecutive read groups before every serial barrier. Only
// this worker owns the toolbox pin; read goroutines use its retained snapshot.
func (s *Session) runTools(ctx context.Context, calls []llm.ToolCall, round modelRound) {
	workers := min(8, max(1, s.deps.readConcurrency))
	for len(calls) > 0 {
		if workers == 1 || !parallelRead(calls[0], round) || s.budget.check(ctx) != nil {
			s.runTool(ctx, calls[0], round)
			calls = calls[1:]
			continue
		}
		n := 1
		for n < len(calls) && parallelRead(calls[n], round) {
			n++
		}
		s.runReadGroup(ctx, calls[:n], round, workers)
		calls = calls[n:]
	}
}

func (s *Session) runReadGroup(ctx context.Context, calls []llm.ToolCall, round modelRound, workers int) {
	tb := s.acquireToolbox()
	defer s.releaseToolbox(tb)
	orderedReads(ctx, calls, workers, func(call llm.ToolCall) toolOutcome {
		return s.executeToolCall(ctx, call, round, tb)
	}, s.commitToolOutcome)
}

// orderedReads bounds both active work and buffered outcomes to one N-call
// window.
// ponytail: refill only on prefix commit; a completion-driven pool can improve
// utilization if measurements justify a larger reorder buffer.
// All commits run on the caller, and every invocation joins before return.
func orderedReads(ctx context.Context, calls []llm.ToolCall, workers int, execute func(llm.ToolCall) toolOutcome, commit func(toolOutcome)) {
	slots := make([]chan toolOutcome, min(workers, len(calls)))
	var active sync.WaitGroup
	start := func(i int) {
		slot := slots[i%len(slots)]
		if ctx.Err() != nil {
			slot <- toolOutcome{message: toolResult(calls[i].ID, "Cancelled before it ran.")}
			return
		}
		active.Go(func() { slot <- execute(calls[i]) })
	}
	for i := range slots {
		slots[i] = make(chan toolOutcome, 1)
		start(i)
	}
	for i := range calls {
		commit(<-slots[i%len(slots)])
		if next := i + len(slots); next < len(calls) {
			start(next)
		}
	}
	active.Wait()
}
