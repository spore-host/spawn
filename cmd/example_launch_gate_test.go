package cmd

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/spore-host/spawn/internal/treewalk"
)

// The example gate (#1540, extending #1532's usage-line test): every
// `spawn launch` invocation in examples/ — in a README's code, a Snakefile
// rule, a WDL command, an Argo template, a CWL tool or a Python subprocess
// list — must parse with today's launch flags and pass launch's own checks.
// #1540 was a file handed to --params, which takes inline JSON, in nine
// files; nothing ran the examples, so nothing noticed.
//
// Placeholders. Examples are templates: {SWEEP_FILE} (Snakemake),
// ~{params_file} (WDL), {{workflow.parameters.sweep-file}} (Argo),
// <sweep-id>, $VAR, $(cmd), … and a Python expression in a subprocess list
// all stand for a value the user or the engine fills in. isPlaceholder
// recognises them by those characters. A placeholder is accepted wherever a
// value goes (a flag's value, the spore name), is replaced by a typed sample
// for a non-string flag so the flag still parses, and is never opened as a
// file. The one place it is refused is the value of --params: there it can
// only stand for a file, the exact mistake this gate exists for, since inline
// JSON is written literally.

// exampleLaunch is one `spawn launch` invocation found in an example.
type exampleLaunch struct {
	where string   // file:line
	dir   string   // the example file's directory, for relative paths
	argv  []string // the words after `spawn launch`
	// fragment: inline code in prose (`spawn launch --efa`), which names a
	// flag rather than giving a whole command.
	fragment bool
}

// partial reports whether an invocation is not a whole command — a prose
// fragment, or one that elides the rest with … — so it is held to its flags
// and not to the spore-name rule.
func (l exampleLaunch) partial() bool {
	if l.fragment {
		return true
	}
	for _, a := range l.argv {
		if a == "…" || a == "..." {
			return true
		}
	}
	return false
}

// placeholderMarker stands for a CWL input's value. A non-literal Python
// expression in a subprocess list (params_file, str(output_file)) becomes
// {expr:<the expression>}, a placeholder that keeps its text.
const placeholderMarker = "{expr}"

func isPlaceholder(s string) bool {
	return strings.ContainsAny(s, "{}<>$…") || s == "..."
}

var (
	shellLaunch  = regexp.MustCompile(`\bspawn\s+launch\b`)
	pyListLaunch = regexp.MustCompile(`\[\s*['"]spawn['"]\s*,\s*['"]launch['"]`)
	pyQuoted     = regexp.MustCompile(`^(?:[rbuf]{0,2})(['"])(.*?)['"]$`)
)

// findExampleLaunches walks examples/ and extracts every invocation.
func findExampleLaunches(t *testing.T, root string) []exampleLaunch {
	t.Helper()
	return findExampleInvocations(t, root, invocationPattern{shell: shellLaunch, py: pyListLaunch, cwl: true})
}

// invocationPattern says which invocations findExampleInvocations extracts.
// A regexp with a capture group starts the extracted argv at the group (so
// `spawn sweep status x` yields [sweep status x]); one without starts it after
// the match (so `spawn launch x` yields [x]).
type invocationPattern struct {
	shell, py *regexp.Regexp
	// cwl: also read `baseCommand: [spawn, launch]` CWL tools.
	cwl bool
	// quotedInPython: in a .py file, take a shell form only inside a string
	// literal, so a docstring's "launch a spawn sweep and wait" is prose.
	quotedInPython bool
}

