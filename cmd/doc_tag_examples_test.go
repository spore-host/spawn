package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #1244: docs/self-hosted-monitoring.md told users to run
// `spawn launch ... --tag spawn:metrics-enabled=true`, which parseKVTags refuses
// (the spawn: prefix is reserved). Every `--tag key=value` a doc's code block
// passes to a command whose --tag goes through parseKVTags (launch, snapshot
// create, capacity-block purchase; `spawn list --tag` is a filter and is not
// checked) must be one parseKVTags accepts, so a documented command cannot
// fail on its own example.
func TestDocumentedLaunchTagsAreAccepted(t *testing.T) {
	root := repoRootFromCmd(t)
	files, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join(root, "README.md"))

	launch := regexp.MustCompile(`^\s*(\$\s*)?spawn\s+(launch|snapshot\s+create|capacity-block\s+purchase)\b`)
	tagArg := regexp.MustCompile(`--tag[\s=]+("[^"]*"|'[^']*'|\S+)`)
	checked := 0
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // repo docs, discovered by glob
		if err != nil {
			t.Fatal(err)
		}
		for _, cmdLine := range fencedCommands(string(b)) {
			if !launch.MatchString(cmdLine) {
				continue
			}
			for _, m := range tagArg.FindAllStringSubmatch(cmdLine, -1) {
				kv := strings.Trim(m[1], `"'`)
				checked++
				if _, err := parseKVTags([]string{kv}); err != nil {
					t.Errorf("%s documents `--tag %s` on a command, which spawn refuses: %v",
						filepath.Base(f), kv, err)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no `--tag` example on a tagging command in any doc; the discovery is broken, not the docs clean")
	}
}

// fencedCommands returns the logical command lines inside the ``` blocks of a
// Markdown document, with backslash continuations joined.
func fencedCommands(md string) []string {
	var out []string
	in := false
	cur := ""
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			cur = ""
			continue
		}
		if !in {
			continue
		}
		if strings.HasSuffix(strings.TrimRight(line, " "), `\`) {
			cur += strings.TrimSuffix(strings.TrimRight(line, " "), `\`) + " "
			continue
		}
		out = append(out, cur+line)
		cur = ""
	}
	return out
}
