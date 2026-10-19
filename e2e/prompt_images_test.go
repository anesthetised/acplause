package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"testing"
)

const imageModelsJSON = `{"data":[
	{"id":"test/model","architecture":{"input_modalities":["text","image"]}},
	{"id":"test/plain","architecture":{"input_modalities":["text"]}},
	{"id":"test/unknown"}
]}`

func promptImage(t *testing.T, mime string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	pixels := image.NewGray(image.Rect(0, 0, 3, 2))
	var err error
	if mime == "image/png" {
		err = png.Encode(&buf, pixels)
	} else {
		err = jpeg.Encode(&buf, pixels, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": "image", "mimeType": mime, "data": base64.StdEncoding.EncodeToString(buf.Bytes()), "uri": "file:///clipboard", "_meta": map[string]any{"future": true}}
}

func imageParams(sid string, blocks []any) map[string]any {
	return map[string]any{"sessionId": sid, "prompt": blocks}
}

func assertImageEcho(t *testing.T, u update, blocks []any) {
	t.Helper()
	var got any
	if err := json.Unmarshal(u.Content, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, blocks) {
		t.Fatal("image prompt echo changed")
	}
}

func assertImageParts(t *testing.T, msg map[string]any, text string, images ...map[string]any) {
	t.Helper()
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != len(images)+1 || msg["role"] != "user" {
		t.Fatal("user image parts missing")
	}
	if part := parts[0].(map[string]any); part["type"] != "text" || part["text"] != text {
		t.Fatalf("image context text = %v", part)
	}
	for i, img := range images {
		part := parts[i+1].(map[string]any)
		want := "data:" + img["mimeType"].(string) + ";base64," + img["data"].(string)
		if part["type"] != "image_url" || part["image_url"].(map[string]any)["url"] != want {
			t.Fatal("provider did not receive the original pixels in order")
		}
	}
	if _, ok := msg["images"]; ok {
		t.Fatal("internal image field leaked upstream")
	}
}

func TestImagePromptRestartAndModelSwitch(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "image only", true: "text and resources"}[mixed], func(t *testing.T) {
			t.Parallel()
			dir, cwd := t.TempDir(), t.TempDir()
			llm := newMockLLM(t, imageModelsJSON)
			llm.enqueue(reply("seen"), reply("text history"), reply("pixels again"))
			first := start(t, llm, withDataDir(dir))
			var init struct {
				Capabilities struct {
					Session struct{ Prompt map[string]json.RawMessage }
				}
			}
			first.ok("initialize", map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "images", "version": "1"}}, &init)
			if string(init.Capabilities.Session.Prompt["image"]) != "{}" {
				t.Fatal("image prompt capability not advertised")
			}
			sid := first.newSession(cwd).SessionID
			setModel(first, sid)
			png, jpeg := promptImage(t, "image/png"), promptImage(t, "image/jpeg")
			blocks := []any{png, jpeg}
			text, title := "Image prompt", "Image prompt"
			if mixed {
				blocks = []any{png, map[string]any{"type": "text", "text": "Compare"}, map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///context", "text": "details"}}, jpeg}
				text, title = "Compare\n\n<resource uri=\"file:///context\">\ndetails\n</resource>", "Compare"
			}
			first.accepted(first.request("session/prompt", imageParams(sid, blocks)))
			updates := first.untilIdle(sid)
			assertImageEcho(t, updates[0], blocks)
			if updates[1].Title != title || updates[len(updates)-1].StopReason != "end_turn" {
				t.Fatalf("image turn = %v", kinds(updates))
			}
			assertImageParts(t, llm.request(0).Messages[1], text, png, jpeg)
			first.stop()

			second := start(t, llm, withDataDir(dir))
			second.initialize()
			var list listResult
			second.ok("session/list", map[string]any{}, &list)
			if len(list.Sessions) != 1 || list.Sessions[0].Title != title {
				t.Fatal("image title did not survive restart")
			}
			second.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := second.untilIdle(sid)
			assertImageEcho(t, replay[0], blocks)
			second.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/plain"}, nil)
			second.accepted(second.prompt(sid, "Continue"))
			second.untilIdle(sid)
			req := llm.request(1)
			if req.Model != "test/plain" {
				t.Fatal("model was substituted")
			}
			history, ok := req.Messages[1]["content"].(string)
			if !ok || !strings.Contains(history, "2 images omitted") || strings.Contains(history, png["data"].(string)) {
				t.Fatal("historical images not marked for text model")
			}
			setModel(second, sid)
			second.accepted(second.prompt(sid, "Look again"))
			second.untilIdle(sid)
			assertImageParts(t, llm.request(2).Messages[1], text, png, jpeg)
		})
	}
}

func TestImagePromptRejection(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, imageModelsJSON)
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	sid := a.newSession(cwd).SessionID
	valid := promptImage(t, "image/png")
	a.fails("session/prompt", imageParams(sid, []any{valid}), -32602)
	for _, model := range []string{"test/plain", "test/unknown", "test/model"} {
		a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": model}, nil)
		blocks := []any{valid}
		if model == "test/model" {
			blocks = []any{map[string]any{"type": "image", "mimeType": "image/png", "data": "!!!"}}
		}
		a.fails("session/prompt", imageParams(sid, blocks), -32602)
	}
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
	if replay := a.untilIdle(sid); len(replay) != 1 {
		t.Fatalf("rejected prompt emitted updates: %v", kinds(replay))
	}
	if llm.requestCount() != 0 {
		t.Fatal("rejected images reached provider")
	}
}

func TestImagePromptQueueAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "model changed", true: "session cancelled"}[cancel], func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t, imageModelsJSON)
			release := make(chan struct{})
			llm.enqueue(waitFor(release, nil))
			a, sid, cwd := started(t, llm)
			a.accepted(a.prompt(sid, "Wait"))
			a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
			queued := a.request("session/prompt", imageParams(sid, []any{promptImage(t, "image/png")}))
			code := -32602
			if cancel {
				code = -32800
				a.notify("session/cancel", map[string]any{"sessionId": sid})
			} else {
				a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/plain"}, nil)
				close(release)
			}
			if resp := a.response(queued); resp.Error == nil || resp.Error.Code != code {
				t.Fatalf("queued image prompt = %+v", resp.Error)
			}
			updates := a.untilIdle(sid)
			want := "end_turn"
			if cancel {
				want = "cancelled"
			}
			if updates[len(updates)-1].StopReason != want {
				t.Fatalf("first turn = %v", kinds(updates))
			}
			a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			users := 0
			for _, u := range a.untilIdle(sid) {
				if u.is("user_message") {
					users++
				}
			}
			if users != 1 || llm.requestCount() != 1 {
				t.Fatal("queued image prompt escaped rejection")
			}
		})
	}
}
