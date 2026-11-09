package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/media"
)

// sizingText mixes in everything JSON escapes, multi-byte runes and, now and
// then, bytes that are not UTF-8.
func sizingText(rng *rand.Rand, n int) string {
	runes := []rune("abc def\n\t\"\\<>&{}[]:,é世\u2028 \x7f\x01")
	var b strings.Builder
	for b.Len() < n {
		b.WriteRune(runes[rng.IntN(len(runes))])
	}
	if rng.IntN(6) == 0 {
		b.WriteString("\xff\xfe tail \xe2\x82")
	}
	return b.String()
}

// sizingHistory is a system message followed by random exchanges, some with
// images on user messages and tool results.
func sizingHistory(rng *rand.Rand, images []media.Image) []llm.Message {
	history := []llm.Message{{Role: llm.RoleSystem, Content: sizingText(rng, 100)}}
	for turn := range 1 + rng.IntN(8) {
		user := llm.Message{Role: llm.RoleUser, Content: sizingText(rng, 5+rng.IntN(300))}
		if rng.IntN(5) == 0 {
			user.Images = images[:1+rng.IntN(len(images))]
		}
		history = append(history, user)
		for round := range rng.IntN(4) {
			ids := []string{fmt.Sprintf("c%d-%d-a", turn, round)}
			if rng.IntN(3) == 0 {
				ids = append(ids, fmt.Sprintf("c%d-%d-b", turn, round))
			}
			call := callMessage(ids...)
			if rng.IntN(3) == 0 {
				call.Content = sizingText(rng, 50)
				call.Reasoning = "private " + sizingText(rng, 20)
				call.ReasoningDetails = []llm.ReasoningDetail{{"type": "reasoning.text", "text": sizingText(rng, 20)}}
			}
			history = append(history, call)
			for _, id := range ids {
				result := toolResult(id, sizingText(rng, rng.IntN(3000)))
				if rng.IntN(4) == 0 {
					result.Images = images[:1+rng.IntN(len(images))]
				}
				history = append(history, result)
			}
		}
		if rng.IntN(3) == 0 {
			history = append(history, llm.Message{Role: llm.RoleAssistant, Content: sizingText(rng, rng.IntN(500))})
		}
	}
	return history
}

func sizingImages(t *testing.T) []media.Image {
	return []media.Image{testImage(t, 4, 4), testImage(t, 40, 30), testImage(t, 9, 7)}
}

// sizingCalibration alternates between the byte estimate and calibrations
// that scale, cap and tie.
func sizingCalibration(rng *rand.Rand, model string) *calibration {
	if rng.IntN(3) == 0 {
		return nil
	}
	c := new(calibration)
	c.observe(textRequest(model, 35000), promptUsage(uint64(5000+rng.IntN(30000))))
	return c
}

func requireSameSize(t *testing.T, z *requestSizer, c *calibration, req llm.Request, context string) {
	t.Helper()
	if got, want := z.size(c, req.Model), c.size(req); got != want {
		t.Fatalf("%s: incremental size %d, requestParts-based size %d", context, got, want)
	}
}

