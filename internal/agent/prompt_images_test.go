package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/media"
)

func imageBlock(t *testing.T, img media.Image) acp.ContentBlock {
	t.Helper()
	data, err := jsonrpc.Marshal(map[string]any{"type": "image", "mimeType": img.MIME(), "data": strings.SplitN(img.DataURL(), ",", 2)[1], "_meta": map[string]any{"unknown": true}})
	if err != nil {
		t.Fatal(err)
	}
	var block acp.ContentBlock
	if err := json.Unmarshal(data, &block); err != nil {
		t.Fatal(err)
	}
	return block
}

func TestImagePromptValidationBounds(t *testing.T) {
	valid := imageBlock(t, testImage(t, 1, 1))
	data, err := base64.StdEncoding.DecodeString(valid.Data)
	if err != nil {
		t.Fatal(err)
	}
	// Headers exercise pixel bounds without allocating the claimed image.
	header := bytes.Clone(data[:33])
	binary.BigEndian.PutUint32(header[16:], 4001)
	binary.BigEndian.PutUint32(header[20:], 4000)
	binary.BigEndian.PutUint32(header[29:], crc32.ChecksumIEEE(header[12:29]))
	padded := valid
	padded.Data = base64.StdEncoding.EncodeToString(append(bytes.Clone(data), make([]byte, media.MaxImageBytes-len(data))...))
	tooBig := padded
	tooBig.Data = base64.StdEncoding.EncodeToString(append(bytes.Clone(data), make([]byte, media.MaxImageBytes+1-len(data))...))
	for _, tc := range []struct {
		name   string
		blocks []acp.ContentBlock
		want   string
	}{
		{"four images", slices.Repeat([]acp.ContentBlock{valid}, media.MaxImages), ""},
		{"eight MiB", []acp.ContentBlock{padded, padded}, ""},
		{"over eight MiB", []acp.ContentBlock{padded, padded, valid}, "total size"},
		{"over four MiB", []acp.ContentBlock{tooBig}, "bytes"},
		{"over encoded limit", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: media.PNG, Data: strings.Repeat("A", base64.StdEncoding.EncodedLen(media.MaxImageBytes)+8)}}, "bytes"},
		{"MIME mismatch", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: media.JPEG, Data: valid.Data}}, "declared"},
		{"unsupported MIME", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: "image/webp", Data: valid.Data}}, "unsupported"},
		{"header pixels", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: media.PNG, Data: base64.StdEncoding.EncodeToString(header)}}, "pixels"},
		{"header only", []acp.ContentBlock{{Type: acp.ContentImage, MimeType: media.PNG, Data: base64.StdEncoding.EncodeToString(data[:33])}}, "corrupt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := promptImages(context.Background(), tc.blocks)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("validation = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestImagePromptQueuedModelChange(t *testing.T) {
	for _, toVision := range []bool{false, true} {
		t.Run(map[bool]string{false: "vision to text", true: "text to vision"}[toVision], func(t *testing.T) {
			models := []llm.Model{{ID: "vision"}, {ID: "text"}, {ID: "unknown"}}
			models[0].Architecture.InputModalities = []string{"text", "image"}
			models[1].Architecture.InputModalities = []string{"text"}
			model := make(scriptedModel, 2)
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			model <- func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &llm.Completion{FinishReason: llm.FinishStop}, nil
			}
			model <- func(_ context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
				user := req.Messages[len(req.Messages)-1]
				if req.Model != "vision" || !req.Vision || len(user.Images) != 1 || user.Content != "Image prompt" {
					t.Error("accepted image prompt did not reach the selected vision model")
				}
				onDelta(llm.Delta{Kind: llm.DeltaText, Text: "seen"})
				return &llm.Completion{Content: "seen", FinishReason: llm.FinishStop}, nil
			}
			c := startAgent(t, t.TempDir(), model, func(context.Context) ([]llm.Model, error) { return models, nil })
			c.initialize()
			cwd := t.TempDir()
			sid := c.newSession(cwd)
			before, after := "vision", "text"
			if toVision {
				before, after = after, before
			}
			c.selectModel(sid, before)
			c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("wait")}}, nil)
			<-entered
			queued := c.send(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{imageBlock(t, testImage(t, 1, 1))}})
			// This response must arrive while the earlier model request is blocked.
			c.selectModel(sid, after)
			release <- struct{}{}
			c.untilIdle(sid)
			resp := c.await(queued)
			if toVision {
				if resp.Error != nil {
					t.Fatal(resp.Error)
				}
				c.untilIdle(sid)
			} else if resp.Error == nil || resp.Error.Code != -32602 {
				t.Fatalf("queued image prompt = %+v", resp)
			}
			c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := c.untilIdle(sid)
			users := 0
			for _, n := range replay {
				if n.Update.Kind == "user_message" {
					users++
				}
			}
			want := 1
			if toVision {
				want++
			}
			if users != want {
				t.Fatalf("replay has %d user messages, want %d", users, want)
			}
		})
	}
}

