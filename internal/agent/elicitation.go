package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/forms"
	"github.com/anesthetised/acplause/internal/llm"
)

var askUserTool = llm.FunctionTool("ask_user", "Ask the user one to three questions when you need input to continue. Set type to string (default), integer (signed 64-bit), number (finite floating-point), or boolean. For string questions, use options for a single choice, or omit options for text. Set multiSelect:true with string options to allow multiple choices, including an empty selection. Other types cannot use choices or multiselect. This requests information, never permission to perform an action. Never request secrets or credentials, including passwords, API keys, tokens, private keys, recovery codes or payment credentials. If credentials are needed, ask the user to configure them outside this conversation and report only whether setup is complete. Do not ask them to paste secrets into chat.", json.RawMessage(`{
 "type":"object","properties":{
  "message":{"type":"string","minLength":1,"maxLength":2000},
  "title":{"type":"string","maxLength":120},"description":{"type":"string","maxLength":2000},
  "questions":{"type":"array","minItems":1,"maxItems":3,"items":{
   "type":"object","properties":{
    "id":{"type":"string","minLength":1,"maxLength":64},
    "title":{"type":"string","minLength":1,"maxLength":120},
    "question":{"type":"string","minLength":1,"maxLength":2000},
    "type":{"type":"string","enum":["string","integer","number","boolean"],"description":"Answer type; omission means string."},
    "options":{"type":"array","maxItems":10,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":200}},
    "choices":{"type":"array","maxItems":10,"description":"Titled alternative to options; set one of them.","items":{"type":"object","properties":{
     "value":{"type":"string","minLength":1,"maxLength":200},"title":{"type":"string","minLength":1,"maxLength":120},"description":{"type":"string","maxLength":500}
    },"required":["value","title"]}},
    "multiSelect":{"type":"boolean","description":"Allow zero or more offered options; requires nonempty options or choices."},
    "required":{"type":"boolean","description":"Default true. An optional question may be left unanswered."},
    "default":{"description":"Value pre-filled for the user; must be valid for the question. Never used as an answer."},
    "minLength":{"type":"integer","minimum":0},"maxLength":{"type":"integer","minimum":0},
    "format":{"type":"string","maxLength":64,"description":"String question format: date, date-time, email or uri are validated."},
    "minimum":{"type":"number"},"maximum":{"type":"number"},
    "minItems":{"type":"integer","minimum":0},"maxItems":{"type":"integer","minimum":0}
   },"required":["id","title","question"]
  }}
 },"required":["message","questions"]
}`))

func (s *Session) supportsForms() bool {
	return s.deps.forms != nil && s.deps.forms.Load()
}

// askUser waits only on the session worker. The form is connection-local:
// resume replays the pending tool, but never sends this request a second time.
func (s *Session) askUser(ctx context.Context, req acp.ElicitationRequest) toolOutput {
	s.setState(acp.StateUpdate{State: acp.StateRequiresAction})
	s.flush()
	var resp acp.ElicitationResponse
	err := s.deps.out.CallCancellable(ctx, acp.MethodElicitationCreate, req, &resp)
	if ctx.Err() != nil {
		return toolOutput{Output: "Cancelled by the user.", Failed: true}
	}
	s.setState(acp.StateUpdate{State: acp.StateRunning})
	if err != nil {
		return toolOutput{Output: fmt.Sprintf("Error: could not get user input: %v", err), Failed: true}
	}
	result := forms.QuestionResult(req, resp)
	return toolOutput{Output: result.Output, Failed: result.Failed, RawOutput: result.RawOutput}
}