// The incremental size must equal the serialize-everything estimate exactly:
// an underestimate could send an oversized request, an overestimate would
// compact needlessly. It covers framing, tools, the constant, separators,
// escaping, the image reserve and messages that do not serialize.
func TestRequestSizerMatchesRequestParts(t *testing.T) {
	images := sizingImages(t)
	for seed := range uint64(300) {
		rng := rand.New(rand.NewPCG(seed, 1))
		req := llm.Request{Model: "m", Vision: rng.IntN(2) == 0, Messages: sizingHistory(rng, images)}
		if rng.IntN(3) > 0 {
			req.Tools = []llm.Tool{llm.FunctionTool("t", "<tool> & \"desc\"", json.RawMessage(`{"type":"object"}`))}
		}
		if rng.IntN(20) == 0 {
			req.Messages = nil
		}
		c := sizingCalibration(rng, "m")
		z := newRequestSizer(req)
		requireSameSize(t, z, c, req, "new")

		// Trim, project and restart in any order. view is the projection the
		// sizer should be measuring.
		view := slices.Clone(req.Messages)
		for step := range 12 {
			if len(req.Messages) == 0 {
				break
			}
			i := rng.IntN(len(req.Messages))
			where := fmt.Sprintf("seed %d step %d", seed, step)
			switch rng.IntN(3) {
			case 0:
				req.Messages[i].Content = sizingText(rng, rng.IntN(200))
				view[i] = req.Messages[i]
				z.set(i, req.Messages[i])
			case 1:
				if len(req.Messages[i].Images) == 0 {
					continue
				}
				view[i] = z.dropImages(i, req.Messages[i], "reason")
				if want := omitImages(req.Messages[i], "reason"); !reflect.DeepEqual(view[i], want) {
					t.Fatalf("%s: dropImages changed the omitted message", where)
				}
			default:
				copy(view, req.Messages)
				z.reset()
			}
			p := req
			p.Messages = view
			requireSameSize(t, z, c, p, where)
		}
	}
}

func TestRequestSizerUnserializable(t *testing.T) {
	bad := llm.Message{Role: llm.RoleAssistant, ReasoningDetails: []llm.ReasoningDetail{{"x": make(chan int)}}}
	req := llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}, bad}}
	z := newRequestSizer(req)
	if got := z.size(nil, "m"); got != math.MaxUint64 {
		t.Fatalf("unserializable message fits: %d", got)
	}
	z.set(1, llm.Message{Role: llm.RoleAssistant, Content: "ok"})
	req.Messages[1] = llm.Message{Role: llm.RoleAssistant, Content: "ok"}
	requireSameSize(t, z, nil, req, "repaired")

	req.Tools = []llm.Tool{llm.FunctionTool("t", "d", json.RawMessage("{"))}
	if got := newRequestSizer(req).size(nil, "m"); got != math.MaxUint64 {
		t.Fatalf("unserializable tools fit: %d", got)
	}
	var none *requestSizer
	none.set(0, llm.Message{})
	none.reset()
	if got := none.dropImages(0, llm.Message{Images: []media.Image{sizingImages(t)[0]}}, "r"); len(got.Images) != 0 {
		t.Fatal("a nil sizer kept the images")
	}
}

// referenceProjectImages is projectImages before the sizer: fits serializes
// the whole request.
func referenceProjectImages(req llm.Request, fits func(llm.Request) bool) (llm.Request, error) {
	req.Messages = slices.Clone(req.Messages)
	messages := req.Messages
	if !req.Vision {
		for i, m := range messages {
			if len(m.Images) > 0 {
				messages[i] = omitImages(m, "the selected model does not accept images")
			}
		}
		return req, nil
	}
	count, size := 0, 0
	for _, m := range messages {
		count += len(m.Images)
		size += imageBytes(m.Images)
	}
	within := func() bool { return count <= media.MaxImages && size <= media.MaxSetBytes }
	user, keep := latest(messages)
	for i := 0; i < keep && !(within() && fits(req)); i++ {
		if i == user || len(messages[i].Images) == 0 {
			continue
		}
		count -= len(messages[i].Images)
		size -= imageBytes(messages[i].Images)
		messages[i] = omitImages(messages[i], "removed from this request to fit the model's limits")
	}
	if !within() {
		return req, fmt.Errorf("the latest prompt and tool results carry %d images of %d bytes; one model request allows %d images of %d bytes",
			count, size, media.MaxImages, media.MaxSetBytes)
	}
	return req, nil
}

