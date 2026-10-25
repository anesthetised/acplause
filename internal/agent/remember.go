package agent

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"

	"github.com/anesthetised/acplause/internal/acp"
)

// maxRemembered bounds a session's remembered shell decisions (ADR 0029).
const maxRemembered = 256

// Option ids of the remembering choices, offered for built-in shells only.
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

// canRemember reports whether another decision fits, and the roots generation
// the request is made under; the "always" options are withheld once the
// session holds maxRemembered of them.
func (s *Session) canRemember() (ok bool, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.shellDecisions) < maxRemembered, s.rootsGen
}

// remember stores a decision answered under roots generation gen; one answered
// before a root change is dropped, as ADR 0029 clears decisions with the roots.
func (s *Session) remember(command string, allow bool, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.rememberKey(command)
	if gen != s.rootsGen {
		return
	}
	if _, replacing := s.shellDecisions[key]; !replacing && len(s.shellDecisions) >= maxRemembered {
		return
	}
	if s.shellDecisions == nil {
		s.shellDecisions = map[string]bool{}
	}
	s.shellDecisions[key] = allow
}

// logKey keys the hashes in log lines. It exists only in this process, so the
// hash correlates repeated uses of one command but a dictionary of common
// commands cannot be hashed offline to recover the text from a log.
var logKey = func() []byte {
	b := make([]byte, 32)
	rand.Read(b) // never returns an error since Go 1.24
	return b
}()

// logRemembered records that memory decided, never the command text.
func (s *Session) logRemembered(command string, allow bool) {
	mac := hmac.New(sha256.New, logKey)
	mac.Write([]byte(s.rememberKey(command)))
	s.deps.log.Debug("remembered shell decision used", "session", s.id, "key", hex.EncodeToString(mac.Sum(nil)[:6]), "allow", allow)
}
