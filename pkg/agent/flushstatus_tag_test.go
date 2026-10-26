package agent

import (
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// The tag spored writes is the one `spawn task status` reads (#1295), and the
// "ok" value is the one it treats as a successful flush.
func TestFlushStatusTagMatchesTheReader(t *testing.T) {
	if flushStatusTag != taskproto.FlushStatusTag {
		t.Errorf("spored writes %q, task status reads %q", flushStatusTag, taskproto.FlushStatusTag)
	}
	if got := (flushOutcome{status: flushOK}).tagValue(); got != taskproto.FlushStatusOK {
		t.Errorf("a successful flush is tagged %q, task status expects %q", got, taskproto.FlushStatusOK)
	}
}
