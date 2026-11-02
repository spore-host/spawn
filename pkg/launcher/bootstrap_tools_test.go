package launcher

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// #1490: the Linux bootstrap got its instance ID from ec2-metadata and read
// every spawn:* tag with the aws CLI, and stock Canonical Ubuntu ships
// neither. Both failures were silenced (`| cut` exits 0; `2>/dev/null ||
// echo <default>`), so on every Ubuntu launch the hostname, the job-array
// environment, the MOTD and the --command once-marker were wrong with nothing
// said.
//
// linuxBaseTools are the external commands the bootstrap may assume on every
// supported Linux base: Amazon Linux 2, Amazon Linux 2023 (the default AMI)
// and Canonical's Ubuntu 22.04 and 24.04 server cloud images. Each names the
// package that provides it on Amazon Linux, where every launch already runs
// the bootstrap, and on Ubuntu, where the package is in both releases'
// server-cloudimg manifests (cloud-images.ubuntu.com/releases/{jammy,noble}/
// release/*-server-cloudimg-arm64.manifest, checked 2026-11-01). Adding a
// tool here is a claim about all four: check the manifests, or install the
// tool first, or call it only behind `command -v` (linuxGuardedTools).
var linuxBaseTools = map[string]string{
	"awk":         "gawk / gawk, mawk",
	"base64":      "coreutils / coreutils",
	"bash":        "bash / bash",
	"cat":         "coreutils / coreutils",
	"chmod":       "coreutils / coreutils",
	"chown":       "coreutils / coreutils",
	"cp":          "coreutils / coreutils",
	"curl":        "curl (curl-minimal on AL2023) / curl",
	"cut":         "coreutils / coreutils",
	"date":        "coreutils / coreutils",
	"dirname":     "coreutils / coreutils",
	"grep":        "grep / grep",
	"hostname":    "hostname / hostname",
	"hostnamectl": "systemd / systemd",
	"id":          "coreutils / coreutils",
	"logger":      "util-linux / bsdutils",
	"mkdir":       "coreutils / coreutils",
	"mkfifo":      "coreutils / coreutils",
	"mktemp":      "coreutils / coreutils",
	"mountpoint":  "util-linux / util-linux",
	"mv":          "coreutils / coreutils",
	"openssl":     "openssl / openssl",
	"poweroff":    "systemd / systemd-sysv",
	"ps":          "procps-ng / procps",
	"rm":          "coreutils / coreutils",
	"sed":         "sed / sed",
	"sha256sum":   "coreutils / coreutils",
	"shutdown":    "systemd / systemd-sysv",
	"sleep":       "coreutils / coreutils",
	"su":          "util-linux / util-linux",
	"systemctl":   "systemd / systemd",
	"tee":         "coreutils / coreutils",
	"touch":       "coreutils / coreutils",
	"tr":          "coreutils / coreutils",
	"uname":       "coreutils / coreutils",
	"useradd":     "shadow-utils / passwd",
	"usermod":     "shadow-utils / passwd",
}

// linuxGuardedTools are absent from some supported base, so the bootstrap
// may call them only where it has checked for them first: inside an `if`
// or `&&` whose condition runs `command -v <tool>`, or after a
// `command -v <tool> ... || return` in the same function.
var linuxGuardedTools = map[string]string{
	"aws":            "not on Ubuntu; spawn_fatal_retire's terminate (before spawn_need_aws can have run)",
	"restorecon":     "not on Ubuntu (no SELinux); spore_selinux_relabel",
	"timeout":        "coreutils everywhere, but spawn_is_spot_replacement guards it anyway",
	"dnf":            "AL2023 only; the optional acpid install",
	"yum":            "AL2 (and AL2023's dnf alias); the optional acpid install",
	"selinuxenabled": "not on Ubuntu; spore_selinux_relabel",
}

// linuxInstalledTools are not on every base, and the bootstrap provides them
// before it uses them. Each must be called only after its provider ran
// (checked by TestBootstrapUsesAWSOnlyAfterProvidingIt for aws), or only
// inside the function that provides it.
var linuxInstalledTools = map[string]string{
	"aws": "spawn_need_aws installs the pinned AWS CLI v2 when the image has none (#1490)",
	// spored itself, which the verified installer puts there before the
	// bootstrap starts it.
	"/usr/local/bin/spored": "pkg/sporedinstall's verified install",
	// The official installer, from the sha256-checked zip spawn_need_aws unpacks.
	"/tmp/spawn-awscli/aws/install": "the AWS CLI zip's own installer (spawn_need_aws)",
	// Unpacks that zip (Ubuntu has no unzip). Only reached on an image with no
	// aws CLI, and every such supported base (Ubuntu 22.04/24.04: python3
	// 3.10/3.12, which cloud-init runs on) ships it.
	"python3": "only inside spawn_need_aws",
}

