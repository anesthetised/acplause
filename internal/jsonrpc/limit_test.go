package jsonrpc_test

import (
	"testing"

	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/media"
)

// External because internal/media imports jsonrpc.
func TestLineLimitCoversMaximumImagePrompt(t *testing.T) {
	// Base64 expands 4/3; allow the same again for text and framing.
	if need := media.MaxSetBytes * 4 / 3 * 2; jsonrpc.MaxLine < need {
		t.Fatalf("maxLine = %d, want at least %d", jsonrpc.MaxLine, need)
	}
}
