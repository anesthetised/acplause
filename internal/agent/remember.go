package agent

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/anesthetised/acplause/internal/acp"
)

// maxRemembered bounds a session's remembered shell decisions (ADR 0029).
const maxRemembered = 256

// Option ids of the remembering choices, offered for bash only.
const (
	optionAllowAlways  = "allow_always"
	optionRejectAlways = "reject_always"
)

var shellPermissionOptions = []acp.PermissionOption{
	{OptionID: optionAllow, Name: "Allow", Kind: acp.PermissionAllowOnce},
	{OptionID: optionAllowAlways, Name: "Always allow this command", Kind: acp.PermissionAllowAlways},
	{OptionID: optionReject, Name: "Reject", Kind: acp.PermissionRejectOnce},
	{OptionID: optionRejectAlways, Name: "Always reject this command", Kind: acp.PermissionRejectAlways},
}

// rememberKey is the exact command plus the session's absolute cwd: no
// normalization, so a different spelling is a different command.
func (s *Session) rememberKey(command string) string { return s.cwd + "\x00" + command }

// remembered reports a stored decision for command, if any.
func (s *Session) remembered(command string) (allow, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allow, ok = s.shellDecisions[s.rememberKey(command)]
	return
}

// canRemember reports whether another decision fits; the "always" options are
// withheld once the session holds maxRemembered of them.
func (s *Session) canRemember() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.shellDecisions) < maxRemembered
}

func (s *Session) remember(command string, allow bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.rememberKey(command)
	if _, replacing := s.shellDecisions[key]; !replacing && len(s.shellDecisions) >= maxRemembered {
		return
	}
	if s.shellDecisions == nil {
		s.shellDecisions = map[string]bool{}
	}
	s.shellDecisions[key] = allow
}

// logRemembered records that memory decided, never the command text.
func (s *Session) logRemembered(command string, allow bool) {
	sum := sha256.Sum256([]byte(s.rememberKey(command)))
	s.deps.log.Debug("remembered shell decision used", "session", s.id, "key", hex.EncodeToString(sum[:6]), "allow", allow)
}