// toolScopes are tools that may only be called inside the named function.
var toolScopes = map[string]string{
	"python3":                       "spawn_need_aws",
	"/tmp/spawn-awscli/aws/install": "spawn_need_aws",
}

// toolMatrix renders every shape of the bootstrap whose text spawn writes:
// inline and stub (with the published body spliced in where the stub sources
// it, so the result runs in the order an instance runs it), with and without
// a key, a command, a flush hook, staged files, gates, plugins and a seeded
// launch config. The storage, container and custom user-data scripts are
// other packages' (or the user's) and are left out.
func toolMatrix(t *testing.T) map[string]string {
	t.Helper()
	flush := "#!/bin/bash\necho flush\n"
	cfgs := map[string]BootstrapConfig{
		"plain":           {Username: "ec2-user"},
		"key and command": {Username: "ec2-user", PublicKey: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample u@h"), Command: "echo hi", CompletionFile: "/data/done"},
		"task":            {Username: "ec2-user", Command: "/opt/task/wrapper.sh", TaskFlushScript: flush},
		"gates":           {Username: "ec2-user", Command: "ls /efs", ReadyGates: []string{MPIReadyGate}},
		"plugins":         {Username: "ec2-user", Plugins: stubConfigs()["plugins"].Plugins},
		"launch config":   {Username: "ec2-user", LaunchConfig: []byte(`{"v":1}`), ReadsLaunchConfig: true},
	}
	cfgs["staged task"] = staged(cfgs["task"])
	cfgs["staged command"] = staged(BootstrapConfig{Username: "ec2-user", Command: "echo hi"})
	body := BootstrapBody()
	out := map[string]string{}
	for name, cfg := range cfgs {
		for _, version := range []string{"", testRelease} {
			script, err := buildLinuxBootstrap(cfg, version, version, body)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if version != "" {
				src := ". " + bodyPath + "\n"
				if strings.Count(script, src) != 1 {
					t.Fatalf("%s: the stub does not source the body once (%q)", name, src)
				}
				script = strings.Replace(script, src, body, 1)
			}
			out[fmt.Sprintf("%s/release=%q", name, version)] = script
		}
	}
	return out
}

// toolCall is one command a rendered script runs.
type toolCall struct {
	name   string
	offset uint   // in the script it was found in (0 inside a nested here-doc)
	fn     string // the enclosing function, "" at top level
	nested bool   // inside a here-doc that is itself a script
}

// toolCalls discovers every command a rendered script runs: the first word of
// each simple command, the program after exec, and the same inside every
// here-doc that is itself a script (a #! line, or written to a .sh file).
// Functions the script defines are not tools. Each call is returned with the
// tools it is guarded for: run only after `command -v <tool>` succeeded.
func toolCalls(t *testing.T, name, script string, nested bool) (calls []toolCall, guarded []toolCall) {
	t.Helper()
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), name)
	if err != nil {
		t.Fatalf("%s: rendered bootstrap does not parse: %v", name, err)
	}
	funcs := map[string]bool{}
	syntax.Walk(f, func(n syntax.Node) bool {
		if fd, ok := n.(*syntax.FuncDecl); ok {
			funcs[fd.Name.Value] = true
		}
		return true
	})
	var visit func(n syntax.Node, fn string, guards map[string]bool)
	var heredocs []string
	record := func(c *syntax.CallExpr, fn string, guards map[string]bool) {
		if len(c.Args) == 0 {
			return
		}
		for i, w := range []string{c.Args[0].Lit(), ""} {
			if i == 1 {
				if c.Args[0].Lit() != "exec" || len(c.Args) < 2 {
					break
				}
				w = c.Args[1].Lit()
			}
			if w == "" || funcs[w] {
				continue
			}
			tc := toolCall{name: w, offset: c.Pos().Offset(), fn: fn, nested: nested}
			calls = append(calls, tc)
			if guards[w] {
				guarded = append(guarded, tc)
			}
		}
	}
	// commandV returns the tools a node checks for with `command -v`.
	commandV := func(n syntax.Node) map[string]bool {
		m := map[string]bool{}
		syntax.Walk(n, func(n syntax.Node) bool {
			if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) >= 3 && c.Args[0].Lit() == "command" && c.Args[1].Lit() == "-v" {
				m[c.Args[2].Lit()] = true
			}
			return true
		})
		return m
	}
	with := func(g map[string]bool, add map[string]bool) map[string]bool {
		out := map[string]bool{}
		for k := range g {
			out[k] = true
		}
		for k := range add {
			out[k] = true
		}
		return out
	}
	visitStmts := func(stmts []*syntax.Stmt, fn string, guards map[string]bool) {
		for _, st := range stmts {
			visit(st, fn, guards)
			// `command -v X ... || return`: the rest of the function may use X.
			if b, ok := st.Cmd.(*syntax.BinaryCmd); ok && b.Op == syntax.OrStmt && fn != "" {
				if c, ok := b.Y.Cmd.(*syntax.CallExpr); ok && len(c.Args) > 0 && c.Args[0].Lit() == "return" {
					guards = with(guards, commandV(b.X))
				}
			}
		}
	}
	visit = func(n syntax.Node, fn string, guards map[string]bool) {
		switch x := n.(type) {
		case *syntax.Stmt:
			for _, r := range x.Redirs {
				var target string
				for _, r2 := range x.Redirs {
					if r2.Op == syntax.RdrOut || r2.Op == syntax.AppOut {
						target = r2.Word.Lit()
					}
				}
				if (r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc) && r.Hdoc != nil {
					body := r.Hdoc.Lit()
					if strings.HasPrefix(body, "#!") || strings.HasSuffix(target, ".sh") {
						heredocs = append(heredocs, body)
					}
				}
			}
			if x.Cmd != nil {
				visit(x.Cmd, fn, guards)
			}
		case *syntax.CallExpr:
			record(x, fn, guards)
			for _, a := range x.Args {
				visitWord(a, fn, guards, visit)
			}
			for _, a := range x.Assigns {
				if a.Value != nil {
					visitWord(a.Value, fn, guards, visit)
				}
			}
		case *syntax.FuncDecl:
			visit(x.Body, x.Name.Value, map[string]bool{})
		case *syntax.Block:
			visitStmts(x.Stmts, fn, guards)
		case *syntax.Subshell:
			visitStmts(x.Stmts, fn, guards)
		case *syntax.IfClause:
			for c := x; c != nil; c = c.Else {
				g := with(guards, commandV(&syntax.Block{Stmts: c.Cond}))
				visitStmts(c.Cond, fn, g)
				visitStmts(c.Then, fn, g)
			}
		case *syntax.BinaryCmd:
			visit(x.X, fn, guards)
			if x.Op == syntax.AndStmt {
				visit(x.Y, fn, with(guards, commandV(x.X)))
			} else {
				visit(x.Y, fn, guards)
			}
		case *syntax.WhileClause:
			visitStmts(x.Cond, fn, guards)
			visitStmts(x.Do, fn, guards)
		case *syntax.ForClause:
			visitStmts(x.Do, fn, guards)
		case *syntax.CaseClause:
			for _, it := range x.Items {
				visitStmts(it.Stmts, fn, guards)
			}
		case *syntax.DeclClause:
			for _, a := range x.Args {
				if a.Value != nil {
					visitWord(a.Value, fn, guards, visit)
				}
			}
		case *syntax.TestClause, *syntax.ArithmCmd, *syntax.LetClause:
			syntax.Walk(x, func(n syntax.Node) bool {
				if cs, ok := n.(*syntax.CmdSubst); ok {
					visitStmts(cs.Stmts, fn, guards)
					return false
				}
				return true
			})
		}
	}
	visitStmts(f.Stmts, "", map[string]bool{})
	for i, body := range heredocs {
		c, g := toolCalls(t, fmt.Sprintf("%s here-doc %d", name, i), body, true)
		calls = append(calls, c...)
		guarded = append(guarded, g...)
	}
	return calls, guarded
}