// findExampleInvocations walks root and extracts every invocation p matches.
func findExampleInvocations(t *testing.T, root string, p invocationPattern) []exampleLaunch {
	t.Helper()
	var out []exampleLaunch
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !exampleTextFile(d.Name()) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		lines := strings.Split(string(b), "\n")
		switch {
		case strings.HasSuffix(path, ".md"):
			out = append(out, shellInvocations(path, lines, markdownCode(lines), p)...)
		case strings.HasSuffix(path, ".py"):
			out = append(out, pythonInvocations(path, lines, p.py)...)
			out = append(out, shellInvocations(path, lines, nil, p)...)
		case strings.HasSuffix(path, ".cwl"):
			if p.cwl {
				out = append(out, cwlLaunches(path, lines)...)
			}
		default:
			out = append(out, shellInvocations(path, lines, nil, p)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func exampleTextFile(name string) bool {
	switch filepath.Ext(name) {
	case ".md", ".py", ".wdl", ".cwl", ".sh", ".sbatch", ".yaml", ".yml", ".nf", ".smk":
		return true
	}
	return name == "Snakefile"
}

// markdownCode marks the lines of a Markdown file that are code: only those,
// and inline `code` spans in prose, are read, since prose may say "spawn
// launch" in a sentence.
func markdownCode(lines []string) []bool {
	inFence := make([]bool, len(lines))
	fenced := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		inFence[i] = fenced
	}
	return inFence
}

// argvStart is where the argv of a match of a pattern's regexp begins: at its
// capture group if it has one, else after the match.
func argvStart(loc []int) int {
	if len(loc) >= 4 && loc[2] >= 0 {
		return loc[2]
	}
	return loc[1]
}

// shellInvocations extracts shell-form invocations. With code non-nil (a
// Markdown file), a line outside a fence contributes only its backtick spans.
func shellInvocations(path string, lines []string, code []bool, p invocationPattern) []exampleLaunch {
	var out []exampleLaunch
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		for _, loc := range p.shell.FindAllStringSubmatchIndex(line, -1) {
			start := loc[0]
			// The delimiter a quoted or inline-code invocation ends at.
			var closer byte
			if start > 0 {
				switch c := line[start-1]; c {
				case '`', '\'', '"':
					closer = c
				}
			}
			if code != nil && !code[i] && closer != '`' {
				continue // prose
			}
			if p.quotedInPython && closer == 0 && strings.HasSuffix(path, ".py") {
				continue // a docstring or comment
			}
			text, last := line[argvStart(loc):], i
			if closer != 0 {
				j := strings.IndexByte(text, closer)
				if j >= 0 {
					rest := text[j+1:]
					text = text[:j]
					if !strings.HasSuffix(path, ".py") || strings.TrimSpace(rest) != "" {
						j = -2 // closed on this line
					}
				}
				if j != -2 && strings.HasSuffix(path, ".py") {
					// 'spawn launch ' '--flag ...': implicit concatenation.
					for last+1 < len(lines) {
						next := strings.TrimSpace(lines[last+1])
						m := pyQuoted.FindStringSubmatch(strings.TrimRight(next, ",)"))
						if m == nil {
							break
						}
						text += m[2]
						last++
					}
				}
			} else {
				// A usage line in a comment continues in the next comment line.
				comment := strings.HasPrefix(strings.TrimSpace(line[:start]), "#")
				for strings.HasSuffix(strings.TrimSpace(text), `\`) && last+1 < len(lines) {
					next := lines[last+1]
					if comment {
						next = strings.TrimPrefix(strings.TrimSpace(next), "#")
					}
					text = strings.TrimSuffix(strings.TrimSpace(text), `\`) + " " + next
					last++
				}
			}
			out = append(out, exampleLaunch{
				where: path + ":" + strconv.Itoa(i+1), dir: filepath.Dir(path), argv: shellWords(text),
				fragment: closer == '`',
			})
		}
	}
	return out
}

// shellWords splits a command on spaces, honouring quotes, and stops at the
// first shell operator or comment: what follows is not spawn's.
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord, inAngle := false, false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case inAngle:
			// <sweep-id>: a placeholder, whose > is not a redirection.
			cur.WriteRune(r)
			inAngle = r != '>'
		case r == '<':
			cur.WriteRune(r)
			inWord, inAngle = true, true
		case r == '→':
			// A diagram's arrow (process → spawn launch … → runs task).
			flush()
			return words
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			flush()
		case r == '|' || r == ';' || r == '&' || r == '>':
			// A redirection's fd (the 2 of 2>&1) is not a word either.
			if r == '>' && inWord && strings.Trim(cur.String(), "0123456789") == "" {
				cur.Reset()
				inWord = false
			}
			flush()
			return words
		case !inWord && (r == '#' || r == ')'):
			return words
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return words
}

