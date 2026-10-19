package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/tools"
)

const terminalInterval = 100 * time.Millisecond
const terminalChunkLimit = 16 * 1024

// executeTool joins the invocation's publication pump before the caller can
// finish the tool. The pipe writer only touches the tools-owned bounded buffer.
func (s *Session) executeTool(ctx context.Context, call *preparedTool, toolEntry int, toolID string) toolOutput {
	var observer *tools.ShellObserver
	if call.observeShell != nil {
		observer = call.observeShell()
	}
	if observer == nil {
		return call.run(ctx)
	}
	p := terminalPump{s: s, toolEntry: toolEntry, toolID: toolID, entry: -1}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(terminalInterval)
		defer ticker.Stop()
		changed := observer.Changed()
		for {
			select {
			case <-changed:
				if p.start(observer.Snapshot(0)) {
					changed = nil
				}
			case <-ticker.C:
				snapshot := observer.Snapshot(p.offset)
				if p.start(snapshot) {
					changed = nil
					p.publish(snapshot)
				}
			case <-stop:
				snapshot := observer.Snapshot(0)
				if p.start(snapshot) {
					p.finish(snapshot)
				}
				return
			}
		}
	}()
	result := call.run(ctx)
	close(stop)
	<-done
	if p.id != "" {
		result.Content = append([]acp.ToolCallContent{acp.ToolTerminal(p.id)}, result.Content...)
	}
	return result
}

// terminalPump owns one transcript position; intermediate chunks never become
// separate rows. All transcript changes and live publications share Session.mu.
type terminalPump struct {
	s         *Session
	toolEntry int
	toolID    string
	id        string
	entry     int
	offset    int
	data      []byte
}

func (p *terminalPump) start(snapshot tools.ShellSnapshot) bool {
	if p.id != "" {
		return true
	}
	if !snapshot.Started {
		return false
	}
	p.id = newID()
	p.s.mu.Lock()
	p.entry = len(p.s.transcript)
	u := acp.TerminalUpdate{TerminalID: p.id, Command: snapshot.Command, Cwd: snapshot.Cwd, Output: &acp.TerminalOutput{}}
	p.s.transcript = append(p.s.transcript, u)
	p.s.dirty[p.entry] = true
	p.s.send(u)
	patch := acp.ToolCallUpdate{ToolCallID: p.toolID, Status: acp.ToolInProgress, Content: []acp.ToolCallContent{acp.ToolTerminal(p.id)}}
	p.s.transcript[p.toolEntry] = p.s.transcript[p.toolEntry].(acp.ToolCallUpdate).Merge(patch)
	p.s.dirty[p.toolEntry] = true
	p.s.send(patch)
	p.s.mu.Unlock()
	// Persist at the tool-start boundary, never for individual chunks. A crash
	// can lose recent output, but recovery can mark this terminal ended.
	p.s.flush()
	return true
}

func (p *terminalPump) publish(snapshot tools.ShellSnapshot) {
	if len(snapshot.Output) == 0 {
		return
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	for len(snapshot.Output) > 0 {
		n := min(len(snapshot.Output), terminalChunkLimit)
		chunk := snapshot.Output[:n]
		p.data = append(p.data, chunk...)
		p.offset += n
		u := p.s.transcript[p.entry].(acp.TerminalUpdate)
		u.Output = &acp.TerminalOutput{Data: base64.StdEncoding.EncodeToString(p.data)}
		p.s.transcript[p.entry] = u
		p.s.dirty[p.entry] = true
		p.s.send(acp.TerminalOutputChunk{TerminalID: p.id, Data: base64.StdEncoding.EncodeToString(chunk)})
		snapshot.Output = snapshot.Output[n:]
	}
}

func (p *terminalPump) finish(snapshot tools.ShellSnapshot) {
	data := snapshot.Output
	if snapshot.Dropped > 0 {
		data = append(data, []byte(fmt.Sprintf("\n... (%d more bytes of output dropped)", snapshot.Dropped))...)
	}
	status := &acp.TerminalExitStatus{}
	if snapshot.Exit != nil {
		status.ExitCode, status.Signal = snapshot.Exit.Code, snapshot.Exit.Signal
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	u := p.s.transcript[p.entry].(acp.TerminalUpdate)
	u.Output = &acp.TerminalOutput{Data: base64.StdEncoding.EncodeToString(data)}
	u.ExitStatus = status
	p.s.transcript[p.entry] = u
	p.s.dirty[p.entry] = true
	p.s.send(u)
}
