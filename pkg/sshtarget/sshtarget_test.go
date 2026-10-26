package sshtarget

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// The exact argv `spawn status` built for the stopped instance in #928: the
// destination is "ec2-user@" because PublicIP was empty. ssh resolved that to
// this machine's own sshd (::1). It must never be built.
func TestCommand_RefusesTheStoppedInstanceArgvFromTheIssue(t *testing.T) {
	argv := []string{
		"-i", "/home/u/.ssh/spawn-key",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"ec2-user@", "sudo /usr/local/bin/spored status",
	}
	cmd, err := Command(context.Background(), "ssh", argv...)
	if err == nil {
		t.Fatalf("built %v for an empty host", cmd.Args)
	}
	if !errors.Is(err, ErrNoHost) {
		t.Errorf("err = %v, want ErrNoHost", err)
	}
}

func TestCheckHost(t *testing.T) {
	refuse := []string{
		"", "  ", "localhost", "LOCALHOST", "localhost.", "foo.localhost", "ip6-localhost", "ip6-loopback",
		"127.0.0.1", "127.1.2.3", "::1", "[::1]", "0:0:0:0:0:0:0:1", "::ffff:127.0.0.1",
		"0.0.0.0", "::", "[::]", "-oProxyCommand=evil",
	}
	for _, h := range refuse {
		if err := CheckHost(h); err == nil {
			t.Errorf("CheckHost(%q) = nil, want refusal", h)
		}
	}
	allow := []string{"54.12.3.4", "ec2-54-12-3-4.us-west-2.compute.amazonaws.com", "2600:1f14::5", "[2600:1f14::5]", "box.c1d2.spore.host", "10.0.0.5"}
	for _, h := range allow {
		if err := CheckHost(h); err != nil {
			t.Errorf("CheckHost(%q) = %v, want nil", h, err)
		}
	}
}

func TestDestination(t *testing.T) {
	if _, err := Destination("ec2-user", ""); !errors.Is(err, ErrNoHost) {
		t.Errorf("Destination with empty host: %v", err)
	}
	if d, err := Destination("ubuntu", "54.1.2.3"); err != nil || d != "ubuntu@54.1.2.3" {
		t.Errorf("Destination = %q, %v", d, err)
	}
}

func TestRemoteHosts(t *testing.T) {
	cases := []struct {
		name string
		prog string
		args []string
		want []string
	}{
		{"ssh user@host cmd", "ssh", []string{"-i", "k", "-p", "22", "u@1.2.3.4", "ls", "-la"}, []string{"1.2.3.4"}},
		{"option value looks like host", "ssh", []string{"-L", "7777:127.0.0.1:7777", "-N", "u@5.6.7.8"}, []string{"5.6.7.8"}},
		{"joined option value", "ssh", []string{"-ik", "-oBatchMode=yes", "5.6.7.8"}, []string{"5.6.7.8"}},
		{"flag cluster then value", "ssh", []string{"-tti", "k", "u@5.6.7.8"}, []string{"5.6.7.8"}},
		{"jump hosts", "ssh", []string{"-J", "a@9.9.9.9,localhost", "u@5.6.7.8"}, []string{"9.9.9.9", "localhost", "5.6.7.8"}},
		{"ssh url", "ssh", []string{"ssh://u@[::1]:2222"}, []string{"::1"}},
		{"double dash", "ssh", []string{"-i", "k", "--", "u@"}, []string{""}},
		{"scp upload", "scp", []string{"-P", "2222", "-i", "k", "/tmp/a:b", "u@5.6.7.8:/tmp/x"}, []string{"5.6.7.8"}},
		{"scp empty host", "scp", []string{"./f", "u@:/tmp/x"}, []string{""}},
		{"scp v6", "scp", []string{"f", "u@[::1]:/tmp"}, []string{"::1"}},
		{"path to ssh", "/usr/bin/ssh", []string{"u@5.6.7.8"}, []string{"5.6.7.8"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RemoteHosts(c.prog, c.args)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("RemoteHosts = %q, want %q", got, c.want)
			}
		})
	}
	for _, bad := range [][]string{{"-i", "k"}, {"-i"}} {
		if _, err := RemoteHosts("ssh", bad); err == nil {
			t.Errorf("RemoteHosts(ssh %v) = nil error, want refusal (no destination)", bad)
		}
	}
	if _, err := RemoteHosts("scp", []string{"a", "b"}); err == nil {
		t.Error("scp with no remote operand accepted")
	}
	if _, err := RemoteHosts("bash", []string{"-c", "ssh u@"}); err == nil {
		t.Error("a non-ssh program was accepted")
	}
}