// visitWord visits the command substitutions inside a word.
func visitWord(w *syntax.Word, fn string, guards map[string]bool, visit func(syntax.Node, string, map[string]bool)) {
	syntax.Walk(w, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst:
			for _, st := range x.Stmts {
				visit(st, fn, guards)
			}
			return false
		case *syntax.ProcSubst:
			for _, st := range x.Stmts {
				visit(st, fn, guards)
			}
			return false
		}
		return true
	})
}

func linuxShellBuiltins(t *testing.T) map[string]bool {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs bash")
	}
	out, err := exec.Command("bash", "-c", "compgen -b; compgen -k").Output()
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	b := map[string]bool{}
	for _, f := range strings.Fields(string(out)) {
		b[f] = true
	}
	return b
}

// TestBootstrapToolsExistOnEveryBase: every command any shape of the Linux
// bootstrap runs is a builtin, one of its own functions, a base tool of every
// supported Linux base, a tool it provides first, or one it calls only after
// checking for it. ec2-metadata (the instance ID) and an unguarded aws (every
// tag read) were neither on Ubuntu, which is #1490.
func TestBootstrapToolsExistOnEveryBase(t *testing.T) {
	builtins := linuxShellBuiltins(t)
	inventory := map[string][]string{}
	for name, script := range toolMatrix(t) {
		calls, guarded := toolCalls(t, name, script, false)
		isGuarded := map[toolCall]bool{}
		for _, g := range guarded {
			isGuarded[g] = true
		}
		for _, c := range calls {
			if builtins[c.name] {
				continue
			}
			inventory[c.name] = append(inventory[c.name], name)
			if _, ok := linuxBaseTools[c.name]; ok {
				continue
			}
			if scope, ok := toolScopes[c.name]; ok {
				if c.fn != scope {
					t.Errorf("%s: %q runs outside %s (in %q), which is the only place it may", name, c.name, scope, c.fn)
				}
				continue
			}
			if _, ok := linuxInstalledTools[c.name]; ok {
				continue // ordering: TestBootstrapUsesAWSOnlyAfterProvidingIt
			}
			if _, ok := linuxGuardedTools[c.name]; ok && isGuarded[c] {
				continue
			}
			if _, ok := linuxGuardedTools[c.name]; ok {
				t.Errorf("%s: runs %q outside a `command -v %s` check; it is not on every supported base", name, c.name, c.name)
				continue
			}
			t.Errorf("%s: runs %q, which is not a base tool of every supported Linux base (AL2, AL2023, Ubuntu 22.04/24.04), is not installed first and is not checked for (#1490: ec2-metadata and aws are not on Ubuntu)", name, c.name)
		}
	}
	if len(inventory) < 25 {
		t.Fatalf("discovered only %d tools in the bootstrap; the discovery is broken: %v", len(inventory), inventory)
	}
	var tools []string
	for c := range inventory {
		tools = append(tools, c)
	}
	sort.Strings(tools)
	t.Logf("Linux bootstrap tool inventory (%d): %s", len(tools), strings.Join(tools, " "))
}

