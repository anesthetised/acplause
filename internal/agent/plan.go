package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
)

// updatePlan publishes a full snapshot at the plan's first transcript
// position. Only an accepted turn may allocate an ID; resume and live
// updates share mu so an older replay cannot overtake a newer snapshot.
func (s *Session) updatePlan(ctx context.Context, entries []acp.PlanEntry) (string, error) {
	if err := validatePlan(entries); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lost {
		return "", errSessionLost()
	}
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		return "", context.Canceled
	}
	if s.state.State == acp.StateIdle {
		return "", fmt.Errorf("plan requires an accepted turn")
	}
	if s.planID == "" {
		s.planID = newID()
	}
	u := acp.PlanUpdate{Plan: acp.PlanItems{Type: "items", PlanID: s.planID, Entries: slices.Clone(entries)}}
	i, exists := s.planIndex[s.planID]
	if exists {
		s.transcript[i] = u
	} else {
		i = len(s.transcript)
		s.planIndex[s.planID] = i
		s.transcript = append(s.transcript, u)
	}
	s.dirty[i] = true
	s.updatedAt = time.Now()
	s.send(u)
	return s.planID, nil
}

// validatePlan applies ADR 0012's model-input limits, not restrictions on
// historical ACP snapshots, which may contain future values or no entries.
func validatePlan(entries []acp.PlanEntry) error {
	if len(entries) < 1 || len(entries) > 20 {
		return fmt.Errorf("plan must contain 1 to 20 entries")
	}
	active := 0
	for i, entry := range entries {
		if strings.TrimSpace(entry.Content) == "" || utf8.RuneCountInString(entry.Content) > 1000 {
			return fmt.Errorf("plan entry %d must have nonempty content of at most 1000 characters", i+1)
		}
		switch entry.Priority {
		case "high", "medium", "low":
		default:
			return fmt.Errorf("plan entry %d has invalid priority %q", i+1, entry.Priority)
		}
		switch entry.Status {
		case "pending", "completed":
		case "in_progress":
			active++
		default:
			return fmt.Errorf("plan entry %d has invalid status %q", i+1, entry.Status)
		}
	}
	if active > 1 {
		return fmt.Errorf("plan may have at most one in_progress entry")
	}
	return nil
}