// pythonInvocations extracts ['spawn', 'launch', ...] subprocess lists (or
// whatever re matches), which may span lines. A string literal is a word;
// anything else is a placeholder that keeps the expression's text, so
// {expr:sweep_id} still says what it stands for.
func pythonInvocations(path string, lines []string, re *regexp.Regexp) []exampleLaunch {
	var out []exampleLaunch
	src := strings.Join(lines, "\n")
	for _, loc := range re.FindAllStringSubmatchIndex(src, -1) {
		from := argvStart(loc)
		end := strings.IndexByte(src[from:], ']')
		if end < 0 {
			continue
		}
		var argv []string
		for _, item := range strings.Split(src[from:from+end], ",") {
			item = strings.TrimSpace(item)
			if i := strings.Index(item, "#"); i >= 0 && !strings.ContainsAny(item[:i], `'"`) {
				item = strings.TrimSpace(item[:i])
			}
			if item == "" {
				continue
			}
			if m := pyQuoted.FindStringSubmatch(item); m != nil && !strings.HasPrefix(item, "f") {
				argv = append(argv, m[2])
			} else {
				argv = append(argv, "{expr:"+item+"}")
			}
		}
		line := 1 + strings.Count(src[:loc[0]], "\n")
		out = append(out, exampleLaunch{where: path + ":" + strconv.Itoa(line), dir: filepath.Dir(path), argv: argv})
	}
	return out
}

// cwlLaunches reads a `baseCommand: [spawn, launch]` tool's input bindings
// as flags, each bound to a placeholder value (or none, for a boolean).
func cwlLaunches(path string, lines []string) []exampleLaunch {
	src := strings.Join(lines, "\n")
	if !regexp.MustCompile(`baseCommand:\s*\[\s*spawn\s*,\s*launch\s*\]`).MatchString(src) {
		return nil
	}
	var argv []string
	inputType := ""
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "type:"); ok {
			inputType = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, "prefix:"); ok {
			argv = append(argv, strings.TrimSpace(v))
			switch inputType {
			case "boolean":
			case "File":
				argv = append(argv, "{File}")
			default:
				argv = append(argv, placeholderMarker)
			}
		}
	}
	return []exampleLaunch{{where: path, dir: filepath.Dir(path), argv: argv}}
}

// typedSample is what a placeholder value becomes for a flag of a type a
// placeholder would not parse as.
func typedSample(flagType string) (string, bool) {
	switch flagType {
	case "int", "int32", "int64", "uint", "float64", "float32":
		return "1", true
	case "duration":
		return "1h", true
	case "bool":
		return "true", true
	}
	return "", false
}

// checkExampleLaunch runs one invocation (t only resets the flags at cleanup) through launch's flag parser, its
// name rule, the sweep flag rules and the param-source checks.
func checkExampleLaunch(t *testing.T, l exampleLaunch) error {
	t.Helper()
	keepLaunchGlobals(t)
	fs := launchCmd.Flags()
	argv := append([]string(nil), l.argv...)
	for i := 0; i+1 < len(argv); i++ {
		name, ok := strings.CutPrefix(argv[i], "--")
		if !ok || strings.Contains(name, "=") || !isPlaceholder(argv[i+1]) {
			continue
		}
		if f := fs.Lookup(name); f != nil {
			if sample, ok := typedSample(f.Value.Type()); ok && f.Value.Type() != "bool" {
				argv[i+1] = sample
			}
		}
	}
	positional, err := parseExampleLaunchArgv(argv)
	if err != nil && l.partial() && strings.Contains(err.Error(), "flag needs an argument") {
		// A fragment may end at a flag: "spawn launch … --user-data".
		positional, err = parseExampleLaunchArgv(append(argv, "…"))
	}
	if err != nil {
		return fmt.Errorf("spawn launch rejects the flags %q: %v", l.argv, err)
	}
	if err := launchNameArgs(launchCmd, positional); err != nil && !l.partial() {
		return fmt.Errorf("spawn launch %q: %v", l.argv, err)
	}
	if isSweepLaunch() {
		if err := sweepUnsupportedFlags(fs); err != nil {
			return fmt.Errorf("spawn launch %q: %v", l.argv, err)
		}
	}
	if err := validateSweepParamFlags(paramFile, params); err != nil {
		return fmt.Errorf("spawn launch %q: %v", l.argv, err)
	}
	if params != "" {
		if isPlaceholder(params) {
			return fmt.Errorf("spawn launch %q: --params takes inline JSON, and a placeholder (%s) there stands "+
				"for a file: use --param-file", l.argv, params)
		}
		if _, err := parseInlineParams(params); err != nil {
			return fmt.Errorf("spawn launch %q: %v", l.argv, err)
		}
	}
	// A concrete param file that exists (relative to the example, the repo
	// root or examples/) must be one. A placeholder or a file the example
	// only describes is not opened.
	if paramFile != "" && !isPlaceholder(paramFile) {
		for _, base := range []string{l.dir, "..", filepath.Join("..", "examples")} {
			p := filepath.Join(base, paramFile)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			pf, err := parseParamFile(p)
			if err != nil {
				return fmt.Errorf("spawn launch %q: %s: %v", l.argv, p, err)
			}
			if err := validateTopLevelParamKeys(p, pf.UnknownTopLevelKeys); err != nil {
				return fmt.Errorf("spawn launch %q: %v", l.argv, err)
			}
			if err := validateSweepParamKeys(pf); err != nil {
				return fmt.Errorf("spawn launch %q: %v", l.argv, err)
			}
			break
		}
	}
	return nil
}

