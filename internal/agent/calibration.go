package agent

import (
	"math"
	"sync"

	"github.com/anesthetised/acplause/internal/llm"
)

// calibrationMargin is added to every calibrated estimate (ADR 0031). An
// oversized request fails the turn, so the estimate must stay on the safe side.
const calibrationMargin = 1.2

// maxCalibrationRatio rejects implausible observations (ADR 0031). Code, JSON
// and prose run at about 3–4.5 bytes per token, so a higher ratio means the
// provider undercounted, for example cached tokens, and trusting it would
// overflow the window.
const maxCalibrationRatio = 6

// calibration learns how many serialized request bytes each model counts as
// one prompt token (ADR 0031). The agent owns one for the whole process;
// sessions share it and nothing is persisted. The zero value and nil are
// ready to use; nil never calibrates.
type calibration struct {
	mu     sync.Mutex
	ratios map[string]float64 // by model, bytes per prompt token
}

// observe records the bytes of a sent request against the prompt tokens the
// provider counted for it. Only the smallest ratio is kept, which is the most
// conservative one. Requests carrying images are ignored: their reserve is a
// fixed heuristic (ADR 0009) and says nothing about text.
func (c *calibration) observe(req llm.Request, u *llm.Usage) {
	if c == nil || u == nil || !u.HasPromptTokens || u.PromptTokens == 0 {
		return
	}
	for _, m := range req.Messages {
		if len(m.Images) > 0 {
			return
		}
	}
	text, _ := requestParts(req)
	if text == math.MaxUint64 {
		return
	}
	ratio := float64(text) / float64(u.PromptTokens)
	if ratio > maxCalibrationRatio {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.ratios[req.Model]; ok && old <= ratio {
		return
	}
	if c.ratios == nil {
		c.ratios = map[string]float64{}
	}
	c.ratios[req.Model] = ratio
}

// size estimates the prompt tokens of req. Before any observation of its
// model, a byte counts as a token (ADR 0006); afterwards text bytes are
// divided by the ratio and the margin is added, but never above a byte per
// token, so a bogus tiny ratio cannot make every request oversized. Image
// reserves are already tokens and are not scaled.
func (c *calibration) size(req llm.Request) uint64 {
	text, images := requestParts(req)
	return c.estimate(req.Model, text, images)
}

// estimate applies size's rule to the parts of an already measured request.
// Callers that keep the parts incrementally (requestSizer) use it directly, so
// both paths share the ratio, margin and cap.
func (c *calibration) estimate(model string, text, images uint64) uint64 {
	if text == math.MaxUint64 {
		return text
	}
	if c != nil {
		c.mu.Lock()
		ratio := c.ratios[model]
		c.mu.Unlock()
		if est := math.Ceil(float64(text) / ratio * calibrationMargin); ratio > 0 && est < float64(text) {
			text = uint64(est)
		}
	}
	return text + images
}
