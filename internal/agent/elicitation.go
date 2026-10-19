package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

var askUserTool = llm.FunctionTool("ask_user", "Ask the user one to three questions when you need input to continue. Use options for a single choice, or omit options for text. This requests information, never permission to perform an action.", json.RawMessage(`{
 "type":"object","properties":{
  "message":{"type":"string","minLength":1,"maxLength":2000},
  "questions":{"type":"array","minItems":1,"maxItems":3,"items":{
   "type":"object","properties":{
    "id":{"type":"string","minLength":1,"maxLength":64},
    "title":{"type":"string","minLength":1,"maxLength":120},
    "question":{"type":"string","minLength":1,"maxLength":2000},
    "options":{"type":"array","maxItems":10,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":200}}
   },"required":["id","title","question"]
  }}
 },"required":["message","questions"]
}`))

func (s *Session) supportsForms() bool {
	return s.deps.forms != nil && s.deps.forms.Load()
}

// parseQuestions accepts only the bounded string/enum surface from ADR 0011.
// Unknown fields remain compatible; the model cannot supply arbitrary schemas.
func parseQuestions(args string) (acp.ElicitationRequest, error) {
	var input struct {
		Message   string `json:"message"`
		Questions []struct {
			ID       string   `json:"id"`
			Title    string   `json:"title"`
			Question string   `json:"question"`
			Options  []string `json:"options"`
		} `json:"questions"`
	}
	req := acp.ElicitationRequest{Mode: "form", RequestedSchema: acp.ElicitationSchema{Type: "object", Properties: map[string]acp.ElicitationStringProperty{}}}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return req, fmt.Errorf("invalid ask_user arguments: %w", err)
	}
	bounded := func(s string, max int) bool { return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max }
	if !bounded(input.Message, 2000) || len(input.Questions) < 1 || len(input.Questions) > 3 {
		return req, errors.New("ask_user requires a message (1–2000 characters) and one to three questions")
	}
	req.Message = input.Message
	for _, q := range input.Questions {
		if !bounded(q.ID, 64) || !bounded(q.Title, 120) || !bounded(q.Question, 2000) || len(q.Options) > 10 {
			return req, errors.New("invalid ask_user question: ID/title/question/options exceed their bounds or are empty")
		}
		if _, exists := req.RequestedSchema.Properties[q.ID]; exists {
			return req, fmt.Errorf("duplicate question ID %q", q.ID)
		}
		seen := map[string]bool{}
		for _, option := range q.Options {
			if !bounded(option, 200) || seen[option] {
				return req, fmt.Errorf("question %q has an empty, duplicate or oversized option", q.ID)
			}
			seen[option] = true
		}
		req.RequestedSchema.Properties[q.ID] = acp.ElicitationStringProperty{Type: "string", Title: q.Title, Description: q.Question, Enum: q.Options}
		req.RequestedSchema.Required = append(req.RequestedSchema.Required, q.ID)
	}
	return req, nil
}

func questionResult(req acp.ElicitationRequest, resp acp.ElicitationResponse) toolOutput {
	fail := func(message string) toolOutput { return toolOutput{Output: "Error: " + message, Failed: true} }
	if resp.Action != "accept" {
		return fail(fmt.Sprintf("user input was not accepted (action %q)", resp.Action))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Content, &raw); err != nil || len(raw) != len(req.RequestedSchema.Properties) {
		return fail("answers must contain exactly the requested fields")
	}
	answers := map[string]string{}
	for id, property := range req.RequestedSchema.Properties {
		value, ok := raw[id]
		var answer *string // JSON null must not become an empty string
		if !ok || json.Unmarshal(value, &answer) != nil || answer == nil {
			return fail(fmt.Sprintf("answer %q must be a string", id))
		}
		if len(property.Enum) > 0 && !slices.Contains(property.Enum, *answer) {
			return fail(fmt.Sprintf("answer %q is not an offered option", id))
		}
		answers[id] = *answer
	}
	encoded, _ := jsonrpc.Marshal(answers)
	return toolOutput{Output: string(encoded), RawOutput: encoded}
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
	return questionResult(req, resp)
}
