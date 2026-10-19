package acp

// TerminalUpdate patches an agent-owned, display-only terminal. Output replaces
// all previously streamed bytes; a nonnil ExitStatus marks completion.
type TerminalUpdate struct {
	TerminalID string              `json:"terminalId"`
	Command    string              `json:"command,omitempty"`
	Cwd        string              `json:"cwd,omitempty"`
	Output     *TerminalOutput     `json:"output,omitempty"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
}

// TerminalOutput is an authoritative replacement of independently base64-encoded bytes.
type TerminalOutput struct {
	Data string `json:"data"`
}

// TerminalExitStatus marks completion even when no exit details are known.
// A pointer retains a successful zero exit code.
type TerminalExitStatus struct {
	ExitCode *uint32 `json:"exitCode,omitempty"`
	Signal   string  `json:"signal,omitempty"`
}

// TerminalOutputChunk appends independently base64-encoded bytes to a terminal.
type TerminalOutputChunk struct {
	TerminalID string `json:"terminalId"`
	Data       string `json:"data"`
}

// ToolTerminal links a tool call to its agent-owned terminal.
func ToolTerminal(id string) ToolCallContent {
	return ToolCallContent{Type: "terminal", TerminalID: id}
}

func (TerminalUpdate) sessionUpdate() string      { return "terminal_update" }
func (TerminalOutputChunk) sessionUpdate() string { return "terminal_output_chunk" }
