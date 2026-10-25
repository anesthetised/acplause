//go:build unix

package agent

import "testing"

// Open roots can be renamed on Unix; Windows rejects that physical operation.
// The shared tests cover content freshness and additional-root rebinding.
func TestInstructionsRefreshResumeReplacedRoot(t *testing.T) {
	testInstructionsRefreshBarrier(t, false, true)
}

func TestParallelReadResumeReplacedRootPinsGroup(t *testing.T) {
	testParallelReadResumePinsGroup(t, true)
}
