// Package curlcompat answers one question for the repo's gates: does a shell
// script call curl with an option that the oldest curl spawn's install paths run
// under does not have?
//
// Every Linux script spawn generates — the boot bootstrap, the app user-data, the
// SSM spored upgrade, the pipeline stage script — runs on whatever curl the AMI
// ships. The oldest one spawn launches is EL8 (Rocky/RHEL 8), curl 7.61.1. The
// spored installer used --retry-all-errors (curl 7.71+), which 7.61.1 rejects
// with exit 2; the installer hid curl's stderr, so every source looked like a
// network failure, the bootstrap aborted, and the instance ran with no TTL and no
// cost limit (#1056).
//
// The allowed option set is not written from memory. It is parsed from the real
// `curl --help` of the rockylinux:8 image, committed as curl-7.61.1-help.txt
// (CLAUDE.md, "Writing a gate", rule 4: where a list is unavoidable, take it from
// the real thing).
package curlcompat

import (
	_ "embed"
	"regexp"
	"strings"
)

// Floor names the curl the allowed option set was captured from.
const Floor = "curl 7.61.1 (EL8)"

//go:embed curl-7.61.1-help.txt
var el8Help string

// helpLine matches an option line of `curl --help`:
//
//	-a, --append        Append to target file when uploading
//	    --abstract-unix-socket <path> Connect via ...
var helpLine = regexp.MustCompile(`^\s+(?:(-\S),\s+)?(--[A-Za-z0-9][A-Za-z0-9.-]*)`)

// Options is the set of curl options a curl release accepts.
type Options struct {
	Long  map[string]bool // "--retry"
	Short map[byte]bool   // 'f'
}

// EL8 returns the options curl 7.61.1 accepts.
func EL8() Options { return parseHelp(el8Help) }

func parseHelp(help string) Options {
	o := Options{Long: map[string]bool{}, Short: map[byte]bool{}}
	for _, line := range strings.Split(help, "\n") {
		m := helpLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] != "" {
			o.Short[m[1][1]] = true
		}
		o.Long[m[2]] = true
	}
	return o
}

// Invocation is one curl command line found in a script.
type Invocation struct {
	Line int    // 1-based
	Text string // the whole source line, trimmed
	Args []string
}

// invocation finds a curl command, or a call to the installer's retry wrapper
// (spore_curl_retry <retries> <delay> <curl args...>), whose arguments go to curl
// verbatim. The command must start a word: "libcurl" or "$CURL_BIN" do not match.
var invocation = regexp.MustCompile(`(?:^|[\s;|&(!{"'` + "`" + `])(?:curl|spore_curl_retry\s+\S+\s+\S+)(\s.*)?$`)

// commandEnd cuts a command line at the end of the curl command: a pipe, a list
// operator, or the close of a command substitution.
var commandEnd = regexp.MustCompile(`\|\||&&|[|;)]`)

// Invocations returns every curl command line in script. Comment lines — shell
// "#" and Go "//" — are skipped, so prose about an option does not count as using
// it (rule 3: match the invocation, never the bare name).
func Invocations(script string) []Invocation {
	var out []Invocation
	for i, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "*") {
			continue
		}
		m := invocation.FindStringSubmatchIndex(line)
		if m == nil {
			continue
		}
		rest := ""
		if m[2] >= 0 {
			rest = line[m[2]:m[3]]
		}
		if loc := commandEnd.FindStringIndex(rest); loc != nil {
			rest = rest[:loc[0]]
		}
		out = append(out, Invocation{Line: i + 1, Text: line, Args: strings.Fields(rest)})
	}
	return out
}

// Unsupported returns the options in inv that o does not accept.
func (o Options) Unsupported(inv Invocation) []string {
	var bad []string
	for _, a := range inv.Args {
		switch {
		case a == "--":
			return bad
		case strings.HasPrefix(a, "--"):
			name, _, _ := strings.Cut(a, "=")
			if !o.Long[name] {
				bad = append(bad, name)
			}
		case len(a) > 1 && a[0] == '-':
			// A cluster of short options, e.g. -fsSL or -sX. Stop at the first
			// character that is not a letter or digit: in "-o/tmp/x" the rest is
			// the option's value.
			for j := 1; j < len(a); j++ {
				c := a[j]
				if !isAlnum(c) && c != '#' && c != ':' {
					break
				}
				if !o.Short[c] {
					bad = append(bad, "-"+string(c))
				}
			}
		}
	}
	return bad
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
