package iamcoverage

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	spawncmd "github.com/spore-host/spawn/cmd"
	"golang.org/x/tools/go/ssa"
)

var update = flag.Bool("update", false, "rewrite "+cliDocPath+" from the code")

// cliDocPath is the generated inventory, relative to the repository root.
const cliDocPath = "docs/reference/cli-iam-actions.md"

const cmdPkg = "github.com/spore-host/spawn/cmd"

// cliPlatforms are the platforms the spawn CLI ships for; a command's actions
// are the union across them.
var cliPlatforms = []string{"darwin", "linux", "windows"}

// cliCommand is one runnable spawn command, the function cobra runs for it,
// and the persistent pre-runs of its ancestors below the root (root.go runs
// every ancestor's, not only the nearest).
type cliCommand struct {
	path string
	fn   any
	pre  []any
}

// cliCommands walks the live command tree. Hidden commands are internal
// plumbing (doc generation, the bootstrap body) and are left out. rootPre are
// the root's persistent pre-runs, which every command inherits.
func cliCommands() (rootPre []any, runnable []cliCommand) {
	root := spawncmd.RootCommand()
	persistentOf := func(c *cobra.Command) []any {
		var out []any
		if c.PersistentPreRunE != nil {
			out = append(out, c.PersistentPreRunE)
		}
		if c.PersistentPreRun != nil {
			out = append(out, c.PersistentPreRun)
		}
		return out
	}
	rootPre = persistentOf(root)
	var walk func(c *cobra.Command, pre []any)
	walk = func(c *cobra.Command, pre []any) {
		if c.Hidden {
			return
		}
		if c != root {
			pre = append(append([]any{}, pre...), persistentOf(c)...)
		}
		switch {
		case c.RunE != nil:
			runnable = append(runnable, cliCommand{path: c.CommandPath(), fn: c.RunE, pre: pre})
		case c.Run != nil:
			runnable = append(runnable, cliCommand{path: c.CommandPath(), fn: c.Run, pre: pre})
		}
		for _, sub := range c.Commands() {
			walk(sub, pre)
		}
	}
	walk(root, nil)
	sort.Slice(runnable, func(i, j int) bool { return runnable[i].path < runnable[j].path })
	return rootPre, runnable
}

// ssaFor matches a function value from the live command tree to its SSA
// function, by declaration position.
func ssaFor(prog *Program, fn any) (*ssa.Function, error) {
	rf := runtime.FuncForPC(reflect.ValueOf(fn).Pointer())
	if rf == nil {
		return nil, fmt.Errorf("no runtime function for %T", fn)
	}
	file, line := rf.FileLine(rf.Entry())
	matches := prog.FuncsAt(cmdPkg, filepath.Base(file), line)
	if len(matches) != 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.String()
		}
		return nil, fmt.Errorf("%s (%s:%d) matched %d SSA functions %v", rf.Name(), filepath.Base(file), line, len(matches), names)
	}
	return matches[0], nil
}

// TestCLIActionInventoryIsCurrent generates the inventory of IAM actions each
// spawn command can call — the other half of spore-host#610: the CLI runs as
// the user, so these are the permissions a user's identity needs — and fails
// when the committed copy differs. Regenerate with `make cli-iam-actions`.
func TestCLIActionInventoryIsCurrent(t *testing.T) {
	rootPre, runnable := cliCommands()
	if len(runnable) < 50 {
		t.Fatalf("found only %d runnable commands; the walk has broken", len(runnable))
	}

	byCommand := map[string]map[string]bool{}
	every := map[string]bool{}
	for _, goos := range cliPlatforms {
		prog, err := Load(LoadConfig{Dir: repoRoot, GOOS: goos, GOARCH: "amd64", Patterns: []string{"."}, Root: repoRoot})
		if err != nil {
			t.Fatal(err)
		}
		var runFns []*ssa.Function
		for _, c := range runnable {
			var roots []*ssa.Function
			for _, fn := range append([]any{c.fn}, c.pre...) {
				f, err := ssaFor(prog, fn)
				if err != nil {
					t.Fatalf("%s (GOOS=%s): %v", c.path, goos, err)
				}
				roots = append(roots, f)
			}
			runFns = append(runFns, roots...)
			inv := prog.Calls(roots, nil)
			failUnmapped(t, c.path, inv)
			if byCommand[c.path] == nil {
				byCommand[c.path] = map[string]bool{}
			}
			for a := range inv.Actions {
				byCommand[c.path][a] = true
			}
		}
		// What runs outside every command's own code: startup, the root's
		// persistent pre-run, and shell completion. Everything reachable from
		// main without entering a command's run function or pre-runs.
		roots := prog.Mains()
		for _, fn := range rootPre {
			f, err := ssaFor(prog, fn)
			if err != nil {
				t.Fatalf("root persistent pre-run (GOOS=%s): %v", goos, err)
			}
			roots = append(roots, f)
		}
		inv := prog.Calls(roots, runFns)
		failUnmapped(t, "(every command)", inv)
		for a := range inv.Actions {
			every[a] = true
		}
	}

	doc := renderCLIInventory(every, byCommand)
	path := filepath.Join(repoRoot, cliDocPath)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil { //nolint:gosec // a doc in the repository
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path) //nolint:gosec // a doc in the repository
	if err != nil {
		t.Fatalf("read %s: %v (generate it with `make cli-iam-actions`)", cliDocPath, err)
	}
	if string(got) != doc {
		t.Errorf("%s is stale: the CLI's AWS calls changed. Regenerate it with `make cli-iam-actions` "+
			"and review the diff — a new action there is a new permission every user needs.", cliDocPath)
	}
}

