// rc13 coordinator regression test: scrubRecord must not mutate the
// caller's ToolCalls array. Root cause of a data race found by -race
// in TestApprovalCallbackWhileTurnSlotsFull (pre-existing on rc12,
// 3/10 runs): Transcript.AppendMessage scrubbed tool-call arguments
// in place on the caller's shared ToolCalls backing array, racing
// with a concurrent FileStore Save marshal of that same array
// (Shutdown saves while a turn is still appending). The fix copies
// the slice before scrubbing; this test pins the no-mutation
// property deterministically, without needing -race timing.
package session

import (
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
)

func TestRC13AppendMessageDoesNotMutateCallerToolCalls(t *testing.T) {
	tr := mustOpen(t, t.TempDir(), NewID())
	// Synthetic placeholder only — never a real key. RedactSecrets
	// rewrites this shape, so on the old code the call below
	// mutated the caller's array in place.
	const fakeArgs = `{"api_key": "sk-faketoken00000000"}`
	msg := model.Message{
		Role:    "assistant",
		Content: "calling the tool",
		ToolCalls: []model.ToolCall{
			{ID: "call-1", Name: "some_tool", Arguments: fakeArgs},
		},
	}
	if err := tr.AppendMessage(msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if got := msg.ToolCalls[0].Arguments; got != fakeArgs {
		t.Fatalf("caller ToolCalls[0].Arguments = %q, want unchanged %q (scrub must work on a copy, not the caller's array)", got, fakeArgs)
	}
	// The persisted record must still be redacted — the fix moves
	// the scrub onto a copy, it must not drop it.
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 1 || recs[0].Message == nil || len(recs[0].Message.ToolCalls) != 1 {
		t.Fatalf("Records = %+v, want one message record with one tool call", recs)
	}
	if got := recs[0].Message.ToolCalls[0].Arguments; strings.Contains(got, "sk-faketoken") {
		t.Errorf("persisted Arguments still contains the fake secret: %q", got)
	}
}
