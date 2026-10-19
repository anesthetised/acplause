package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/mcp"
	"github.com/anesthetised/acplause/internal/media"
)

func TestMCPImageProjection(t *testing.T) {
	img := imageBlock(t, testImage(t, 3, 2))
	raw, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	for _, mixed := range []bool{false, true} {
		for _, isError := range []bool{false, true} {
			r := mcp.Result{Content: []json.RawMessage{raw}, IsError: isError}
			if mixed {
				r.Content = append(r.Content, json.RawMessage(`{"type":"text","text":"DOM snapshot"}`))
				r.StructuredContent = json.RawMessage(`{"taskId":"opaque","n":9007199254740993}`)
			}
			out := projectMCPResult(r, true)
			if out.Failed != isError || len(out.Content) != 2 {
				t.Fatalf("mixed=%v isError=%v: missing image content, failed=%v parts=%d", mixed, isError, out.Failed, len(out.Content))
			}
			if len(out.Images) != 1 || out.Images[0].DataURL() != "data:"+img.MimeType+";base64,"+img.Data {
				t.Fatal("tool history attachment differs from ACP image")
			}
			image := out.Content[1]
			if image.Type != "content" || image.Content.Type != acp.ContentImage || image.Content.Data != img.Data || image.Content.MimeType != img.MimeType {
				t.Fatal("ACP image lost its pixels or MIME type")
			}
			if !strings.Contains(out.Output, "3x2") || strings.Contains(out.Output, img.Data) || strings.Contains(string(out.RawOutput), img.Data) || strings.Contains(string(out.RawOutput), "_meta") {
				t.Fatal("text/rawOutput must describe images without copying bytes or metadata")
			}
			if mixed && (!strings.Contains(out.Output, "DOM snapshot") || !strings.Contains(string(out.RawOutput), `9007199254740993`)) {
				t.Fatal("image projection lost text or opaque structured output")
			}
		}
	}
}

func TestMCPImageBoundsAndUnsupportedModel(t *testing.T) {
	valid := imageBlock(t, testImage(t, 3, 2))
	big, err := bigImage()
	if err != nil {
		t.Fatal(err)
	}
	large := imageBlock(t, big)
	invalid := acp.ContentBlock{Type: acp.ContentImage, MimeType: media.PNG, Data: "BASE64_MUST_NOT_ESCAPE"}
	for _, tc := range []struct {
		name   string
		blocks []acp.ContentBlock
		vision bool
		images int
		marker string
	}{
		{"four", slices.Repeat([]acp.ContentBlock{valid}, 4), true, 4, ""},
		{"count", slices.Repeat([]acp.ContentBlock{valid}, 5), true, 4, "count exceeds"},
		{"invalids consume count", append(slices.Repeat([]acp.ContentBlock{invalid}, 4), valid), true, 0, "count exceeds"},
		{"set bytes", []acp.ContentBlock{large, large, large}, true, 2, "total size"},
		{"text model", []acp.ContentBlock{valid}, false, 0, "does not advertise"},
		{"bad base64", []acp.ContentBlock{invalid}, true, 0, "not valid base64"},
		{"wrong MIME", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: media.JPEG, Data: valid.Data}}, true, 0, "declared"},
		{"unsupported", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: "image/webp", Data: valid.Data}}, true, 0, "unsupported image type"},
		{"oversized", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: media.PNG, Data: strings.Repeat("A", media.MaxImageBytes*2)}}, true, 0, "bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var content []json.RawMessage
			for _, b := range tc.blocks {
				raw, err := json.Marshal(b)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, raw)
			}
			out := projectMCPResult(mcp.Result{Content: content}, tc.vision)
			if len(out.Images) != tc.images || out.Failed != (tc.images == 0) || !strings.Contains(out.Output, tc.marker) {
				t.Fatalf("images=%d failed=%v output=%q", len(out.Images), out.Failed, out.Output)
			}
			if tc.images == 0 && !strings.Contains(out.Output, "may have completed") {
				t.Fatal("image-only failure must warn against automatic repetition")
			}
			if len(out.Content) != tc.images+1 || media.CheckSet(out.Images) != nil || strings.Contains(out.Output, valid.Data) || strings.Contains(string(out.RawOutput), "BASE64_MUST_NOT_ESCAPE") {
				t.Fatal("image bounds or sanitized output violated")
			}
			content = append(content, json.RawMessage(`{"type":"text","text":"retained text"}`))
			out = projectMCPResult(mcp.Result{Content: content}, tc.vision)
			if out.Failed || !strings.Contains(out.Output, "retained text") {
				t.Fatal("an unusable image hid a usable textual result")
			}
		})
	}
	// A wrongly typed known image payload is also an omission, not a lost
	// remote result or an invalid protocol response.
	out := projectMCPResult(mcp.Result{Content: []json.RawMessage{json.RawMessage(`{"type":"image","mimeType":"image/png","data":42}`)}}, true)
	if !out.Failed || !strings.Contains(out.Output, "invalid image data field") {
		t.Fatal("invalid image field was accepted")
	}
}