// Re-projecting after each trim is what the trimming loop does; the result
// must match projecting from scratch with a full serialization.
func TestProjectImagesMatchesReference(t *testing.T) {
	images := sizingImages(t)
	for seed := range uint64(300) {
		rng := rand.New(rand.NewPCG(seed, 2))
		req := llm.Request{Model: "m", Vision: rng.IntN(4) > 0, Messages: sizingHistory(rng, images)}
		c := sizingCalibration(rng, "m")
		budget := uint64(float64(c.size(req)) * (0.1 + rng.Float64()))
		z := newRequestSizer(req)
		fits := func() bool { return z.size(c, "m") <= budget }
		for step := range 5 {
			want, wantErr := referenceProjectImages(req, func(r llm.Request) bool { return c.size(r) <= budget })
			got, err := projectImages(req, z, fits)
			if !reflect.DeepEqual(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Fatalf("seed %d step %d: projection differs (errors %v, %v)", seed, step, err, wantErr)
			}
			if err == nil && fits() != (c.size(want) <= budget) {
				t.Fatalf("seed %d step %d: fits differs", seed, step)
			}
			for i, m := range req.Messages {
				if m.Role == llm.RoleTool && len(m.Content) > 200 {
					req.Messages[i].Content = m.Content[:200]
					z.set(i, req.Messages[i])
					break
				}
			}
		}
	}
}

// referenceChunk is the chunk loop of summarize before the sizer: it
// serializes the whole candidate for every exchange added.
func referenceChunk(cal *calibration, req llm.Request, prefix []llm.Message, summary string, budget uint64) (chunk llm.Request, end int, err error) {
	for next := 1; next <= len(prefix); {
		for next < len(prefix) && prefix[next].Role == llm.RoleTool {
			next++
		}
		data, err := json.Marshal(prefix[:next])
		if err != nil {
			return llm.Request{}, 0, err
		}
		candidate := llm.Request{Model: req.Model, MaxTokens: req.MaxTokens, ReasoningEffort: req.ReasoningEffort,
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: summaryInstruction},
				{Role: llm.RoleUser, Content: "Previous summary:\n" + summary + "\nConversation JSON:\n" + string(data)},
			}}
		if cal.size(candidate) > budget {
			break
		}
		chunk, end = candidate, next
		next++
	}
	return chunk, end, nil
}

// Chunk boundaries and the request sent must not change, for any budget,
// calibration and previous summary, across consecutive chunks that share the
// serialized messages.
func TestSummaryChunkMatchesReference(t *testing.T) {
	for seed := range uint64(300) {
		rng := rand.New(rand.NewPCG(seed, 3))
		history := sizingHistory(rng, sizingImages(t))
		prefix := history[1:]
		for i := range prefix {
			prefix[i].Reasoning, prefix[i].ReasoningDetails, prefix[i].LocalCommand = "", nil, ""
			prefix[i] = describeImages(prefix[i])
		}
		req := llm.Request{Model: "m", MaxTokens: 100, ReasoningEffort: "low"}
		c := sizingCalibration(rng, "m")
		budget := uint64(500 + rng.IntN(4000))
		var h historyJSON
		var summary string
		for len(prefix) > 0 {
			want, wantEnd, wantErr := referenceChunk(c, req, prefix, summary, budget)
			got, end, err := summaryChunk(context.Background(), c, req, prefix, &h, summary, budget)
			if err != nil || wantErr != nil || end != wantEnd || !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d: chunk of %d messages, want %d (errors %v, %v)", seed, end, wantEnd, err, wantErr)
			}
			if end == 0 {
				break
			}
			summary = sizingText(rng, rng.IntN(300))
			prefix = prefix[end:]
			h.drop(end)
		}
	}
}

func TestSummaryChunkErrors(t *testing.T) {
	prefix := []llm.Message{{Role: llm.RoleUser, Content: "one"}, {Role: llm.RoleAssistant, ReasoningDetails: []llm.ReasoningDetail{{"x": make(chan int)}}}}
	var h historyJSON
	if _, _, err := summaryChunk(context.Background(), nil, llm.Request{Model: "m"}, prefix, &h, "", 1<<20); err == nil {
		t.Fatal("unserializable history was summarized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := summaryChunk(ctx, nil, llm.Request{Model: "m"}, prefix[:1], new(historyJSON), "", 1<<20); err == nil {
		t.Fatal("cancelled context ignored")
	}
}
