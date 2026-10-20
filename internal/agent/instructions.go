package agent

import "context"

// refreshInstructions runs only on the session worker. Pinning roots keeps a
// concurrent resume from closing the read's descriptors; comparing generations
// prevents an old read from publishing after that resume. No filesystem work
// happens under mu, and a cancelled read never changes the system message.
func (s *Session) refreshInstructions(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tb := s.acquireToolbox()
		text, err := tb.Instructions(ctx)
		if err != nil && ctx.Err() == nil {
			s.deps.log.Warn("skipping AGENTS.md", "cwd", s.cwd, "err", err)
			text = ""
		}
		s.mu.Lock()
		current := tb == s.toolbox
		cancelled := ctx.Err()
		if current && cancelled == nil {
			s.instructions = text
			s.history[0].Content = systemPrompt(s.cwd, s.dirs, text, s.mode)
		}
		s.mu.Unlock()
		s.releaseToolbox(tb)
		if cancelled != nil {
			return cancelled
		}
		if current {
			return nil
		}
	}
}
