package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestFormQuestionToolDescribesConstraints(t *testing.T) {
	var spec struct {
		Parameters struct {
			Properties map[string]json.RawMessage
		}
	}
	encoded, _ := json.Marshal(askUserTool.Function)
	if err := json.Unmarshal(encoded, &spec); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"title", "description", "questions"} {
		if spec.Parameters.Properties[name] == nil {
			t.Errorf("tool schema lacks %s", name)
		}
	}
	for _, name := range []string{"choices", "required", "default", "minLength", "maxLength", "format", "minimum", "maximum", "minItems", "maxItems"} {
		if !strings.Contains(string(encoded), fmt.Sprintf("%q", name)) {
			t.Errorf("tool schema lacks question field %s", name)
		}
	}
	if strings.Contains(string(encoded), `"pattern"`) {
		t.Error("tool schema advertises the unsupported pattern")
	}
}
