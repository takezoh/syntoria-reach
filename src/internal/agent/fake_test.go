package agent

import (
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
)

// TestFakeHarness is not a test: it turns the test binary into a fake harness.
func TestFakeHarness(t *testing.T) {
	agenttest.MaybeRun()
	t.Skip("helper process")
}
