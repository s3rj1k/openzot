package agent

import (
	"testing"

	"github.com/openzot/openzot/internal/loop"
)

// Token usage must reach the public stream so a consumer can show real cost.
func TestUsageEventReachesTheStream(t *testing.T) {
	got, ok := translate(loop.Event{Kind: loop.EventUsage, InputTokens: 100, OutputTokens: 50})

	if !ok {
		t.Fatal("a usage event must be forwarded, not dropped")
	}

	event, isUsage := got.(UsageEvent)
	if !isUsage {
		t.Fatalf("got %T, want UsageEvent", got)
	}

	if event.InputTokens != 100 || event.OutputTokens != 50 {
		t.Errorf("usage must survive translation: %+v", event)
	}
}
