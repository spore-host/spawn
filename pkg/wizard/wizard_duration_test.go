package wizard

import (
	"bufio"
	"strings"
	"testing"
)

// #990: the wizard re-prompts for a TTL or idle timeout the shared parser
// rejects, instead of handing the launch a value nothing would enforce.
func TestReadDuration(t *testing.T) {
	cases := []struct{ input, want string }{
		{"1d\n", "1d"},
		{"1w\nsoon\n2d12h\n", "2d12h"},
		{"\n", "8h"},
		{"1w\n", "8h"}, // bad value then EOF: the default, never the bad value
	}
	for _, c := range cases {
		w := &Wizard{scanner: bufio.NewScanner(strings.NewReader(c.input))}
		if got := w.readDuration("8h"); got != c.want {
			t.Errorf("input %q: readDuration = %q, want %q", c.input, got, c.want)
		}
	}
}
