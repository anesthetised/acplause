package llm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestReadStreamRejectsMalformedReasoningIdentities(t *testing.T) {
	for _, field := range []string{"index", "type"} {
		for _, malformed := range []string{"[]", `{}`} {
			t.Run(field+"/"+malformed, func(t *testing.T) {
				identity := `"reasoning.text"`
				index := `0`
				if field == "type" {
					identity = malformed
				} else {
					index = malformed
				}
				fragment := `{"type":` + identity + `,"index":` + index + `,"text":"fragment"}`
				stream := data(`{"choices":[{"delta":{"reasoning_details":[`+fragment+`]}}]}`) +
					data(`{"choices":[{"delta":{"reasoning_details":[`+fragment+`]}}]}`) + data(`[DONE]`)
				_, err := readStream(context.Background(), strings.NewReader(stream), func(Delta) {})
				protocolErr, ok := errors.AsType[*Error](err)
				if !ok || protocolErr.Kind != KindProtocol {
					t.Fatalf("got %v, want protocol error", err)
				}
			})
		}
	}
}

func TestMergeDetailPreservesIndexedAndUnindexedPayloads(t *testing.T) {
	stream := data(`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","index":2,"text":"first","signature":"opaque","vendor":{"x":1}},{"type":"reasoning.encrypted","index":3,"data":"cipher"},{"type":"future.detail","payload":"opaque"}]}}]}`) +
		data(`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","index":2,"text":"second","signature":"next"},{"type":"reasoning.encrypted","index":3,"data":"text"},{"type":"future.detail","payload":"next"}]}}]}`) + data(`[DONE]`)
	completion, err := readStream(context.Background(), strings.NewReader(stream), func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	want := []ReasoningDetail{
		{"type": "reasoning.text", "index": float64(2), "text": "firstsecond", "signature": "next", "vendor": map[string]any{"x": float64(1)}},
		{"type": "reasoning.encrypted", "index": float64(3), "data": "ciphertext"},
		{"type": "future.detail", "payload": "next"},
	}
	if len(completion.ReasoningDetails) != len(want) {
		t.Fatalf("details: got %#v", completion.ReasoningDetails)
	}
	for i := range want {
		if !reflect.DeepEqual(completion.ReasoningDetails[i], want[i]) {
			t.Errorf("detail %d: got %#v, want %#v", i, completion.ReasoningDetails[i], want[i])
		}
	}
}
