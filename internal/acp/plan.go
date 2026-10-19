package acp

import (
	"encoding/json"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// PlanEntry describes a task's priority and progress independently of the
// session lifecycle. Unknown priority and status strings remain readable.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// PlanItems replaces all entries of the plan identified by PlanID.
// acplause constructs only the items variant.
type PlanItems struct {
	Type    string      `json:"type"`
	PlanID  string      `json:"planId"`
	Entries []PlanEntry `json:"entries"`
}

// PlanUpdate retains a decoded snapshot verbatim for replay, including
// unknown fields and future plan variants. Parsed fields are read-only.
type PlanUpdate struct {
	Plan PlanItems `json:"plan"`
	raw  json.RawMessage
}

func (PlanUpdate) sessionUpdate() string { return "plan_update" }

// MarshalJSON implements json.Marshaler.
func (u PlanUpdate) MarshalJSON() ([]byte, error) {
	if u.raw != nil {
		return u.raw, nil
	}
	type wire PlanUpdate
	return jsonrpc.Marshal(wire(u))
}

// UnmarshalJSON reads entries only for the known variant: a future plan
// may use the same field name with a different shape.
func (u *PlanUpdate) UnmarshalJSON(data []byte) error {
	var fields struct {
		Plan struct {
			Type    string          `json:"type"`
			PlanID  string          `json:"planId"`
			Entries json.RawMessage `json:"entries"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	plan := PlanItems{Type: fields.Plan.Type, PlanID: fields.Plan.PlanID}
	if plan.Type == "items" && fields.Plan.Entries != nil {
		if err := json.Unmarshal(fields.Plan.Entries, &plan.Entries); err != nil {
			return err
		}
	}
	*u = PlanUpdate{Plan: plan, raw: append(json.RawMessage(nil), data...)}
	return nil
}
