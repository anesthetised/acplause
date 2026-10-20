package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/mcp"
	"github.com/anesthetised/acplause/internal/media"
	"github.com/anesthetised/acplause/internal/store"
)

func testImage(t *testing.T, w, h int) media.Image {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	img, err := media.New(media.PNG, base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// bigImage is about 3 MiB of incompressible pixels, so three exceed the
// request byte limit while each is within the per-image one.
var bigImage = sync.OnceValues(func() (media.Image, error) {
	pixels := image.NewRGBA(image.Rect(0, 0, 880, 880))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range pixels.Pix {
		pixels.Pix[i] = byte(r.Uint32())
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, pixels); err != nil {
		return media.Image{}, err
	}
	return media.New(media.PNG, base64.StdEncoding.EncodeToString(buf.Bytes()))
})

func imageResult(callID, output string, images ...media.Image) llm.Message {
	m := toolResult(callID, output)
	m.Images = images
	return m
}

func imageCount(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		n += len(m.Images)
	}
	return n
}

// imageSession uses a vision model with the given window; "text" is the
// same without image input.
func imageSession(t *testing.T, model Model, window uint64) (*Session, recorder) {
	t.Helper()
	s, out := testSession(t, model)
	vision := llm.Model{ID: "m", ContextLength: window}
	vision.Architecture.InputModalities = []string{"text", "image"}
	s.catalog = newCatalog([]llm.Model{vision, {ID: "text", ContextLength: window}})
	return s, out
}

func noModel(t *testing.T) Model {
	return fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		t.Error("unexpected model request")
		return nil, errors.New("unexpected")
	})
}

func TestRequestSizeReservesImagesNotBase64(t *testing.T) {
	img := testImage(t, 400, 300)
	text := llm.Request{Messages: []llm.Message{toolResult("a", "x")}}
	withImage := llm.Request{Messages: []llm.Message{imageResult("a", "x", img, img)}}
	if got := requestSize(withImage) - requestSize(text); got != imageLabelBytes {
		t.Fatalf("non-vision image counted as %d, want the marker allowance", got)
	}
	withImage.Vision = true
	if got := requestSize(withImage) - requestSize(text); got != imageLabelBytes+2*imageTokens {
		t.Fatalf("vision images counted as %d", got)
	}
	big, err := bigImage()
	if err != nil {
		t.Fatal(err)
	}
	withImage.Messages[0].Images = []media.Image{big}
	if requestSize(withImage) > 10000 {
		t.Fatalf("base64 counted as text: %d", requestSize(withImage))
	}
}

func TestCatalogVisionNeedsExplicitModality(t *testing.T) {
	vision := llm.Model{ID: "v"}
	vision.Architecture.InputModalities = []string{"image", "text"}
	text := llm.Model{ID: "t"}
	text.Architecture.InputModalities = []string{"text"}
	c := newCatalog([]llm.Model{vision, text, {ID: "unknown"}})
	if !c.vision("v") || c.vision("t") || c.vision("unknown") || c.vision("absent") {
		t.Fatal("vision must come only from an explicit image input modality")
	}
}

func TestNonVisionRequestGetsMarkers(t *testing.T) {
	s, _ := imageSession(t, noModel(t), 0)
	if err := s.SetConfigOption(configModel, acp.ConfigValueID, json.RawMessage(`"text"`)); err != nil {
		t.Fatal(err)
	}
	img := testImage(t, 4, 4)
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "look"}, callMessage("a", "b"),
		imageResult("a", "page text", img), imageResult("b", "", img, img))
	before := slices.Clone(s.history)
	req, err := s.prepareRequest(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Vision || imageCount(req.Messages) != 0 {
		t.Fatalf("images sent to a model not known to see them: %+v", req.Request)
	}
	if got := req.Messages[3].Content; got != "page text\n[image omitted: the selected model does not accept images]" {
		t.Fatalf("text with marker: %q", got)
	}
	if got := req.Messages[4].Content; got != "[2 images omitted: the selected model does not accept images]" {
		t.Fatalf("image-only marker: %q", got)
	}
	if !reflect.DeepEqual(s.history, before) {
		t.Fatal("projection changed the history")
	}
	assertToolPairs(t, req.Messages)
}

