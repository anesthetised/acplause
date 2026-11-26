package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "../testdata/schema/v2/schema.json"

// validator checks agent output against the pinned ACP v2 schema: every
// message against the root schema, and each result, error and notification
// params against its specific definition.
type validator struct {
	compiler *jsonschema.Compiler
	root     *jsonschema.Schema
	// Definitions by method: responses of agent methods, params of
	// requests and notifications the agent sends to the client.
	responses map[string]string
	params    map[string]string

	mu       sync.Mutex
	compiled map[string]*jsonschema.Schema
}

var loadValidator = sync.OnceValues(func() (*validator, error) {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	v := &validator{
		compiler:  jsonschema.NewCompiler(),
		responses: map[string]string{},
		params:    map[string]string{},
		compiled:  map[string]*jsonschema.Schema{},
	}
	v.compiler.AssertFormat()
	if err := v.compiler.AddResource("acp.json", doc); err != nil {
		return nil, err
	}
	if v.root, err = v.compiler.Compile("acp.json"); err != nil {
		return nil, err
	}

	var meta struct {
		Defs map[string]struct {
			Side   string `json:"x-side"`
			Method string `json:"x-method"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	for name, def := range meta.Defs {
		switch {
		case def.Method == "":
		case def.Side == "agent" && strings.HasSuffix(name, "Response"):
			v.responses[def.Method] = name
		case (def.Side == "client" || def.Side == "protocol") && (strings.HasSuffix(name, "Notification") || strings.HasSuffix(name, "Request")):
			v.params[def.Method] = name
		}
	}
	return v, nil
})

func (v *validator) def(name string) (*jsonschema.Schema, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.compiled[name]; ok {
		return s, nil
	}
	s, err := v.compiler.Compile("acp.json#/$defs/" + name)
	if err != nil {
		return nil, err
	}
	v.compiled[name] = s
	return s, nil
}

func (v *validator) check(name string, raw json.RawMessage) error {
	s, err := v.def(name)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := s.Validate(inst); err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	return nil
}

// decodeLine validates the entire stdout frame before decoding its envelope.
func (v *validator) decodeLine(line []byte, msg *message) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
	if err != nil {
		return err
	}
	if err := v.root.Validate(inst); err != nil {
		return fmt.Errorf("root schema: %v", err)
	}
	return json.Unmarshal(line, msg)
}

// validatePayload checks the definition for the method a response answers
// or the method of an outgoing request or notification.
func (v *validator) validatePayload(msg *message, method string) error {
	switch {
	case msg.Error != nil:
		raw, _ := json.Marshal(msg.Error)
		return v.check("Error", raw)
	case msg.Result != nil:
		name, ok := v.responses[method]
		if !ok {
			return fmt.Errorf("no schema for the response to %q", method)
		}
		return v.check(name, msg.Result)
	case msg.Method != "":
		name, ok := v.params[msg.Method]
		if !ok {
			return fmt.Errorf("agent sent unknown method %q", msg.Method)
		}
		return v.check(name, msg.Params)
	}
	return fmt.Errorf("unrecognised message")
}

func TestReaderRejectsArrayFrames(t *testing.T) {
	v, err := loadValidator()
	if err != nil {
		t.Fatal(err)
	}
	const valid = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`
	for _, array := range []string{`[]`, `[` + valid + `,` + valid + `]`} {
		a := &agent{out: make(chan *message, 3)}
		err := a.read(strings.NewReader(valid+"\n"+array+"\n"), v)
		if err == nil || !strings.Contains(err.Error(), "line 2: root schema") {
			t.Fatalf("reading %s after a valid frame: %v", array, err)
		}
		if len(a.out) != 1 {
			t.Fatalf("reader delivered %d messages, want only the first frame", len(a.out))
		}
	}
}
