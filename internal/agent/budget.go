package agent

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/anesthetised/acplause/internal/llm"
)

// turnBudget belongs to the single session worker. Only completed model
// responses change it; read workers only inspect its sticky stop while the
// session worker waits for their group to join, without charging again.
type turnBudget struct {
	maxTokens uint64
	maxCost   float64
	tokens    uint64
	cost      float64
	err       error
}

func (b *turnBudget) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err // cancellation always takes precedence over accounting
	}
	if b == nil {
		return nil // standalone preparation outside a turn has no allowance
	}
	return b.err
}

// record accepts usage before summary validation or history replacement. A
// missing metric matters only when its limit is enabled. Each enabled counter
// is checked independently so valid sibling accounting is retained on error.
func (b *turnBudget) record(u *llm.Usage) error {
	if b == nil {
		return nil
	}
	if b.err != nil {
		return b.err
	}
	var errs []error
	if b.maxTokens > 0 {
		n, ok := u.ReportedTokens()
		switch {
		case !ok:
			errs = append(errs, errors.New("turn token accounting unavailable or invalid"))
		case b.tokens > math.MaxUint64-n:
			errs = append(errs, errors.New("turn token accounting unavailable: counter overflow"))
		default:
			b.tokens += n
			if b.tokens >= b.maxTokens {
				errs = append(errs, fmt.Errorf("turn token budget reached (%d >= %d)", b.tokens, b.maxTokens))
			}
		}
	}
	if b.maxCost > 0 {
		n, ok := u.ReportedCost()
		switch {
		case !ok:
			errs = append(errs, errors.New("turn USD accounting unavailable or invalid"))
		case math.IsInf(b.cost+n, 0):
			errs = append(errs, errors.New("turn USD accounting unavailable: counter overflow"))
		default:
			b.cost += n
			if b.cost >= b.maxCost {
				errs = append(errs, fmt.Errorf("turn USD budget reached (%g >= %g)", b.cost, b.maxCost))
			}
		}
	}
	b.err = errors.Join(errs...)
	return b.err
}
