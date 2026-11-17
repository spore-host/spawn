package iamcoverage

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"

	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
)

// markFunc is the lease mark every creating path must pass through first
// (spawn#1654, pkg/lifecyclemark).
const markFunc = "github.com/spore-host/spawn/pkg/lifecyclemark.Mark"

// createActions are, for each resource type spawn's teardown can delete
// (footprintspawn.DeletableTypes), the IAM actions that create one. Whatever
// a teardown may delete must have been marked active before it was made, or a
// teardown racing the create could delete it while it is being set up.
// TestEveryCreatorMarksTheLease fails on a deletable type with no entry here,
// so the set follows the teardown rather than a list of today's call sites.
var createActions = map[string][]string{
	fs.TypeInstance:        {"ec2:RunInstances"},
	fs.TypeVolume:          {"ec2:CreateVolume"},
	fs.TypeSecurityGroup:   {"ec2:CreateSecurityGroup"},
	fs.TypePlacementGroup:  {"ec2:CreatePlacementGroup"},
	fs.TypeKeyPair:         {"ec2:ImportKeyPair", "ec2:CreateKeyPair"},
	fs.TypeEIP:             {"ec2:AllocateAddress"},
	fs.TypeRole:            {"iam:CreateRole"},
	fs.TypeInstanceProfile: {"iam:CreateInstanceProfile"},
	fs.TypeManagedPolicy:   {"iam:CreatePolicy"},
	fs.TypeBucket:          {"s3:CreateBucket"},
	fs.TypeQueue:           {"sqs:CreateQueue"},
	// Not a teardown type, but the reaper reclaims spawn's FSx (#192), so its
	// create is held to the same rule.
	fs.TypeFileSystem: {"fsx:CreateFileSystem"},
}

// creatingExempt are functions that make a creating call that creates
// nothing. Each needs a reason.
var creatingExempt = map[string]string{
	"(*github.com/spore-host/spawn/pkg/doctor.awsProber).EC2LaunchPermission": "RunInstances with DryRun=true: AWS checks the permission and launches nothing",
}

// TestEveryCreatorMarksTheLease: every path from a spawn command to a call
// that creates something spawn's idle teardown could delete passes, first,
// through a function that calls lifecyclemark.Mark (spawn#1654).
//
// The markers are discovered, not listed: every function in the CLI's
// program with a static call to Mark. Each command's reachable code is then
// walked with the markers as cuts (Program.Calls), and any creating action
// still reachable is a path that creates without marking. A marker is trusted
// to mark before it creates; that order, and that a mark never fails the
// create, are pkg/lifecyclemark's and its callers' unit tests.
func TestEveryCreatorMarksTheLease(t *testing.T) {
	creating := map[string]bool{}
	for _, typ := range fs.DeletableTypes() {
		as, ok := createActions[typ]
		if !ok || len(as) == 0 {
			t.Errorf("deletable type %s has no create actions in createActions: add them, or this gate cannot see its creates", typ)
		}
	}
	for _, as := range createActions {
		for _, a := range as {
			creating[a] = true
		}
	}

	prog, err := Load(LoadConfig{Dir: repoRoot, GOOS: "linux", GOARCH: "amd64", Patterns: []string{"."}, Root: repoRoot})
	if err != nil {
		t.Fatal(err)
	}
	var markers []*ssa.Function
	for f := range prog.cg.Nodes {
		if f != nil && callsStatically(f, markFunc) {
			markers = append(markers, f)
		}
	}
	for name := range creatingExempt {
		f, err := prog.Func(name)
		if err != nil {
			t.Errorf("creatingExempt: %v", err)
			continue
		}
		markers = append(markers, f)
	}
	if len(markers) <= len(creatingExempt) {
		t.Fatalf("found no function calling %s: the discovery has broken, or nothing marks", markFunc)
	}

	rootPre, runnable := cliCommands()
	var runFns []*ssa.Function
	creators := 0
	for _, c := range runnable {
		var roots []*ssa.Function
		for _, fn := range append([]any{c.fn}, c.pre...) {
			f, err := ssaFor(prog, fn)
			if err != nil {
				t.Fatalf("%s: %v", c.path, err)
			}
			roots = append(roots, f)
		}
		runFns = append(runFns, roots...)
		if all := prog.Calls(roots, nil); anyCreating(all, creating) {
			creators++
		}
		checkMarked(t, prog, c.path, prog.Calls(roots, markers), creating)
	}
	roots := prog.Mains()
	for _, fn := range rootPre {
		f, err := ssaFor(prog, fn)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, f)
	}
	checkMarked(t, prog, "(every command)", prog.Calls(roots, append(append([]*ssa.Function{}, runFns...), markers...)), creating)
	if creators == 0 {
		t.Fatal("no command reaches a creating action at all: the action mapping or the walk has broken")
	}
	t.Logf("%d marker functions; %d commands create something", len(markers)-len(creatingExempt), creators)
}

