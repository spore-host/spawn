// Package shellmin strips the comments from spawn's own shell script text
// before it goes into EC2 user data (#1246).
package shellmin

import (
	"regexp"
	"strings"
)

// heredocStart matches a heredoc operator and captures its delimiter word,
// with or without quotes and with or without the tab-stripping "-". The
// leading group keeps a here-string (<<<) from reading as one.
var heredocStart = regexp.MustCompile(`(?:^|[^<])<<(-?)\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// Strip returns src without its whole-line comments and blank lines.
// Every other line is kept byte for byte, indentation included, and so is
// everything inside a heredoc body.
//
// It exists for the user-data budget (#1246). EC2 allows 16 KiB of user data
// after base64 decoding, and the bootstrap's static parts are documented
// inline: comments were 16.5 KB of its 39 KB, and the bootstrap alone was
// 12.8 KB of the 16 KiB once gzipped, leaving a task about 1.6 KB. Stripping
// them at build time keeps the documentation in the source, where it is read,
// and out of the bytes EC2 counts.
//
// It is a line filter, not a parser, so pkg/launcher's
// TestMinifiedTemplatesParseTheSame holds it to account: every bootstrap
// template must parse (mvdan.cc/sh, the parser
// behind shfmt) to the same syntax tree with and without the filter. That is
// what catches a removed line that was not a comment, such as one inside a
// multi-line quoted string or after a backslash continuation.
//
// Only spawn's own static script text goes through here. Caller-supplied
// content (the command, storage, container and custom user-data scripts) is
// added to the bootstrap as given: a line starting with # inside it may be
// data.
func Strip(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	delim, stripTabs := "", false
	for _, line := range strings.SplitAfter(src, "\n") {
		if delim != "" {
			// Inside a heredoc body: keep everything, watch for the end.
			b.WriteString(line)
			end := strings.TrimRight(line, "\n")
			if stripTabs {
				end = strings.TrimLeft(end, "\t")
			}
			if end == delim {
				delim = ""
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || (strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "#!")) {
			continue
		}
		b.WriteString(line)
		if m := heredocStart.FindStringSubmatch(line); m != nil {
			delim, stripTabs = m[2], m[1] == "-"
		}
	}
	return b.String()
}