func TestMCPResultProjection(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		failed            bool
	}{
		{"structured", `{"content":[],"structuredContent":{"taskId":"opaque","n":9007199254740993}}`, `9007199254740993`, false},
		{"mixed image", `{"content":[{"type":"text","text":"snapshot"},{"type":"image","mimeType":"image/png","data":"BASE64_MUST_NOT_ESCAPE"}]}`, "snapshot", false},
		{"image only", `{"content":[{"type":"image","mimeType":"image/png","data":"BASE64_MUST_NOT_ESCAPE"}]}`, "may have completed", true},
		{"audio", `{"content":[{"type":"audio","mimeType":"audio/wav","data":"BASE64_MUST_NOT_ESCAPE"}]}`, "may have completed", true},
		{"blob", `{"content":[{"type":"resource","resource":{"uri":"test:blob","blob":"BASE64_MUST_NOT_ESCAPE"}}]}`, "blob resource omitted", true},
		{"text resource", `{"content":[{"type":"resource","resource":{"uri":"test:text","text":"embedded text"}}]}`, "embedded text", false},
		{"unknown", `{"content":[{"type":"_future","data":"BASE64_MUST_NOT_ESCAPE"}]}`, "Unsupported", true},
		{"empty", `{"content":[]}`, "(no output)", false},
		{"error", `{"content":[{"type":"text","text":"declined"}],"isError":true}`, "declined", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var r mcp.Result
			if err := json.Unmarshal([]byte(tt.input), &r); err != nil {
				t.Fatal(err)
			}
			out := projectMCPResult(r, false)
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

func TestMCPResultProjectionLimit(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", 256<<10+1), strings.Repeat(`"`, 128<<10)} {
		raw, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		out := projectMCPResult(mcp.Result{Content: []json.RawMessage{raw}}, false)
		if !out.Failed || !strings.Contains(out.Output, "exceeds 256 KiB") || len(out.RawOutput) > 256<<10 {
			t.Fatal("oversized projection was not rejected")
		}
	}
}

func TestMCPProjectionDoesNotDuplicateStructuredJSON(t *testing.T) {
	for _, text := range []string{`{"taskId":"opaque","n":9007199254740993}`, "{\n  \"taskId\": \"opaque\", \"n\": 9007199254740993\n}"} {
		raw, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		out := projectMCPResult(mcp.Result{Content: []json.RawMessage{raw}, StructuredContent: json.RawMessage(`{"taskId":"opaque","n":9007199254740993}`)}, false)
		if out.Failed || strings.Count(out.Output, "taskId") != 1 || strings.Count(string(out.RawOutput), "taskId") != 1 {
			t.Fatalf("duplicate JSON projection: %+v", out)
		}
	}
	raw := json.RawMessage(`{"type":"text","text":"Task scheduled"}`)
	out := projectMCPResult(mcp.Result{Content: []json.RawMessage{raw}, StructuredContent: json.RawMessage(`{"taskId":"opaque"}`)}, false)
	if !strings.Contains(out.Output, "Task scheduled") || !strings.Contains(out.Output, "opaque") {
		t.Fatal("human text hid structured data")
	}
}