func failUnmapped(t *testing.T, what string, inv Inventory) {
	t.Helper()
	for _, u := range inv.Unmapped {
		t.Errorf("%s: an AWS operation this inventory cannot map to an IAM action: %s", what, u)
	}
}

func renderCLIInventory(every map[string]bool, byCommand map[string]map[string]bool) string {
	var b strings.Builder
	b.WriteString(`# IAM actions the spawn CLI calls

<!-- Generated by ` + "`make cli-iam-actions`" + ` (internal/iamcoverage). Do not edit by hand. -->

The spawn CLI runs with **your** AWS credentials, so every action below is one
your IAM identity needs for the command that calls it. The list is discovered
from the code, not maintained by hand: the generator type-checks the CLI,
builds its call graph, and records every AWS SDK operation reachable from each
command's run function (see ` + "`internal/iamcoverage`" + `). CI fails when the code
and this page disagree, so a new AWS call cannot ship without appearing here.

How to read it:

- **It over-approximates.** A command is listed with every action any of its
  code paths can reach, including flags you may not use and error paths.
- **It names actions, not resources.** Most of spawn's calls are scoped to
  ` + "`spawn:managed`" + ` resources or ` + "`spawn-*`" + ` names; see the command's documentation.
- **Tag-on-create permissions are listed.** A call that tags what it creates
  is also authorized as the service's tag action: ` + "`ec2:RunInstances`" + ` with tags
  needs ` + "`ec2:CreateTags`" + `, ` + "`sqs:CreateQueue`" + ` with tags needs ` + "`sqs:TagQueue`" + `, an
  S3 upload with tags needs ` + "`s3:PutObjectTagging`" + `. The generator lists the tag
  action wherever the command's code may set the tags.
- **` + "`iam:PassRole`" + ` is listed where a call passes a role.** ` + "`ec2:RunInstances`" + `
  with an instance profile, ` + "`lambda:CreateFunction`" + ` (its execution role),
  ` + "`scheduler:CreateSchedule`" + ` (the schedule's role), ` + "`iam:AddRoleToInstanceProfile`" + `
  and the Image Builder import each also need ` + "`iam:PassRole`" + ` on the role they
  pass. ` + "`iam:PassRole`" + ` is resource-scoped: grant it on the roles spawn creates
  (` + "`spored-instance-role`" + `, ` + "`spawn-instance-*`" + `, and the role the command names),
  not on ` + "`*`" + `.
- **Calls made through the AWS CLI** (` + "`aws ssm start-session`" + `, ` + "`aws s3 cp`" + ` in
  generated scripts) are not Go SDK calls and are not listed.
- ` + "`sts:GetCallerIdentity`" + ` needs no permission and is omitted.

## Outside any command

What runs outside every command's own code: startup, the root command's
persistent pre-run, and shell completion (completing an instance ID lists your
instances):

`)
	if len(every) == 0 {
		b.WriteString("None.\n")
	} else {
		for _, a := range sortedKeys(every) {
			fmt.Fprintf(&b, "- `%s`\n", a)
		}
	}

	b.WriteString("\n## By command\n\n| Command | IAM actions |\n| --- | --- |\n")
	byAction := map[string][]string{}
	for _, cmd := range sortedKeys(byCommand) {
		acts := sortedKeys(byCommand[cmd])
		cell := "none"
		if len(acts) > 0 {
			quoted := make([]string, len(acts))
			for i, a := range acts {
				quoted[i] = "`" + a + "`"
				byAction[a] = append(byAction[a], cmd)
			}
			cell = strings.Join(quoted, ", ")
		}
		fmt.Fprintf(&b, "| `%s` | %s |\n", cmd, cell)
	}

	b.WriteString("\n## By action\n\n| IAM action | Commands |\n| --- | --- |\n")
	actions := make([]string, 0, len(byAction))
	for a := range byAction {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		cmds := byAction[a]
		quoted := make([]string, len(cmds))
		for i, c := range cmds {
			quoted[i] = "`" + c + "`"
		}
		fmt.Fprintf(&b, "| `%s` | %s |\n", a, strings.Join(quoted, ", "))
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
