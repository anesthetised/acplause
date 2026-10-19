package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/mcp"
)

func TestMCPResultProjection(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		failed            bool
	}{
		{"structured", `{"content":[],"structuredContent":{"taskId":"opaque","n":9007199254740993}}`, `9007199254740993`, false},
		{"mixed image", `{"content":[{"type":"text","text":"snapshot"},{"type":"image","mimeType":"image/png","data":"BASE64_MUST_NOT_ESCAPE"}]}`, "snapshot", false},
		{"image only", `{"content":[{"type":"image","mimeType":"image/png","data":"BASE64_MUST_NOT_ESCAPE"}]}`, "may have completed", true},
		{"unknown", `{"content":[{"type":"_future","data":"BASE64_MUST_NOT_ESCAPE"}]}`, "Unsupported", true},
		{"empty", `{"content":[]}`, "(no output)", false},
		{"error", `{"content":[{"type":"text","text":"declined"}],"isError":true}`, "declined", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var r mcp.Result
			if err := json.Unmarshal([]byte(tt.input), &r); err != nil {
				t.Fatal(err)
			}
			out := projectMCPResult(r)
			if out.Failed != tt.failed || !strings.Contains(out.Output, tt.want) {
				t.Fatalf("projection: %+v", out)
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "BASE64_MUST_NOT_ESCAPE") {
				t.Fatal("binary content leaked")
			}
			if tt.name == "structured" && !strings.Contains(string(out.RawOutput), `"structuredContent":{"taskId":"opaque","n":9007199254740993}`) {
				t.Fatalf("lost structured result: %s", out.RawOutput)
			}
		})
	}
}
