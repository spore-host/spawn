package userdata

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// #1483: the MPI user data built its hostfile with jq, which stock Amazon
// Linux 2 does not ship and spawn never installed. Under the bootstrap's
// set -e the script died there (127) on every AL2 member, before mpirun and
// before any completion record, so the cohort billed to its TTL.
//
// mpiBaseTools are the external commands the MPI user data may assume on every
// supported MPI base (the default AL2023 AMI and an AL2 --ami). Each was run by
// the script on a stock AL2 EC2 instance in #1483 (all but jq succeeded) or is
// in the AL2 and AL2023 AMIs' base package set, named beside it. Adding a tool
// here is a claim about every base: check it on both, or install it in the
// script and list it in mpiInstalledTools.
var mpiBaseTools = map[string]string{
	"awk":          "gawk",
	"bash":         "bash",
	"cat":          "coreutils",
	"chmod":        "coreutils",
	"cut":          "coreutils",
	"dirname":      "coreutils",
	"mkdir":        "coreutils",
	"mkfifo":       "coreutils",
	"nproc":        "coreutils",
	"rm":           "coreutils",
	"sleep":        "coreutils",
	"tee":          "coreutils",
	"touch":        "coreutils",
	"tr":           "coreutils",
	"grep":         "grep",
	"hostname":     "hostname",
	"ps":           "procps-ng",
	"ssh":          "openssh-clients",
	"ssh-keygen":   "openssh",
	"curl":         "curl (curl-minimal on AL2023); EFA path only",
	"tar":          "tar; EFA path only",
	"yum":          "yum (dnf's yum alias on AL2023)",
	"ec2-metadata": "ec2-utils / amazon-ec2-utils; only when the bootstrap did not set INSTANCE_ID",
}

// mpiInstalledTools are commands the script installs before it uses them.
var mpiInstalledTools = map[string]string{
	"mpirun": "yum install -y openmpi openmpi-devel, or the image (--skip-mpi-install)",
	// The EFA installer's own script, fetched and unpacked by the EFA block.
	"./efa_installer.sh": "aws-efa-installer-latest.tar.gz",
}

// mpiConfigMatrix renders every shape of the MPI user data: rank 0 and a peer,
// with and without --mpi-command, install or not, EFA or not, gate or not.
func mpiConfigMatrix() map[string]MPIConfig {
	m := map[string]MPIConfig{}
	for _, rank := range []int{0, 1} {
		for _, cmd := range []string{"", "./sim --x"} {
			for _, skip := range []bool{false, true} {
				for _, efa := range []bool{false, true} {
					for _, gate := range []string{"", "/run/spawn/mpi-ready"} {
						for _, ppn := range []int{0, 8} {
							name := fmt.Sprintf("rank%d,cmd=%q,skip=%v,efa=%v,gate=%q,ppn=%d", rank, cmd, skip, efa, gate, ppn)
							m[name] = MPIConfig{Region: "us-east-1", JobArrayID: "a", JobArrayIndex: rank, JobArraySize: 2,
								MPIProcessesPerNode: ppn, MPICommand: cmd, SkipInstall: skip, EFAEnabled: efa, ReadyGate: gate}
						}
					}
				}
			}
		}
	}
	return m
}

// shellBuiltins asks bash for its builtins and keywords rather than listing
// them, so a builtin is never mistaken for a tool the base must ship.
func shellBuiltins(t *testing.T) map[string]bool {
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

// scriptCommands discovers every command a rendered script runs: the first
// word of each simple command, the program after exec, and the same inside
// every here-doc that is itself a script (a #! line, or written to a .sh file).
// The --mpi-command here-doc is the user's, not spawn's, so it is not one.
// Functions the script defines, and ones it calls only behind a
// `declare -F <name>` guard (the bootstrap's), are not tools.
func scriptCommands(t *testing.T, name, script string) map[string]bool {
	t.Helper()
	f, err := syntax.NewParser().Parse(strings.NewReader(script), name)
	if err != nil {
		t.Fatalf("%s: rendered MPI user data does not parse: %v", name, err)
	}
	calls, funcs := map[string]bool{}, map[string]bool{}
	var nested []string
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.FuncDecl:
			funcs[x.Name.Value] = true
		case *syntax.CallExpr:
			if len(x.Args) == 0 {
				return true
			}
			w := x.Args[0].Lit()
			calls[w] = true
			if w == "exec" && len(x.Args) > 1 {
				calls[x.Args[1].Lit()] = true
			}
		case *syntax.DeclClause:
			// declare -F <name>: a function the bootstrap may have defined.
			if x.Variant.Value == "declare" {
				for _, a := range x.Args {
					if a.Naked && a.Name != nil {
						funcs[a.Name.Value] = true
					}
				}
			}
		case *syntax.Stmt:
			var body, target string
			for _, r := range x.Redirs {
				switch r.Op {
				case syntax.Hdoc, syntax.DashHdoc:
					if r.Hdoc != nil {
						body = r.Hdoc.Lit()
					}
				case syntax.RdrOut, syntax.AppOut:
					target = r.Word.Lit()
				}
			}
			if body != "" && (strings.HasPrefix(body, "#!") || strings.HasSuffix(target, ".sh")) {
				nested = append(nested, body)
			}
		}
		return true
	})
	for i, body := range nested {
		for c := range scriptCommands(t, fmt.Sprintf("%s here-doc %d", name, i), body) {
			calls[c] = true
		}
	}
	delete(calls, "")
	for fn := range funcs {
		delete(calls, fn)
	}
	return calls
}

// TestMPIUserDataToolsExistOnEveryBase: every command any shape of the MPI
// user data runs is a builtin, one of its own functions, a base tool of every
// supported MPI base, or something it installs first. jq is none of these on
// AL2, which is #1483.
func TestMPIUserDataToolsExistOnEveryBase(t *testing.T) {
	builtins := shellBuiltins(t)
	inventory := map[string][]string{}
	for name, cfg := range mpiConfigMatrix() {
		script, err := GenerateMPIUserData(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for c := range scriptCommands(t, name, script) {
			if builtins[c] {
				continue
			}
			inventory[c] = append(inventory[c], name)
		}
	}
	if len(inventory) < 10 {
		t.Fatalf("discovered only %d tools in the MPI user data; the discovery is broken: %v", len(inventory), inventory)
	}
	var tools []string
	for c, where := range inventory {
		tools = append(tools, c)
		if _, ok := mpiBaseTools[c]; ok {
			continue
		}
		if _, ok := mpiInstalledTools[c]; ok {
			continue
		}
		sort.Strings(where)
		t.Errorf("the MPI user data runs %q, which is not a base tool of every supported MPI base and is not installed first (#1483: jq on AL2 died with 127 and no completion record); e.g. in %s", c, where[0])
	}
	sort.Strings(tools)
	t.Logf("MPI user-data tool inventory: %s", strings.Join(tools, " "))
}

// TestMPIToolScanFindsJq proves the discovery sees a jq call where the #1483
// hostfile step had one, so the gate above is not passing on a blind scan.
func TestMPIToolScanFindsJq(t *testing.T) {
	const old = `SLOTS=1
jq -r ".[] | \"\(.ip) slots=$SLOTS\"" /etc/spawn/job-array-peers.json > /tmp/mpi-hostfile
`
	if !scriptCommands(t, "the #1483 hostfile line", old)["jq"] {
		t.Fatal("the tool scan does not see the jq call of the #1483 hostfile line")
	}
}
