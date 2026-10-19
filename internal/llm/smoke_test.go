//go:build smoke

package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/media"
)

// TestSmokeVision sends a tool-result screenshot through the real API and
// expects the model to name its colour. Run with OPENROUTER_API_KEY set:
//
//	go test -tags smoke -run TestSmokeVision -v ./internal/llm
//
// SMOKE_MODEL overrides the cheapest known-vision tool-capable model.
func TestSmokeVision(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	c := New(Config{APIKey: key})

	modelID := os.Getenv("SMOKE_MODEL")
	if modelID == "" {
		modelID = cheapestVision(t, key)
	}
	t.Logf("model: %s", modelID)

	pic := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			pic.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, pic); err != nil {
		t.Fatal(err)
	}
	img, err := media.New(media.PNG, base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	call := ToolCall{ID: "call_1", Type: "function", Function: FunctionCall{Name: "screenshot", Arguments: "{}"}}
	comp, err := c.Chat(t.Context(), Request{
		Model:     modelID,
		Vision:    true,
		MaxTokens: 200,
		Messages: []Message{
			{Role: RoleUser, Content: "Take a screenshot, then tell me its single dominant colour in one word."},
			{Role: RoleAssistant, ToolCalls: []ToolCall{call}},
			{Role: RoleTool, ToolCallID: call.ID, Content: "screenshot captured", Images: []media.Image{img}},
		},
	}, func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("answer: %q", comp.Content)
	if !strings.Contains(strings.ToLower(comp.Content), "red") {
		t.Fatalf("model did not see the tool image: %q", comp.Content)
	}
}

// cheapestVision lists models and picks the cheapest paid one that advertises
// image input and tools, which also exercises Model.Vision on live metadata.
func cheapestVision(t *testing.T, key string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, DefaultBaseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			Model
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	best, bestCost := "", 0.0
	for _, m := range list.Data {
		p, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)
		o, _ := strconv.ParseFloat(m.Pricing.Completion, 64)
		cost := p + o
		if !m.Vision() || !slices.Contains(m.SupportedParameters, "tools") || cost <= 0 || strings.HasSuffix(m.ID, ":free") {
			continue
		}
		if best == "" || cost < bestCost {
			best, bestCost = m.ID, cost
		}
	}
	if best == "" {
		t.Fatal("no vision model with tools found")
	}
	return best
}
