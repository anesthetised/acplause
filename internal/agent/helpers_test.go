package agent

import "encoding/json"

// drop forgets a session another process now owns, as lease renewal does.
func (a *Agent) drop(id string) {
	a.ownershipMu.Lock()
	defer a.ownershipMu.Unlock()
	a.mu.Lock()
	pending := a.operations[id]
	a.mu.Unlock()
	a.dropLocked(id, pending)
}

// SetConfigOption changes a setting without the protocol handler's setup fence.
func (s *Session) SetConfigOption(id, valueType string, raw json.RawMessage) error {
	return s.setConfigOption(id, valueType, raw, false)
}