func anyCreating(inv Inventory, creating map[string]bool) bool {
	for a := range inv.Actions {
		if creating[a] {
			return true
		}
	}
	return false
}

// checkMarked fails on every creating action inv still reaches.
//
// A site in a closure counts only when the closure's enclosing named function
// is itself reached without a marker. The call graph cannot tell apart the
// closures handed to a shared helper (retryIAMWrite takes a dozen), so a
// helper reached from an unmarked path appears to call every closure any
// caller ever passed it. A closure is only made when its enclosing function
// runs, so it is covered exactly when that function is.
func checkMarked(t *testing.T, prog *Program, what string, inv Inventory, creating map[string]bool) {
	t.Helper()
	var bad []string
	for a, sites := range inv.Actions {
		if !creating[a] {
			continue
		}
		for _, s := range sites {
			if f := prog.byStr[s.Func]; f != nil {
				top := f
				for top.Parent() != nil {
					top = top.Parent()
				}
				if top != f && inv.PathTo(top.String()) == nil {
					continue
				}
			}
			bad = append(bad, a+" at "+s.Pos+" via "+strings.Join(inv.PathTo(s.Func), " -> "))
			break
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s creates without marking the lifecycle lease first (call lifecyclemark.Mark before the create): %s", what, b)
	}
}

// callsStatically reports whether f has a call (or go/defer) whose static
// callee is the function named name.
func callsStatically(f *ssa.Function, name string) bool {
	for _, b := range f.Blocks {
		for _, ins := range b.Instrs {
			c, ok := ins.(ssa.CallInstruction)
			if !ok {
				continue
			}
			if callee := c.Common().StaticCallee(); callee != nil && callee.String() == name {
				return true
			}
		}
	}
	return false
}

// TestLeaseRequiredActionsAreWhatMarkCalls: the actions pkg/lifecyclemark's
// warning names, and the documented baseline repeats, are exactly the
// DynamoDB actions Mark can make (spawn#1654). A list that missed one would
// send a user to grant something that still fails; one that over-asks is a
// grant nothing uses.
func TestLeaseRequiredActionsAreWhatMarkCalls(t *testing.T) {
	prog, err := Load(LoadConfig{Dir: repoRoot, GOOS: "linux", GOARCH: "amd64", Patterns: []string{"."}, Root: repoRoot})
	if err != nil {
		t.Fatal(err)
	}
	mark, err := prog.Func(markFunc)
	if err != nil {
		t.Fatal(err)
	}
	inv := prog.Calls([]*ssa.Function{mark}, nil)
	var got []string
	for a := range inv.Actions {
		if strings.HasPrefix(a, "dynamodb:") {
			got = append(got, a)
		}
	}
	if len(got) == 0 {
		t.Fatal("Mark reaches no DynamoDB action: the walk has broken")
	}
	want := append([]string{}, lifecyclemark.RequiredActions...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Mark calls %v; lifecyclemark.RequiredActions says %v", got, want)
	}
}
