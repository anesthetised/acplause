package agent

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/media"
)

func promptUsage(tokens uint64) *llm.Usage {
	return &llm.Usage{PromptTokens: tokens, HasPromptTokens: true}
}

func textRequest(model string, size int) llm.Request {
	return llm.Request{Model: model, Messages: []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("x", size)}}}
}

func TestCalibratedEstimate(t *testing.T) {
	c := new(calibration)
	req := textRequest("m", 40000)
	bytes := requestSize(req)
	if got := c.size(req); got != bytes {
		t.Fatalf("before any usage: %d, want one token per byte (%d)", got, bytes)
	}

	c.observe(req, promptUsage(bytes/4))
	want := float64(bytes) / (float64(bytes) / float64(bytes/4)) * calibrationMargin
	if got := c.size(req); float64(got) < want || float64(got) > want+1 {
		t.Fatalf("after one response: %d, want %.1f", got, want)
	}
	if got := c.size(textRequest("other", 40000)); got != bytes {
		t.Fatalf("another model used this calibration: %d", got)
	}

	// Only a smaller ratio replaces the observation.
	calibrated := c.size(req)
	c.observe(req, promptUsage(bytes/8))
	if got := c.size(req); got != calibrated {
		t.Fatalf("a less conservative ratio was kept: %d, want %d", got, calibrated)
	}
	c.observe(req, promptUsage(bytes/2))
	if got := c.size(req); got <= calibrated {
		t.Fatalf("a more conservative ratio was ignored: %d", got)
	}

	// Unusable usage leaves the calibration unchanged.
	before := c.size(req)
	c.observe(req, nil)
	c.observe(req, &llm.Usage{PromptTokens: 1})
	c.observe(req, promptUsage(0))
	if got := c.size(req); got != before {
		t.Fatalf("missing or zero prompt tokens calibrated: %d, want %d", got, before)
	}
	var none *calibration
	none.observe(req, promptUsage(1))
	if got := none.size(req); got != bytes {
		t.Fatalf("nil calibration: %d", got)
	}
}

func TestCalibrationIgnoresImageRequests(t *testing.T) {
	img := testImage(t, 2, 2)
	c := new(calibration)
	text := textRequest("m", 40000)
	withImage := text
	withImage.Vision = true
	withImage.Messages = []llm.Message{text.Messages[0], {Role: llm.RoleTool, ToolCallID: "a", Images: []media.Image{img}}}
	c.observe(withImage, promptUsage(10))
	if got, want := c.size(withImage), requestSize(withImage); got != want {
		t.Fatalf("an image request calibrated: %d, want %d", got, want)
	}

	// After a text calibration the image reserve stays in fixed tokens.
	c.observe(text, promptUsage(requestSize(text)/4))
	textOnly, images := requestParts(withImage)
	ratio := float64(requestSize(text)) / float64(requestSize(text)/4)
	want := uint64(math.Ceil(float64(textOnly)/ratio*calibrationMargin)) + images
	if images != imageLabelBytes+imageTokens {
		t.Fatalf("image reserve: %d", images)
	}
	if got := c.size(withImage); got+1 < want || got > want+1 {
		t.Fatalf("image request estimate: %d, want %d", got, want)
	}
}

func TestCalibrationMarginNeverShrinks(t *testing.T) {
	c := new(calibration)
	for _, n := range []int{1, 7, 999, 40000} {
		req := textRequest("m", n)
		c.observe(req, promptUsage(max(1, requestSize(req)/3)))
	}
	for _, n := range []int{0, 1, 12345, 100000} {
		req := textRequest("m", n)
		text, _ := requestParts(req)
		ratio := c.ratios["m"]
		if got := c.size(req); float64(got) < float64(text)/ratio*calibrationMargin {
			t.Fatalf("estimate %d for %d bytes is below the margin (ratio %.3f)", got, text, ratio)
		}
	}
}

// TestCalibrationAvoidsCompaction checks the session path: once usage shows a
// realistic ratio, a history that the byte estimate compacts fits, and a
// model change starts again from the byte estimate.
func TestCalibrationAvoidsCompaction(t *testing.T) {
	summaries := 0
	s, _ := compactionSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		summaries++
		return &llm.Completion{Content: "summary", FinishReason: llm.FinishStop}, nil
	}))
	s.deps.calibration = new(calibration)
	s.catalog = newCatalog([]llm.Model{windowModel("m", 16000, 16000), windowModel("other", 16000, 16000)})
	s.history = append(s.history,
		llm.Message{Role: llm.RoleUser, Content: strings.Repeat("old request ", 600)},
		llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("old answer ", 600)},
		llm.Message{Role: llm.RoleUser, Content: "continue"})
	sent := s.request().Request
	if requestSize(sent) <= 12000 {
		t.Fatalf("fixture fits without calibration: %d", requestSize(sent))
	}
	s.reportUsage(sent, promptUsage(requestSize(sent)/4))
	remaining := 5
	req, err := s.prepareRequest(context.Background(), &remaining)
	if err != nil || summaries != 0 || len(req.Messages) != len(s.history) {
		t.Fatalf("calibrated request compacted: summaries=%d, err=%v", summaries, err)
	}

	if err := s.SetConfigOption(configModel, acp.ConfigValueID, json.RawMessage(`"other"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareRequest(context.Background(), &remaining); err != nil || summaries == 0 {
		t.Fatalf("another model inherited the calibration: summaries=%d, err=%v", summaries, err)
	}
}

func TestMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name            string
		window, ceiling uint64
		want            uint64
	}{
		{"unknown window and ceiling", 0, 0, 0},
		{"unknown window", 0, 4096, 0},
		{"unknown ceiling", 128000, 0, 0},
		{"quarter below ceiling", 128000, 64000, 32000},
		{"ceiling below quarter", 200000, 16384, 16384},
		{"above the former fixed cap", 400000, 128000, 100000},
		{"tiny window", 3, 100, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxTokens(tc.window, tc.ceiling); got != tc.want {
				t.Fatalf("maxTokens(%d, %d) = %d, want %d", tc.window, tc.ceiling, got, tc.want)
			}
		})
	}
}

func TestCalibrationRejectsImplausibleRatios(t *testing.T) {
	req := textRequest("m", 40000)
	bytes := requestSize(req)

	// An undercount above six bytes per token would shrink the estimate
	// enough to overflow the window; it is not recorded.
	c := new(calibration)
	c.observe(req, promptUsage(bytes/7))
	if got := c.size(req); got != bytes {
		t.Fatalf("implausible ratio used: %d, want %d", got, bytes)
	}

	// An absurd overcount must not make requests larger than a byte per token.
	c.observe(req, promptUsage(math.MaxUint64))
	if got := c.size(req); got != bytes {
		t.Fatalf("tiny ratio inflated the estimate: %d, want %d", got, bytes)
	}
	c.observe(req, promptUsage(bytes/4))
	if got := c.size(req); got != bytes {
		t.Fatalf("a later observation replaced the smaller ratio: %d", got)
	}
}