func TestVisionRequestCapsImagesOldestFirst(t *testing.T) {
	s, _ := imageSession(t, noModel(t), 0) // the limits apply without a known window
	img := testImage(t, 4, 4)
	s.history = append(s.history,
		llm.Message{Role: llm.RoleUser, Content: "go"},
		callMessage("a"), imageResult("a", "first", img, img),
		callMessage("b"), imageResult("b", "second", img, img),
		callMessage("c"), imageResult("c", "third", img),
		callMessage("d"), imageResult("d", "latest", img, img))
	before := slices.Clone(s.history)
	req, err := s.prepareRequest(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Vision || imageCount(req.Messages) != 3 {
		t.Fatalf("sent %d images", imageCount(req.Messages))
	}
	for i, want := range map[int]string{3: "first", 5: "second"} {
		if m := req.Messages[i]; len(m.Images) != 0 || m.Content != want+"\n[2 images omitted: removed from this request to fit the model's limits]" {
			t.Fatalf("message %d: %+v", i, m)
		}
	}
	if len(req.Messages[7].Images) != 1 || len(req.Messages[9].Images) != 2 {
		t.Fatal("newer images were removed")
	}
	if !reflect.DeepEqual(s.history, before) {
		t.Fatal("image removal reached the history")
	}
	assertToolPairs(t, req.Messages)
}

func TestVisionRequestCapsImageBytes(t *testing.T) {
	big, err := bigImage()
	if err != nil {
		t.Fatal(err)
	}
	if big.Size() > media.MaxImageBytes || 3*big.Size() <= media.MaxSetBytes {
		t.Fatalf("fixture of %d bytes does not exercise the byte limit", big.Size())
	}
	s, _ := imageSession(t, noModel(t), 0)
	s.history = append(s.history,
		llm.Message{Role: llm.RoleUser, Content: "go"},
		callMessage("a"), imageResult("a", "", big),
		callMessage("b"), imageResult("b", "", big),
		callMessage("c"), imageResult("c", "", big))
	req, err := s.prepareRequest(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[3].Images) != 0 || len(req.Messages[5].Images) != 1 || len(req.Messages[7].Images) != 1 {
		t.Fatal("oldest image not removed to fit the byte limit")
	}
}

func TestProtectedImagesOverLimitFail(t *testing.T) {
	img := testImage(t, 4, 4)
	for name, history := range map[string][]llm.Message{
		"siblings": {{Role: llm.RoleUser, Content: "go"}, callMessage("a", "b"),
			imageResult("a", "", img, img, img), imageResult("b", "", img, img)},
		"prompt and exchange": {{Role: llm.RoleUser, Content: "see", Images: []media.Image{img, img, img}}, callMessage("a"),
			imageResult("a", "", img, img)},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := imageSession(t, noModel(t), 0)
			s.history = append(s.history, history...)
			before := slices.Clone(s.history)
			_, err := s.prepareRequest(context.Background(), nil)
			if err == nil || !strings.Contains(err.Error(), "5 images") {
				t.Fatalf("protected images over the limit: %v", err)
			}
			if !reflect.DeepEqual(s.history, before) {
				t.Fatal("failure changed the history")
			}
		})
	}
}

