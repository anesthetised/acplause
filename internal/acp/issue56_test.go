package acp

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestIssue56UnknownContentFieldsStayOpaque(t *testing.T) {
	for _, kind := range []string{"_widget", "future_variant"} {
		raw := `{"type":"` + kind + `","text":{"value":"opaque"},"name":42,"uri":{"future":true},"data":{},"mimeType":[] ,"resource":false}`
		var block ContentBlock
		if err := json.Unmarshal([]byte(raw), &block); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		got, err := json.Marshal(block)
		var compact bytes.Buffer
		_ = json.Compact(&compact, []byte(raw))
		if err != nil || !bytes.Equal(got, compact.Bytes()) {
			t.Fatalf("%s round trip = %s, %v", kind, got, err)
		}
	}
}

func TestIssue56KnownContentConsumesOnlyItsFields(t *testing.T) {
	text := "body"
	for _, tt := range []struct {
		raw  string
		want ContentBlock
	}{
		{`{"type":"text","text":"hello","uri":42}`, ContentBlock{Type: ContentText, Text: "hello"}},
		{`{"type":"resource_link","uri":"file:///x","name":"x","text":42}`, ContentBlock{Type: ContentResourceLink, URI: "file:///x", Name: "x"}},
		{`{"type":"image","data":"YWJj","mimeType":"image/png","text":{},"uri":42}`, ContentBlock{Type: ContentImage, Data: "YWJj", MimeType: "image/png"}},
		{`{"type":"resource","resource":{"uri":"file:///x","text":"body"},"data":{}}`, ContentBlock{Type: ContentResource, Resource: &Resource{URI: "file:///x", Text: &text}}},
	} {
		var block ContentBlock
		if err := json.Unmarshal([]byte(tt.raw), &block); err != nil {
			t.Fatalf("unrelated fields: %v", err)
		}
		got, err := json.Marshal(block)
		if err != nil || string(got) != tt.raw {
			t.Fatalf("known variant round trip = %s, %v", got, err)
		}
		block.raw = nil
		if !reflect.DeepEqual(block, tt.want) {
			t.Fatalf("parsed %s: got %+v, want %+v", tt.raw, block, tt.want)
		}
	}
	for _, raw := range []string{`{"type":"text","text":42}`, `{"type":"resource_link","uri":42}`, `{"type":"image","data":{},"mimeType":"image/png"}`, `{"type":"resource","resource":false}`} {
		var block ContentBlock
		if err := json.Unmarshal([]byte(raw), &block); err == nil {
			t.Fatalf("accepted malformed consumed field: %s", raw)
		}
	}
}
