package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/media"
)

func testImage(t *testing.T, w int) media.Image {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, 1))); err != nil {
		t.Fatal(err)
	}
	img, err := media.New(media.PNG, base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func wireJSON(t *testing.T, msgs []Message, vision bool) string {
	t.Helper()
	b, err := json.Marshal(wireMessages(msgs, vision))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWireTextOnlyKeepsShape(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}, Reasoning: "r"},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
	}
	for _, vision := range []bool{false, true} {
		want, _ := json.Marshal(msgs)
		if got := wireJSON(t, msgs, vision); got != string(want) {
			t.Fatalf("vision=%v:\n got %s\nwant %s", vision, got, want)
		}
	}
}

func TestWireToolImagesFollowAllSiblingResults(t *testing.T) {
	a, b := testImage(t, 1), testImage(t, 2)
	msgs := []Message{
		{Role: RoleUser, Content: "look"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}}},
		{Role: RoleTool, ToolCallID: "c1", Content: "shot", Images: []media.Image{a}},
		{Role: RoleTool, ToolCallID: "c2", Content: "plain"},
		{Role: RoleTool, ToolCallID: "c3", Images: []media.Image{a, b}}, // image only
		{Role: RoleAssistant, Content: "done"},
	}
	var got []struct {
		Role       string
		ToolCallID string `json:"tool_call_id"`
		Content    any
	}
	if err := json.Unmarshal([]byte(wireJSON(t, msgs, true)), &got); err != nil {
		t.Fatal(err)
	}
	roles := ""
	for _, m := range got {
		roles += m.Role[:1]
	}
	if roles != "uattt"+"u"+"a" {
		t.Fatalf("roles %q", roles)
	}
	for i, id := range []string{"c1", "c2", "c3"} {
		if got[2+i].ToolCallID != id {
			t.Fatalf("tool result %d has id %q", i, got[2+i].ToolCallID)
		}
	}
	if got[4].Content != nil {
		t.Fatalf("image-only tool result keeps no text: %v", got[4].Content)
	}
	parts := got[5].Content.([]any)
	var kinds []string
	var labels []string
	for _, p := range parts {
		m := p.(map[string]any)
		kinds = append(kinds, m["type"].(string))
		if s, ok := m["text"].(string); ok {
			labels = append(labels, s)
		}
	}
	if strings.Join(kinds, ",") != "text,image_url,text,image_url,image_url" {
		t.Fatalf("parts %v", kinds)
	}
	if !strings.Contains(labels[0], "c1") || !strings.Contains(labels[1], "c3") || !strings.Contains(labels[0], "untrusted") {
		t.Fatalf("labels %q", labels)
	}
}

func TestWireUserImages(t *testing.T) {
	img := testImage(t, 1)
	got := wireJSON(t, []Message{{Role: RoleUser, Content: "see", Images: []media.Image{img}}, {Role: RoleUser, Images: []media.Image{img}}}, true)
	want := `[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"` + img.DataURL() + `"}}]},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + img.DataURL() + `"}}]}]`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestWireWithoutVisionOmitsImages(t *testing.T) {
	img := testImage(t, 1)
	msgs := []Message{
		{Role: RoleUser, Content: "see", Images: []media.Image{img}},
		{Role: RoleTool, ToolCallID: "c1", Images: []media.Image{img}},
	}
	got := wireJSON(t, msgs, false)
	if strings.Contains(got, "base64") || strings.Contains(got, "image_url") || strings.Count(got, imagesOmitted) != 2 {
		t.Fatalf("got %s", got)
	}
}

func TestWireDoesNotMutateHistory(t *testing.T) {
	img := testImage(t, 1)
	msgs := []Message{{Role: RoleTool, ToolCallID: "c1", Content: "x", Images: []media.Image{img}}}
	before, _ := json.Marshal(msgs)
	wireMessages(msgs, true)
	wireMessages(msgs, false)
	if after, _ := json.Marshal(msgs); !bytes.Equal(before, after) || len(msgs) != 1 {
		t.Fatalf("history changed: %s", after)
	}
}

func TestMessageStorageCompatibility(t *testing.T) {
	var old Message
	if err := json.Unmarshal([]byte(`{"role":"tool","content":"ok","tool_call_id":"c1"}`), &old); err != nil || old.Images != nil {
		t.Fatalf("old row: %+v %v", old, err)
	}
	b, _ := json.Marshal(old)
	if strings.Contains(string(b), "images") {
		t.Fatalf("empty images persisted: %s", b)
	}
	m := Message{Role: RoleTool, ToolCallID: "c1", Images: []media.Image{testImage(t, 1)}}
	b, _ = json.Marshal(m)
	var back Message
	if err := json.Unmarshal(b, &back); err != nil || len(back.Images) != 1 || back.Images[0].DataURL() != m.Images[0].DataURL() {
		t.Fatalf("new row: %v", err)
	}
}

func TestModelVision(t *testing.T) {
	var models []Model
	body := `[{"id":"a","architecture":{"input_modalities":["text","image"]}},{"id":"b","architecture":{"input_modalities":["text"]}},{"id":"c"}]`
	if err := json.Unmarshal([]byte(body), &models); err != nil {
		t.Fatal(err)
	}
	if !models[0].Vision() || models[1].Vision() || models[2].Vision() {
		t.Fatal("only an explicit image modality means vision")
	}
	if !models[2].Supports("tools") {
		t.Fatal("tool support filtering changed")
	}
}

func TestChatSendsImagesOnlyInImageURLParts(t *testing.T) {
	img := testImage(t, 1)
	var raw string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		raw = buf.String()
		sse(w, data(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), data(`[DONE]`))
	})
	req := Request{Model: "m", Vision: true, Messages: []Message{{Role: RoleTool, ToolCallID: "c1", Content: "t", Images: []media.Image{img}}}}
	if _, err := c.Chat(t.Context(), req, func(Delta) {}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, `"images"`) || strings.Contains(raw, "mimeType") || strings.Count(raw, img.DataURL()) != 1 {
		t.Fatalf("request body leaks internal fields: %s", raw)
	}
}