// TestExampleLaunchInvocationsParse is the gate.
func TestExampleLaunchInvocationsParse(t *testing.T) {
	launches := findExampleLaunches(t, filepath.Join("..", "examples"))
	if len(launches) < 20 {
		t.Fatalf("found %d `spawn launch` invocations in examples/, want at least 20: the scan is broken", len(launches))
	}
	for _, l := range launches {
		t.Run(l.where, func(t *testing.T) {
			if err := checkExampleLaunch(t, l); err != nil {
				t.Errorf("%s: %v", l.where, err)
			}
		})
	}
}

// TestExampleLaunchGateCatchesParamsGivenAFile: the gate fails each form
// #1540 found, so a revert of the fix to the examples fails CI.
func TestExampleLaunchGateCatchesParamsGivenAFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "```bash\nspawn launch my-sweep --params sweeps/hp.yaml\n```\n"+
		"Run `spawn launch --params …` from a rule.\n")
	write("Snakefile", "rule a:\n    shell:\n        \"\"\"\n        spawn launch \\\n            --params {SWEEP_FILE} \\\n            --detach\n        \"\"\"\n")
	write("dag.py", "cmd = ['spawn', 'launch', '--params', params_file,\n       '--detach']\n"+
		"op = BashOperator(bash_command='spawn launch --params sweep.yaml --detach --output-id /tmp/id.txt')\n")
	write("tool.cwl", "baseCommand: [spawn, launch]\ninputs:\n  p:\n    type: File\n    inputBinding:\n      prefix: --params\n")
	write("mpi.md", "```bash\nspawn launch --param-file p.yaml --mpi\n```\n")

	launches := findExampleLaunches(t, dir)
	if len(launches) != 7 {
		t.Fatalf("found %d invocations, want 7: %+v", len(launches), launches)
	}
	for _, l := range launches {
		t.Run(l.where, func(t *testing.T) {
			err := checkExampleLaunch(t, l)
			if err == nil {
				t.Fatalf("the gate accepted %q", l.argv)
			}
			t.Log(err)
		})
	}
}

// parseExampleLaunchArgv parses argv as spawn launch's flags and sets them on
// launchCmd, returning the positional arguments. It parses into a copy of the
// flag definitions and then sets each value as setLaunchFlag does: pflag's
// own Set on a slice flag marks an internal "changed" that nothing can clear,
// so a later test's --tag would append to this one's. The caller is under
// keepLaunchGlobals.
func parseExampleLaunchArgv(argv []string) ([]string, error) {
	clone := pflag.NewFlagSet("launch", pflag.ContinueOnError)
	clone.SetOutput(io.Discard)
	define := func(f *pflag.Flag) {
		if clone.Lookup(f.Name) != nil {
			return
		}
		switch f.Value.Type() {
		case "bool":
			clone.BoolP(f.Name, f.Shorthand, false, "")
		case "stringSlice":
			clone.StringSliceP(f.Name, f.Shorthand, nil, "")
		default:
			if _, ok := f.Value.(pflag.SliceValue); ok {
				clone.StringArrayP(f.Name, f.Shorthand, nil, "")
			} else {
				clone.StringP(f.Name, f.Shorthand, "", "")
			}
		}
	}
	launchCmd.Flags().VisitAll(define)
	launchCmd.InheritedFlags().VisitAll(define)
	if err := clone.Parse(argv); err != nil {
		return nil, err
	}
	var setErr error
	clone.Visit(func(c *pflag.Flag) {
		f := launchCmd.Flags().Lookup(c.Name)
		if f == nil {
			f = launchCmd.InheritedFlags().Lookup(c.Name)
		}
		if f == nil || setErr != nil {
			return
		}
		var err error
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			err = sv.Replace(c.Value.(pflag.SliceValue).GetSlice())
		} else {
			err = f.Value.Set(c.Value.String())
		}
		if err != nil {
			setErr = fmt.Errorf("invalid argument %q for --%s: %v", c.Value.String(), c.Name, err)
		}
		f.Changed = true
	})
	return clone.Args(), setErr
}
