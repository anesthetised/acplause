package acp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

func TestPromptContentValidation(t *testing.T) {
	for _, tt := range []struct {
		name, block, reason string
	}{
		{"audio", `{"type":"audio","mimeType":"audio/wav","data":"AA=="}`, "unsupported content block type"},
		{"custom", `{"type":"_widget","payload":{"text":[]}}`, "unsupported content block type"},
		{"future", `{"type":"future_variant"}`, "unsupported content block type"},
		{"missing text", `{"type":"text"}`, "text is required"},
		{"null text", `{"type":"text","text":null}`, "text is required"},
		{"wrong case", `{"type":"text","Text":"opaque"}`, "text is required"},
		{"missing image data", `{"type":"image","mimeType":"image/png"}`, "data is required"},
		{"missing image MIME type", `{"type":"image","data":"AA=="}`, "mimeType is required"},
		{"invalid image URI", `{"type":"image","data":"AA==","mimeType":"image/png","uri":"relative"}`, "absolute URI"},
		{"missing link name", `{"type":"resource_link","uri":"file:///x"}`, "name is required"},
		{"invalid link description", `{"type":"resource_link","uri":"file:///x","name":"x","description":{}}`, "description"},
		{"relative link", `{"type":"resource_link","uri":"./x","name":"x"}`, "absolute URI"},
		{"negative size", `{"type":"resource_link","uri":"file:///x","name":"x","size":-1}`, "size"},
		{"fractional size", `{"type":"resource_link","uri":"file:///x","name":"x","size":1.5}`, "size"},
		{"string size", `{"type":"resource_link","uri":"file:///x","name":"x","size":"1"}`, "size"},
		{"invalid link icon", `{"type":"resource_link","uri":"file:///x","name":"x","icons":[{}]}`, "src is required"},
		{"invalid annotation priority", `{"type":"text","text":"x","annotations":{"priority":2}}`, "priority"},
		{"invalid annotation audience", `{"type":"text","text":"x","annotations":{"audience":[null]}}`, "audience"},
		{"invalid metadata", `{"type":"text","text":"x","_meta":[]}`, "_meta"},
		{"missing resource", `{"type":"resource","resource":null}`, "resource is required"},
		{"missing resource contents", `{"type":"resource","resource":{"uri":"file:///x"}}`, "text or blob"},
		{"missing resource URI", `{"type":"resource","resource":{"text":"x"}}`, "uri is required"},
		{"invalid resource blob", `{"type":"resource","resource":{"uri":"file:///x","blob":"!"}}`, "base64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var request PromptRequest
			err := jsonrpc.DecodeParams([]byte(`{"sessionId":"s","prompt":[`+tt.block+`]}`), &request)
			if err == nil || jsonrpc.AsError(err).Code != jsonrpc.CodeInvalidParams || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("got %v, want invalid params containing %q", err, tt.reason)
			}
		})
	}
}

func TestPromptContentPreservesExtensions(t *testing.T) {
	const blocks = `[{"type":"text","text":"","Text":false,"annotations":{"audience":["_local","future_role"],"priority":0,"future":{}},"_meta":{"example.test/data":9007199254740993}},{"type":"resource_link","uri":"custom:item#here","name":"","size":18446744073709551615,"icons":[{"src":"https://example.test/icon","theme":"_custom"}]},{"type":"resource_link","uri":"file:///x","name":"x","size":null},{"type":"resource","resource":{"uri":"file:///x","text":"","blob":false}},{"type":"resource","resource":{"uri":"file:///x","blob":"AA==","text":42}}]`
	var request PromptRequest
	if err := jsonrpc.DecodeParams([]byte(`{"sessionId":"s","prompt":`+blocks+`}`), &request); err != nil {
		t.Fatal(err)
	}
	if request.Prompt[0].Text != "" || request.Prompt[3].Resource.Text == nil || request.Prompt[4].Resource.Text != nil {
		t.Fatalf("unexpected projection: %#v", request.Prompt)
	}
	if got, err := json.Marshal(request.Prompt); err != nil || string(got) != blocks {
		t.Fatalf("round trip = %s, %v", got, err)
	}
}
