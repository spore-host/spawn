package cmd

import (
	"bytes"
	"io"
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
)

// tableWriter is spawn's column aligner. It follows text/tabwriter's
// elastic-tabstop rules (cells are terminated by '\t'; a column's width is set
// by the contiguous block of lines that have a cell in it; a line with no tab
// ends every block) but measures a cell by its display width on a terminal
// rather than its rune count, and does not count ANSI escape sequences at all.
//
// text/tabwriter has no hook for either. Counting runes misaligns every column
// after a CJK cell, because 名前 is two runes but four terminal cells (#909);
// counting escape bytes pushes a coloured cell's column wider than its
// uncoloured header.
type tableWriter struct {
	out     io.Writer
	padding int
	buf     []byte
}

// tableWidthCondition is fixed rather than runewidth's default, which reads
// LANG/LC_ALL and would turn East Asian ambiguous-width characters (such as
// the ✓ in `spawn list`'s spot column) to width 2 under a ja locale. Terminals
// render those narrow unless configured otherwise, and a table's layout must
// not depend on the environment of whoever renders it.
var tableWidthCondition = &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: true}

// ansiCSI matches ANSI CSI sequences (colour, bold, reset) so they occupy no
// width when measuring a cell.
var ansiCSI = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// displayWidth is the number of terminal cells s occupies.
func displayWidth(s string) int {
	if strings.IndexByte(s, '\x1b') >= 0 {
		s = ansiCSI.ReplaceAllString(s, "")
	}
	return tableWidthCondition.StringWidth(s)
}

// Write buffers p. Like text/tabwriter, everything up to and including a
// completed line that contains no tab is formatted and written immediately, so
// output order relative to other writers matches what callers had before.
func (t *tableWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	// Find the last complete tab-free line.
	cut := -1
	start := 0
	for i, c := range t.buf {
		if c != '\n' {
			continue
		}
		if bytes.IndexByte(t.buf[start:i], '\t') < 0 {
			cut = i + 1
		}
		start = i + 1
	}
	if cut >= 0 {
		if err := t.flushN(cut); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Flush formats and writes everything buffered.
func (t *tableWriter) Flush() error {
	return t.flushN(len(t.buf))
}

func (t *tableWriter) flushN(n int) error {
	text := string(t.buf[:n])
	t.buf = append(t.buf[:0], t.buf[n:]...)
	if text == "" {
		return nil
	}
	trailingNewline := strings.HasSuffix(text, "\n")
	if trailingNewline {
		text = text[:len(text)-1]
	}
	rawLines := strings.Split(text, "\n")
	lines := make([][]string, len(rawLines))
	for i, l := range rawLines {
		lines[i] = strings.Split(l, "\t")
	}
	// As in text/tabwriter, an unterminated last line ending in a tab has no
	// trailing cell, so its last tab-terminated cell is not part of a column.
	if last := lines[len(lines)-1]; !trailingNewline && len(last) > 1 && last[len(last)-1] == "" {
		lines[len(lines)-1] = last[:len(last)-1]
	}

	var out bytes.Buffer
	t.format(&out, lines, nil, 0, len(lines))
	if trailingNewline {
		out.WriteByte('\n')
	}
	_, err := t.out.Write(out.Bytes())
	return err
}

// format is text/tabwriter's recursive block algorithm: widths holds the
// widths of the columns already fixed for lines [line0, line1); the next
// column's width is taken from each maximal run of lines that have a
// terminated cell in it.
func (t *tableWriter) format(out *bytes.Buffer, lines [][]string, widths []int, line0, line1 int) {
	column := len(widths)
	for this := line0; this < line1; this++ {
		if column >= len(lines[this])-1 {
			continue
		}
		t.writeLines(out, lines, widths, line0, this)
		line0 = this
		width := 0
		for ; this < line1; this++ {
			if column >= len(lines[this])-1 {
				break
			}
			if w := displayWidth(lines[this][column]) + t.padding; w > width {
				width = w
			}
		}
		t.format(out, lines, append(widths, width), line0, this)
		line0 = this
	}
	t.writeLines(out, lines, widths, line0, line1)
}

func (t *tableWriter) writeLines(out *bytes.Buffer, lines [][]string, widths []int, line0, line1 int) {
	for i := line0; i < line1; i++ {
		for j, cell := range lines[i] {
			out.WriteString(cell)
			if j < len(widths) && j < len(lines[i])-1 {
				if pad := widths[j] - displayWidth(cell); pad > 0 {
					out.WriteString(strings.Repeat(" ", pad))
				}
			}
		}
		if i+1 < len(lines) {
			out.WriteByte('\n')
		}
	}
}
