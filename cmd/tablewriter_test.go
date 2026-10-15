package cmd

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/mattn/go-runewidth"
	"github.com/spore-host/libs/i18n"
)

// tableTestCSI strips colour escapes so the test measures what a terminal shows.
var tableTestCSI = regexp.MustCompile("\x1b\\[[0-9;]*m")

// termWidth is how many terminal cells s occupies, measured independently of
// the code under test's displayWidth.
func termWidth(s string) int {
	return (&runewidth.Condition{}).StringWidth(tableTestCSI.ReplaceAllString(s, ""))
}

// columnStarts returns the terminal column at which each of cells begins in a
// rendered line.
func columnStarts(t *testing.T, line string, cells []string) []int {
	t.Helper()
	starts := make([]int, len(cells))
	pos := 0
	for i, c := range cells {
		idx := strings.Index(line[pos:], c)
		if idx < 0 {
			t.Fatalf("cell %q not found in rendered line %q", c, line)
		}
		starts[i] = termWidth(line[:pos+idx])
		pos += idx + len(c)
	}
	return starts
}

// TestTableWriterAlignsJapaneseByDisplayWidth is #909 where it lives: `spawn
// list`'s real ja header row above mixed-width and coloured cells must have
// every column start at the same terminal column on every line. text/tabwriter
// pads by rune count, so each two-cell-wide CJK character shifted later columns
// left by one, and each colour escape shifted them right.
func TestTableWriterAlignsJapaneseByDisplayWidth(t *testing.T) {
	ja, err := i18n.NewLocalizer(i18n.Config{Language: "ja"})
	if err != nil {
		t.Fatalf("ja localizer: %v", err)
	}
	header := []string{
		ja.T("spawn.list.header.instance_id"),
		ja.T("spawn.list.header.name"),
		ja.T("spawn.list.header.type"),
		ja.T("spawn.list.header.state"),
		ja.T("spawn.list.header.iam_role"),
		ja.T("spawn.list.header.age"),
		ja.T("spawn.list.header.spot"),
	}
	if header[1] == "spawn.list.header.name" || termWidth(header[1]) == len([]rune(header[1])) {
		t.Fatalf("ja name header %q is not double-width text; the test would not exercise #909", header[1])
	}
	rows := [][]string{
		header,
		{"i-0123456789abcdef0", "ウェブサーバー", "m7i.large", "\x1b[32mrunning\x1b[0m", "-", "3h", "✓"},
		{"i-0fedcba9876543210", "db", "t3.micro", "\x1b[33mstopped\x1b[0m", "研究ロール", "12m", ""},
		{"i-0aaaaaaaaaaaaaaaa", "mixed名前x", "c7g.16xlarge", "terminated", "role", "2d", "✓"},
	}

	var out bytes.Buffer
	w := newTableWriter(&out)
	for _, r := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("rendered %d lines, want %d:\n%s", len(lines), len(rows), out.String())
	}
	want := columnStarts(t, lines[0], rows[0])
	for i := 1; i < len(rows); i++ {
		got := columnStarts(t, lines[i], rows[i])
		for j := range got {
			if rows[i][j] == "" {
				continue // an empty last cell has no visible start
			}
			if got[j] != want[j] {
				t.Errorf("line %d column %d (%q) starts at terminal column %d, header's starts at %d\n%s",
					i, j, rows[i][j], got[j], want[j], out.String())
			}
		}
	}
	// Padding is spaced from the widest cell, not merely consistent.
	for j := 1; j < len(want); j++ {
		widest := 0
		for _, r := range rows {
			if w := termWidth(r[j-1]); w > widest {
				widest = w
			}
		}
		if gap := want[j] - want[j-1]; gap != widest+2 {
			t.Errorf("column %d is %d cells wide, want widest cell %d + padding 2", j-1, gap, widest)
		}
	}
}

// TestTableWriterGolden pins the exact bytes for a small CJK table, so the
// alignment check above cannot pass on an output shape no one intended.
func TestTableWriterGolden(t *testing.T) {
	var out bytes.Buffer
	w := newTableWriter(&out)
	_, _ = fmt.Fprint(w, "名前\tx\nab\ty\n\x1b[1mz\x1b[0m\tw\n")
	_ = w.Flush()
	want := "名前  x\nab    y\n\x1b[1mz\x1b[0m     w\n"
	if out.String() != want {
		t.Errorf("got\n%q\nwant\n%q", out.String(), want)
	}
}

// TestTableWriterMatchesTabwriterOnASCII: for text where a rune is one cell
// wide and there are no escapes, the replacement must be byte-for-byte what
// every table in spawn printed through text/tabwriter before #909, including
// its block rules for ragged rows and tab-free lines.
func TestTableWriterMatchesTabwriterOnASCII(t *testing.T) {
	inputs := []string{
		"",
		"a\tb\tc\n",
		"a\tb\tc",
		"h1\th2\nlong-value\tx\n",
		"a\tb\tc\nlonger\tb\nx\n\nzz\tyyyyy\tq\n",
		"title\nA\tB\tC\naaaa\tb\nheading\n1\t22\t333\n",
		"trailing\t\nx\t\t\n\t\tend\n",
		"a\tb\n\nc\td\n",
	}
	rng := &splitmix{state: 909}
	pieces := []string{"", "a", "bb", "cccc", "ddddddd", "x y"}
	for n := 0; n < 300; n++ {
		var sb strings.Builder
		for l := rng.Intn(6); l >= 0; l-- {
			for c := rng.Intn(4); c >= 0; c-- {
				sb.WriteString(pieces[rng.Intn(len(pieces))])
				if c > 0 {
					sb.WriteByte('\t')
				}
			}
			if l > 0 || rng.Intn(2) == 0 {
				sb.WriteByte('\n')
			}
		}
		inputs = append(inputs, sb.String())
	}
	for _, in := range inputs {
		var want, got bytes.Buffer
		tw := tabwriter.NewWriter(&want, 0, 0, 2, ' ', 0)
		_, _ = tw.Write([]byte(in))
		_ = tw.Flush()
		w := newTableWriter(&got)
		// Feed it a byte at a time to exercise the incremental flush.
		for i := 0; i < len(in); i++ {
			_, _ = w.Write([]byte{in[i]})
		}
		_ = w.Flush()
		if got.String() != want.String() {
			t.Errorf("input %q:\n got %q\nwant %q", in, got.String(), want.String())
		}
	}
}

// TestTableWriterWritesThroughAtTabFreeLine keeps tabwriter's output timing: a
// completed line with no tab is written immediately (with everything before
// it), so a caller interleaving direct prints is not reordered.
func TestTableWriterWritesThroughAtTabFreeLine(t *testing.T) {
	var out bytes.Buffer
	w := newTableWriter(&out)
	_, _ = fmt.Fprint(w, "a\tb\nSection\nc\td\n")
	if got, want := out.String(), "a  b\nSection\n"; got != want {
		t.Fatalf("before Flush got %q, want %q", got, want)
	}
	_ = w.Flush()
	if got, want := out.String(), "a  b\nSection\nc  d\n"; got != want {
		t.Fatalf("after Flush got %q, want %q", got, want)
	}
}

// splitmix is a tiny deterministic generator for the randomized comparison
// above. The codebase carries no math/rand (see pkg/agent/dns_lease.go), and a
// test only needs a reproducible sequence, not randomness.
type splitmix struct{ state uint64 }

func (s *splitmix) Intn(n int) int {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z % uint64(n))
}