func TestCommand_RefusesEveryForbiddenHostAnywhereInTheVector(t *testing.T) {
	for _, argv := range [][]string{
		{"u@localhost"},
		{"-J", "::1", "u@5.6.7.8"},
		{"u@127.0.0.1", "uptime"},
	} {
		if _, err := Command(context.Background(), "ssh", argv...); err == nil {
			t.Errorf("Command(ssh %v) built", argv)
		}
	}
	if _, err := Command(context.Background(), "scp", "f", "u@0.0.0.0:/x"); err == nil {
		t.Error("scp to 0.0.0.0 built")
	}
	cmd, err := Command(context.Background(), "ssh", "-i", "k", "u@5.6.7.8", "uptime")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args[1:], " "); got != "-i k u@5.6.7.8 uptime" {
		t.Errorf("args = %q", got)
	}
}

// TestEverySSHExecGoesThroughTheGuard is the gate for #928: no code outside
// this package may start an ssh or scp client itself, because a client started
// anywhere else skips the empty/loopback-host check. It parses every non-test
// Go file in the repository (nested modules included), finds each os/exec
// Command/CommandContext call, and fails on any whose program is ssh or scp —
// either as a literal, or as a parameter of a wrapper some caller passes "ssh"
// or "scp" (the shape cmd/service.go's runQuietly had).
func TestEverySSHExecGoesThroughTheGuard(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	guardDir := filepath.Dir(here)

	type wrapper struct {
		dir, name string
		param     int
	}
	var (
		violations []string
		execCalls  int
		guardExecs int
		wrappers   []wrapper
		files      = map[string][]*ast.File{} // by directory
		fset       = token.NewFileSet()
	)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "bin", ".wt":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		files[filepath.Dir(path)] = append(files[filepath.Dir(path)], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	isClient := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			return false
		}
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(v)), ".exe")
		return base == "ssh" || base == "scp" || base == "sftp"
	}

	for dir, fs := range files {
		for _, f := range fs {
			execName := ""
			for _, imp := range f.Imports {
				if imp.Path.Value == `"os/exec"` {
					execName = "exec"
					if imp.Name != nil {
						execName = imp.Name.Name
					}
				}
			}
			if execName == "" {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				params := map[string]int{}
				idx := 0
				for _, field := range fn.Type.Params.List {
					for _, n := range field.Names {
						params[n.Name] = idx
						idx++
					}
					if len(field.Names) == 0 {
						idx++
					}
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || pkg.Name != execName {
						return true
					}
					nameArg := -1
					switch sel.Sel.Name {
					case "Command":
						nameArg = 0
					case "CommandContext":
						nameArg = 1
					}
					if nameArg < 0 || len(call.Args) <= nameArg {
						return true
					}
					execCalls++
					arg := call.Args[nameArg]
					if dir == guardDir {
						guardExecs++
						return true
					}
					if isClient(arg) {
						violations = append(violations, fset.Position(call.Pos()).String()+": starts an ssh/scp client directly")
					}
					if id, ok := arg.(*ast.Ident); ok {
						if p, ok := params[id.Name]; ok {
							wrappers = append(wrappers, wrapper{dir: dir, name: fn.Name.Name, param: p})
						}
					}
					return true
				})
			}
		}
	}

	// Second pass: a wrapper is only a violation when a caller hands it ssh/scp.
	for _, w := range wrappers {
		for _, f := range files[w.dir] {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				}
				if name == w.name && len(call.Args) > w.param && isClient(call.Args[w.param]) {
					violations = append(violations, fset.Position(call.Pos()).String()+
						": passes an ssh/scp client to "+w.name+", which execs it directly")
				}
				return true
			})
		}
	}

	// A discovery that found nothing has broken, not proven the repo clean.
	if len(files) == 0 || execCalls == 0 {
		t.Fatalf("discovery found %d Go directories and %d exec calls under %s — the scan is broken", len(files), execCalls, root)
	}
	if guardExecs == 0 {
		t.Fatal("found no exec call in pkg/sshtarget itself — the scan cannot see the guard it protects")
	}
	for _, v := range violations {
		t.Errorf("%s — use sshtarget.Command so an empty or loopback host is refused (#928)", v)
	}
}
