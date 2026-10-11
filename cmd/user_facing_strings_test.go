package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every command and path the tool PRINTS must be real (#806).
//
// #790 fixed this for one function's recommendations, and the gate I added then
// covered only that function — which is the #799 lesson again: a gate scoped to
// where the author happened to be looking. The same defect was live in seven
// more places, including the one instruction given at the moment money is
// leaking (`spawn instances list`, printed by drainJobArray when a failed cohort
// may still be billing) and one that **exits 0** (`spawn fsx create` prints group
// help and creates nothing, so a script following the advice reports success).
//
// This generalises it to every string literal in cmd/ and pkg/.
//
// The hard part is telling an invocation from prose. "from the spawn repo" is
// indistinguishable from "spawn repo" to any parser, and resolving-or-not cannot
// be the test — `spawn instances list` does not resolve and that is precisely the
// bug. So a string is treated as an invocation when EITHER:
//
//   - its first word is a real subcommand (so `spawn config init` is checked even
//     though `config` alone resolves — that was the #790 defect), or
//   - it is quoted: backticks, single quotes, or parentheses. Prose is not.
//
// Literals are extracted with go/ast rather than by regex, so comments are
// excluded and both interpreted and raw strings are handled.

// spawnInvocation finds "spawn <word>..." inside a string.
var spawnInvocation = regexp.MustCompile(`\bspawn ((?:[a-z][a-z0-9-]*)(?: +(?:[a-z][a-z0-9-]*|--?[a-z][a-z0-9-]*(?:=[^ ]*)?))*)`)

// userFacingLiterals returns every string literal in the package rooted at dir,
// excluding tests, paired with the file it came from.
type literal struct {
	file string
	text string
}

func userFacingLiterals(t *testing.T, repoRoot string, dirs ...string) []literal {
	t.Helper()
	var out []literal
	fset := token.NewFileSet()

	for _, dir := range dirs {
		err := filepath.Walk(filepath.Join(repoRoot, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// A file that will not parse cannot be silently skipped: the gate
				// would then report a clean bill of health for it.
				t.Fatalf("parse %s: %v", path, perr)
			}
			rel, _ := filepath.Rel(repoRoot, path)
			ast.Inspect(f, func(n ast.Node) bool {
				bl, ok := n.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(bl.Value)
				if uerr != nil {
					return true
				}
				pos := fset.Position(bl.Pos())
				out = append(out, literal{file: rel + ":" + strconv.Itoa(pos.Line), text: s})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(out) == 0 {
		t.Fatal("extracted no string literals — discovery has broken, it has not proven the tree clean")
	}
	return out
}

// quotedAt reports whether the match at idx is wrapped in backticks, single
// quotes or parentheses — i.e. presented as a command rather than as prose.
func quotedAt(s string, idx, end int) bool {
	before, after := byte(0), byte(0)
	if idx > 0 {
		before = s[idx-1]
	}
	if end < len(s) {
		after = s[end]
	}
	// Backticks and single quotes only. Parentheses were in this list and had to
	// come out: parenthesised PROSE is common — "(spawn does not know the stored
	// size)", "(spawn holds the tunnel until you interrupt it)" — so treating an
	// opening paren as a command delimiter flagged five such sentences. The one
	// real finding that used parens, "pre-create it (spawn fsx create)", is caught
	// by the first-word rule instead because `fsx` IS a command. The paren rule
	// bought nothing and cost five false positives.
	// LEADING delimiter only. The trailing half had to go for the same reason as
	// the parens: an apostrophe in ordinary English ends a match and looks like a
	// closing quote — "spawn holds the tunnel until … the instance's lifetime
	// ends" was flagged because the regex stopped before `'s`. A command is
	// introduced by its opening delimiter; that is the half that carries meaning.
	_ = after
	return before == '`' || before == '\''
}

// firstWordIsACommand reports whether the first token resolves as a direct
// subcommand of spawn.
func firstWordIsACommand(word string) bool {
	for _, c := range rootCmd.Commands() {
		if c.Name() == word {
			return true
		}
		for _, a := range c.Aliases {
			if a == word {
				return true
			}
		}
	}
	return false
}

// knownProse is for literals that read as invocations under both rules but are
// not. Each entry needs a reason; this is a list of exceptions, not a list of
// what to check, so a new printed command is gated by default.
var knownProse = map[string]string{
	"spawn schedule reads max":           "prose: `--max-concurrent` is a flag, not a param-file key",
	"spawn schedule reads sweep":         "prose: `--name` is a flag, not a param-file key",
	"spawn encryption key":               "the --description text of an `aws kms create-key` example",
	"spawn plugin host":                  "an SSH-config block marker (`# >>> spawn plugin host <x> >>>`)",
	"spawn task terminal-flush hook":     "a comment header written into a generated script",
	"spawn task wrapper":                 "a comment header written into a generated script",
	"spawn service python3 -m app":       "documents a FAILURE mode — the shorthand -m is deliberately shown being rejected",
	"spawn service python3 -m app.serve": "same, in the error-message example",
}

// Every printed spawn invocation must resolve completely.
func TestPrintedSpawnCommandsResolve(t *testing.T) {
	root := repoRootFromCmd(t)
	lits := userFacingLiterals(t, root, "cmd", "pkg")

	checked := 0
	for _, l := range lits {
		for _, m := range spawnInvocation.FindAllStringSubmatchIndex(l.text, -1) {
			whole := l.text[m[0]:m[1]]
			body := l.text[m[2]:m[3]]
			words := strings.Fields(body)
			if len(words) == 0 {
				continue
			}
			if isKnownProse(whole) {
				continue
			}
			if !firstWordIsACommand(words[0]) && !quotedAt(l.text, m[0], m[1]) {
				continue // prose
			}

			checked++
			if _, problem := resolvePrinted(words); problem != "" {
				t.Errorf("%s prints `spawn %s`: %s\n  in: %q",
					l.file, whole[len("spawn "):], problem, shortLit(l.text))
			}
		}
	}

	if checked == 0 {
		t.Fatal("checked no invocations — the matcher has stopped matching, which is not the same " +
			"as the tree being clean")
	}
	t.Logf("checked %d printed spawn invocations across %d literals", checked, len(lits))
}

// A printed command must not merely resolve — it must DO something.
//
// `spawn fsx create` is the insidious case: cobra prints group help and exits 0
// for an unknown subcommand of a group with no RunE, so a script that follows the
// advice reports success having created nothing. An exit-code check would miss
// it; resolving against the tree is what catches it, and this gate adds the
// second half — the resolved command must be runnable.
func TestPrintedSpawnCommandsAreRunnable(t *testing.T) {
	root := repoRootFromCmd(t)
	lits := userFacingLiterals(t, root, "cmd", "pkg")

	checked := 0
	for _, l := range lits {
		for _, m := range spawnInvocation.FindAllStringSubmatchIndex(l.text, -1) {
			words := strings.Fields(l.text[m[2]:m[3]])
			if len(words) == 0 {
				continue
			}
			if isKnownProse(l.text[m[0]:m[1]]) {
				continue
			}
			if !firstWordIsACommand(words[0]) && !quotedAt(l.text, m[0], m[1]) {
				continue
			}
			var args []string
			for _, w := range words {
				if strings.HasPrefix(w, "-") {
					break
				}
				args = append(args, w)
			}
			c, leftover, err := rootCmd.Find(args)
			if err != nil || len(leftover) > 0 {
				continue // the resolve gate reports this
			}
			checked++
			if c.Runnable() || isFormatTemplate(l.text, m[1]) {
				continue
			}
			// A group with no RunE is fine to NAME (e.g. "see spawn fsx"), but not
			// as an instruction with a verb after it.
			if len(args) > 1 {
				t.Errorf("%s prints `spawn %s`, which resolves to the group %q and is not runnable — "+
					"cobra prints group help and exits 0, so a script following this reports success "+
					"having done nothing.\n  in: %q",
					l.file, strings.Join(args, " "), c.CommandPath(), shortLit(l.text))
			}
		}
	}
	if checked == 0 {
		t.Fatal("checked no resolved invocations — the matcher has stopped matching")
	}
}

// Flags named in a printed command must exist on it.
func TestPrintedSpawnFlagsExist(t *testing.T) {
	root := repoRootFromCmd(t)
	lits := userFacingLiterals(t, root, "cmd", "pkg")

	checked := 0
	for _, l := range lits {
		for _, m := range spawnInvocation.FindAllStringSubmatchIndex(l.text, -1) {
			words := strings.Fields(l.text[m[2]:m[3]])
			if len(words) == 0 {
				continue
			}
			if isKnownProse(l.text[m[0]:m[1]]) {
				continue
			}
			if !firstWordIsACommand(words[0]) && !quotedAt(l.text, m[0], m[1]) {
				continue
			}
			var args, flags []string
			for _, w := range words {
				if strings.HasPrefix(w, "-") {
					name := strings.TrimLeft(w, "-")
					flags = append(flags, strings.SplitN(name, "=", 2)[0])
					continue
				}
				if len(flags) == 0 {
					args = append(args, w)
				}
			}
			if len(flags) == 0 || len(args) == 0 {
				continue
			}
			c, problem := resolvePrinted(args)
			if problem != "" {
				continue // the resolve gate reports this
			}
			for _, f := range flags {
				checked++
				// --help is synthesised by cobra at execution time, so it is not in
				// the flag set here. It is always valid.
				if f == "help" {
					continue
				}
				// ShorthandLookup PANICS on a name longer than one character, so it
				// cannot be used as a blanket fallback probe. That panic is what the
				// first run of this gate produced — a stack trace instead of a
				// finding. And a shorthand like -o is registered as a PERSISTENT
				// flag on the root, so it is reachable only through InheritedFlags;
				// looking it up there by name ("o") finds nothing, because its name
				// is "output". Both halves of that were false positives.
				known := c.Flags().Lookup(f) != nil || c.InheritedFlags().Lookup(f) != nil
				if !known && len(f) == 1 {
					known = c.Flags().ShorthandLookup(f) != nil ||
						c.InheritedFlags().ShorthandLookup(f) != nil
				}
				if !known {
					t.Errorf("%s prints `--%s` on %q, which has no such flag.\n  in: %q",
						l.file, f, c.CommandPath(), shortLit(l.text))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("checked no flags — the matcher has stopped matching")
	}
}

// Repo-relative paths and spore-host URLs the tool prints must exist.
var (
	printedPath    = regexp.MustCompile(`\b(?:docs|infra|deployment|scripts|examples)/[A-Za-z0-9._/<>-]+`)
	printedRepoURL = regexp.MustCompile(`https://github\.com/spore-host/[A-Za-z0-9._-]+/blob/[A-Za-z0-9._-]+/([A-Za-z0-9._/-]+)`)
)

func TestPrintedPathsExist(t *testing.T) {
	root := repoRootFromCmd(t)
	lits := userFacingLiterals(t, root, "cmd", "pkg")

	checked := 0
	for _, l := range lits {
		for _, p := range printedPath.FindAllString(l.text, -1) {
			// A path at the end of a sentence picks up its full stop, and a
			// trailing comma or paren likewise. Reporting "README.md." as missing
			// when README.md exists is a false positive that would teach whoever
			// hits it to distrust this gate.
			p = strings.TrimRight(p, ".,;:)")
			// A path with a placeholder cannot be stat'd; check the directory that
			// contains it instead, which is the part that was wrong in every case
			// #806 found (docs/compliance/ does not exist at all).
			target := p
			if i := strings.IndexAny(p, "<>"); i >= 0 {
				target = filepath.Dir(p[:i])
			}
			checked++
			if _, err := os.Stat(filepath.Join(root, target)); err != nil {
				t.Errorf("%s prints path %q, which does not exist in the repo.\n  in: %q",
					l.file, p, shortLit(l.text))
			}
		}
		for _, m := range printedRepoURL.FindAllStringSubmatch(l.text, -1) {
			checked++
			if _, err := os.Stat(filepath.Join(root, m[1])); err != nil {
				t.Errorf("%s prints %q, whose path %q does not exist in the repo.\n  in: %q",
					l.file, m[0], m[1], shortLit(l.text))
			}
		}
	}
	if checked == 0 {
		t.Fatal("checked no paths — the matcher has stopped matching")
	}
}

func shortLit(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// resolvePrinted walks an invocation's words through the cobra tree the way cobra
// itself does, and reports the first thing that is wrong.
//
// The subtlety that made the first version of this gate useless: "the command
// path is everything up to the first flag" treats POSITIONAL ARGUMENTS as
// subcommands, so `spawn cost my-devbox` was reported as a nonexistent command.
// The rule that actually matches cobra: descend only while the current command is
// a GROUP and the next word names one of its subcommands. A group followed by a
// word that is not one of its subcommands is the defect (`spawn fsx create`). A
// leaf command's trailing words are arguments and are none of this gate's
// business.
func resolvePrinted(words []string) (resolved *cobra.Command, problem string) {
	cur := rootCmd
	for _, w := range words {
		if strings.HasPrefix(w, "-") {
			break
		}
		var next *cobra.Command
		for _, c := range cur.Commands() {
			if c.Name() == w {
				next = c
				break
			}
			for _, a := range c.Aliases {
				if a == w {
					next = c
					break
				}
			}
			if next != nil {
				break
			}
		}
		if next != nil {
			cur = next
			continue
		}
		// No subcommand by that name. Only a problem if the current command is a
		// group that cannot act on its own — a leaf takes arguments.
		if cur.Runnable() {
			break // trailing words are arguments
		}
		if cur == rootCmd {
			return cur, fmt.Sprintf("%q is not a spawn command", w)
		}
		return cur, fmt.Sprintf("%q has no subcommand %q, and is not runnable itself — cobra prints "+
			"group help and exits 0, so a script following this reports success having done nothing",
			cur.CommandPath(), w)
	}
	return cur, ""
}

// isKnownProse matches the FULL phrase, not just the first word. Keying on the
// first word would exempt every `spawn task …` string because one of them is a
// generated-script comment header — turning a precise exception into a blanket
// one, which is how an allowlist stops being an allowlist.
func isKnownProse(phrase string) bool {
	_, ok := knownProse[strings.TrimSpace(phrase)]
	return ok
}

// isFormatTemplate reports whether the invocation is immediately followed by a
// printf verb, i.e. the next word is substituted at runtime.
//
// `use \`spawn notify workspace %s\` instead` becomes `spawn notify workspace add`
// when printed, which is runnable. Demanding runnability of the template text
// flags a string that is correct in every value it ever takes.
func isFormatTemplate(s string, end int) bool {
	rest := strings.TrimLeft(s[min(end, len(s)):], " ")
	return strings.HasPrefix(rest, "%")
}