func TestImagesLeaveBeforeTextIsTrimmed(t *testing.T) {
	s, _ := imageSession(t, noModel(t), 0)
	img := testImage(t, 4, 4)
	long := strings.Repeat("output ", 500)
	s.history = append(s.history,
		llm.Message{Role: llm.RoleUser, Content: "go"},
		callMessage("a"), imageResult("a", long, img),
		callMessage("b"), imageResult("b", "b", img),
		callMessage("c"), imageResult("c", "c", img))
	// Require one image's removal regardless of the current tool catalog size.
	budget := requestSize(s.request().Request) - imageTokens/2
	model := s.catalog.models["m"]
	model.ContextLength = budget * 4 / 3
	s.catalog.models["m"] = model
	before := slices.Clone(s.history)
	req, err := s.prepareRequest(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if requestSize(req.Request) > budget || len(req.Messages[3].Images) != 0 || len(req.Messages[5].Images) != 1 || len(req.Messages[7].Images) != 1 {
		t.Fatalf("size %d, images %d", requestSize(req.Request), imageCount(req.Messages))
	}
	if !strings.HasPrefix(req.Messages[3].Content, long) {
		t.Fatal("text trimmed before an image was removed")
	}
	if !reflect.DeepEqual(s.history, before) {
		t.Fatal("history changed although only the request needed to")
	}
}

func TestCompactionDescribesImages(t *testing.T) {
	img := testImage(t, 64, 48)
	raw, err := json.Marshal(imageBlock(t, img))
	if err != nil {
		t.Fatal(err)
	}
	projected := projectMCPResult(mcp.Result{Content: []json.RawMessage{raw}}, true)
	if projected.Failed || len(projected.Images) != 1 {
		t.Fatal("MCP image projection failed before compaction")
	}
	encoded := strings.TrimPrefix(img.DataURL(), "data:image/png;base64,")
	var summaries []llm.Request
	s, out := imageSession(t, fakeModel(func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		summaries = append(summaries, req)
		return &llm.Completion{Content: "Earlier: a screenshot was taken.", FinishReason: llm.FinishStop}, nil
	}), 22000) // the retained image and expanded tool catalog fit a 16500-byte input budget
	s.history = append(s.history,
		llm.Message{Role: llm.RoleUser, Content: strings.Repeat("old request ", 600), Images: []media.Image{img}},
		callMessage("a"), imageResult("a", "old page\n"+projected.Output, projected.Images...),
		llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("old answer ", 400)},
		llm.Message{Role: llm.RoleUser, Content: "current"}, callMessage("b"), imageResult("b", "new page\n"+projected.Output, projected.Images...))
	keep := slices.Clone(s.history[5:])
	req, err := s.prepareRequest(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) == 0 {
		t.Fatal("no summary request")
	}
	var user, tool bool
	for _, r := range summaries {
		if r.Vision || imageCount(r.Messages) != 0 {
			t.Fatal("summary request carries images")
		}
		for _, m := range r.Messages {
			if strings.Contains(m.Content, encoded[:32]) || strings.Contains(m.Content, "data:image") {
				t.Fatal("summary input contains image bytes")
			}
			tool = tool || strings.Contains(m.Content, `[image returned by this tool call: image/png, 64x48 pixels; not shown to you]`)
			user = user || strings.Contains(m.Content, `[image attached by the user: image/png, 64x48 pixels; not shown to you]`)
		}
	}
	if !user || !tool {
		t.Fatal("summary input lacks image descriptors")
	}
	if !reflect.DeepEqual(s.history[2:], keep) || !reflect.DeepEqual(req.Messages[2:], keep) || !req.Vision || imageCount(req.Messages) != 1 {
		t.Fatalf("protected exchange not kept with its image: %+v", req.Messages)
	}
	assertToolPairs(t, req.Messages)
	<-out
	if end := (<-out).(acp.CompactionUpdate); end.Status != acp.CompactionCompleted {
		t.Fatalf("compaction: %+v", end)
	}
}

func TestCompactionFailsWhenProtectedImagesCannotFit(t *testing.T) {
	img := testImage(t, 4, 4)
	s, out := imageSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		return &llm.Completion{Content: "summary", FinishReason: llm.FinishStop}, nil
	}), 16000) // two reserved images exceed the input budget of 12000
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "look"}, callMessage("a"), imageResult("a", "", img, img))
	before := slices.Clone(s.history)
	remaining := 5
	if _, err := s.prepareRequest(context.Background(), &remaining); err == nil {
		t.Fatal("oversized protected exchange accepted")
	}
	if !reflect.DeepEqual(s.history, before) {
		t.Fatal("failed compaction dropped content")
	}
	<-out
	if end := (<-out).(acp.CompactionUpdate); end.Status != acp.CompactionFailed || end.Error == "" {
		t.Fatalf("compaction: %+v", end)
	}
}