func TestImagePromptCancelledBeforeClaim(t *testing.T) {
	s, out := imageSession(t, noModel(t), 0)
	sub := &submission{ctx: context.Background(), prompt: []acp.ContentBlock{imageBlock(t, testImage(t, 2, 2))}, reply: jsonrpc.NewReply()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The worker can decode while persistence is busy; cancellation must
	// still win before a message becomes visible or durable.
	s.flushMu.Lock()
	done := make(chan bool, 1)
	go func() { done <- s.insert(ctx, sub) }()
	cancel()
	s.flushMu.Unlock()
	if <-done {
		t.Fatal("cancelled image prompt inserted")
	}
	if _, err := settledReply(t, sub.reply); err == nil || err.Code != -32800 {
		t.Fatalf("cancellation = %+v", err)
	}
	if len(s.history) != 1 || len(s.transcript) != 0 || len(out) != 0 {
		t.Fatal("cancelled image prompt escaped insertion boundary")
	}
}

func TestImagePromptInsertion(t *testing.T) {
	img := testImage(t, 3, 2)
	for _, withText := range []bool{false, true} {
		t.Run(map[bool]string{false: "image only", true: "mixed"}[withText], func(t *testing.T) {
			s, out := imageSession(t, noModel(t), 0)
			blocks := []acp.ContentBlock{imageBlock(t, img), imageBlock(t, testImage(t, 2, 3))}
			wantText, wantTitle := "Image prompt", "Image prompt"
			if withText {
				text := "resource body"
				blocks = append(blocks, acp.ContentBlock{Type: acp.ContentResource, Resource: &acp.Resource{URI: "file:///a", Text: &text}}, acp.Text("Explain these\nplease"))
				wantText, wantTitle = "<resource uri=\"file:///a\">\nresource body\n</resource>\n\nExplain these\nplease", "Explain these"
			}
			sub := &submission{ctx: context.Background(), prompt: blocks, reply: jsonrpc.NewReply()}
			if !s.insert(context.Background(), sub) {
				t.Fatal("valid image prompt rejected")
			}
			if user := s.history[1]; user.Content != wantText || len(user.Images) != 2 || user.Images[0].Width() != 3 || user.Images[1].Width() != 2 {
				t.Fatalf("image prompt not retained in order: text=%q, images=%d", user.Content, len(user.Images))
			}
			if s.title != wantTitle {
				t.Fatalf("title = %q, want %q", s.title, wantTitle)
			}
			if got := (<-out).(acp.UserMessage).Content; !reflect.DeepEqual(got, blocks) {
				t.Fatal("prompt echo changed")
			}
		})
	}
}

func TestImagePromptTitleExcludesResourceContents(t *testing.T) {
	image := imageBlock(t, testImage(t, 1, 1))
	text := "PRIVATE RESOURCE CONTENT"
	blocks := []acp.ContentBlock{image, {Type: acp.ContentResource, Resource: &acp.Resource{URI: "file:///context", Text: &text}}, acp.Text(" \n ")}
	if got := title(blocks); got != "Image prompt" {
		t.Fatalf("resource or whitespace used as image title: %q", got)
	}
	if got := promptText(blocks); !strings.Contains(got, text) {
		t.Fatal("resource contents lost from model context")
	}
}

func TestImagePromptRejectsBeforeInsertion(t *testing.T) {
	valid := imageBlock(t, testImage(t, 1, 1))
	var invalid acp.ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"image","data":"!!!","mimeType":"image/png"}`), &invalid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, model string
		blocks      []acp.ContentBlock
		cancel      bool
		code        int
		want        string
	}{
		{"invalid", "m", []acp.ContentBlock{invalid}, false, -32602, "base64"},
		{"unknown vision", "text", []acp.ContentBlock{valid}, false, -32602, "image"},
		{"no selection", "", []acp.ContentBlock{valid}, false, -32602, "select a model"},
		{"count", "m", slices.Repeat([]acp.ContentBlock{valid}, media.MaxImages+1), false, -32602, "limit"},
		{"cancel beats validation", "m", []acp.ContentBlock{invalid}, true, -32800, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, out := imageSession(t, noModel(t), 0)
			s.model = tc.model
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			sub := &submission{ctx: ctx, prompt: tc.blocks, reply: jsonrpc.NewReply()}
			if s.insert(context.Background(), sub) {
				t.Fatal("rejected prompt inserted")
			}
			if _, err := settledReply(t, sub.reply); err == nil || err.Code != tc.code || !strings.Contains(strings.ToLower(err.Message), tc.want) {
				t.Fatalf("rejection = %+v", err)
			}
			if len(s.history) != 1 || len(s.transcript) != 0 || len(out) != 0 || s.title != "" {
				t.Fatal("rejected image prompt escaped insertion boundary")
			}
		})
	}
}