// TestBootstrapUsesAWSOnlyAfterProvidingIt: aws is not on Ubuntu, so every
// aws call the bootstrap makes must either be behind `command -v aws` or come
// after a call of spawn_need_aws whose failure stops the bootstrap. The body's
// call fails closed with exit; the staged-files fetch, which runs before
// spored, with spawn_fatal_retire.
func TestBootstrapUsesAWSOnlyAfterProvidingIt(t *testing.T) {
	for name, script := range toolMatrix(t) {
		f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), name)
		if err != nil {
			t.Fatal(err)
		}
		// The first fail-closed spawn_need_aws: `spawn_need_aws || <exit or retire>`.
		var provided uint
		found := false
		syntax.Walk(f, func(n syntax.Node) bool {
			b, ok := n.(*syntax.BinaryCmd)
			if !ok || b.Op != syntax.OrStmt || found {
				return true
			}
			c, ok := b.X.Cmd.(*syntax.CallExpr)
			if !ok || len(c.Args) != 1 || c.Args[0].Lit() != "spawn_need_aws" {
				return true
			}
			stops := false
			syntax.Walk(b.Y, func(n syntax.Node) bool {
				if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
					if w := c.Args[0].Lit(); w == "exit" || w == "spawn_fatal_retire" {
						stops = true
					}
				}
				return true
			})
			if stops {
				provided, found = b.Pos().Offset(), true
			}
			return true
		})
		calls, guarded := toolCalls(t, name, script, false)
		isGuarded := map[toolCall]bool{}
		for _, g := range guarded {
			isGuarded[g] = true
		}
		n := 0
		for _, c := range calls {
			if c.name != "aws" || c.nested {
				continue
			}
			n++
			if isGuarded[c] {
				continue
			}
			if !found {
				t.Errorf("%s: calls aws at offset %d, and nothing provides it first (no `spawn_need_aws || exit`): on Ubuntu every such call fails", name, c.offset)
			} else if c.offset < provided {
				t.Errorf("%s: calls aws at offset %d, before spawn_need_aws provides it (offset %d)", name, c.offset, provided)
			}
		}
		if n == 0 {
			t.Fatalf("%s: found no aws calls; the discovery is broken", name)
		}
	}
}

// TestToolScanSeesTheOldInstanceIDAndTagReads proves the discovery sees the
// two #1490 lines as they were, so the gates above are not passing on a blind
// scan: ec2-metadata as a tool, and an aws call with no provider before it.
func TestToolScanSeesTheOldInstanceIDAndTagReads(t *testing.T) {
	const old = `INSTANCE_ID=$(ec2-metadata --instance-id | cut -d " " -f 2)
SESSION_TIMEOUT=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:session-timeout" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "30m")
`
	calls, guarded := toolCalls(t, "the pre-#1490 lines", old, false)
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.name] = true
	}
	if !seen["ec2-metadata"] || !seen["aws"] {
		t.Fatalf("the tool scan misses the pre-#1490 calls: %v", calls)
	}
	if len(guarded) != 0 {
		t.Fatalf("the tool scan counts an unchecked call as guarded: %v", guarded)
	}
}