func TestModelChangeReappliesImagePolicy(t *testing.T) {
	s, _ := imageSession(t, noModel(t), 0)
	img := testImage(t, 4, 4)
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "look"}, callMessage("a"), imageResult("a", "", img))
	for _, model := range []string{"m", "text", "m"} {
		if err := s.SetConfigOption(configModel, acp.ConfigValueID, json.RawMessage(`"`+model+`"`)); err != nil {
			t.Fatal(err)
		}
		req, err := s.prepareRequest(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if vision := model == "m"; req.Model != model || req.Vision != vision || (imageCount(req.Messages) == 1) != vision {
			t.Fatalf("%s: vision=%t images=%d", model, req.Vision, imageCount(req.Messages))
		}
		if len(s.history[3].Images) != 1 {
			t.Fatal("history lost the image")
		}
	}
}

func TestImageHistoryPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := store.Open(dir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	img := testImage(t, 5, 3)
	cat := newCatalog([]llm.Model{{ID: "m"}})
	s, err := newSession(context.Background(), deps{out: make(recorder, 8), store: first, log: slog.New(slog.DiscardHandler)}, cat, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := first.Create(context.Background(), s.record()); err != nil {
		t.Fatal(err)
	}

	// A failed save keeps the image rows pending for the next one.
	s.deps.store = saveStore{save: func(context.Context, store.Session, []store.Row, []store.Row) error { return errors.New("disk full") }}
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "look", Images: []media.Image{img}}, callMessage("a"), imageResult("a", "page", img, img))
	s.flush()
	if s.saved != 0 {
		t.Fatal("failed save marked as saved")
	}
	s.deps.store = first
	s.flush()
	if s.saved != 3 {
		t.Fatalf("saved %d messages", s.saved)
	}
	if err := first.Release(context.Background(), s.id); err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(dir, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	loaded, err := second.Acquire(context.Background(), s.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range loaded.History {
		if bytes.Contains(row, []byte(`"images"`)) && !bytes.Contains(row, []byte(`"mimeType":"image/png"`)) {
			t.Fatalf("image row: %s", row)
		}
	}
	restored, err := restoreSession(context.Background(), deps{}, cat, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !reflect.DeepEqual(restored.history[1:], s.history[1:]) {
		t.Fatalf("restored history differs: %+v", restored.history)
	}

	// The first process lost the lease: its next save stops, losing nothing.
	s.appendHistory(llm.Message{Role: llm.RoleAssistant, Content: "late"})
	s.flush()
	if !s.isLost() {
		t.Fatal("save after takeover did not detect the loss")
	}
	again, err := second.Acquire(context.Background(), s.id)
	if err != nil || len(again.History) != 3 {
		t.Fatalf("lost session wrote history: %v, %d rows", err, len(again.History))
	}
}

func TestRestoreImageRows(t *testing.T) {
	img := testImage(t, 2, 2)
	call, err := json.Marshal(callMessage("done", "unfinished"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(imageResult("done", "shot", img))
	if err != nil {
		t.Fatal(err)
	}
	load := func(state string, rows ...[]byte) (*Session, error) {
		return restoreSession(context.Background(), deps{}, newCatalog(nil), &store.Loaded{
			Session: store.Session{ID: "s", Cwd: t.TempDir(), State: state}, History: rows,
		})
	}

	// Restart recovery answers the unfinished call and keeps the image.
	s, err := load("running", call, result)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.history) != 4 || len(s.history[2].Images) != 1 || s.history[2].Images[0].Width() != 2 || s.history[3].Content != interrupted {
		t.Fatalf("recovered history: %+v", s.history)
	}
	assertToolPairs(t, s.history)

	// Rows from before images load unchanged.
	old, err := load("idle", []byte(`{"role":"user","content":"hi"}`), []byte(`{"role":"assistant","content":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if len(old.history) != 3 || old.history[1].Images != nil || old.history[2].Content != "hello" {
		t.Fatalf("old rows: %+v", old.history)
	}

	// A damaged image fails to load instead of reaching the model.
	if _, err := load("idle", []byte(`{"role":"tool","tool_call_id":"a","images":[{"mimeType":"image/png","data":"bm90IGFuIGltYWdl"}]}`)); err == nil {
		t.Fatal("damaged image row loaded")
	}
}
