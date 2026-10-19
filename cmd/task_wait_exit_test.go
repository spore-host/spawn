package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// taskprotoConsts discovers, from pkg/taskproto's source, every string constant
// declared with the named type (e.g. "RetryClass", "TaskState"). Discovered, not
// listed, so a class added later is covered without anyone remembering this
// test. Fails when discovery finds nothing: that means the parse broke, not that
// there are no classes.
func taskprotoConsts(t *testing.T, typeName string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "pkg", "taskproto", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != typeName {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s constant in %s is not a string literal; extend this discovery", typeName, path)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, s)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("discovered no %s constants in pkg/taskproto; the discovery is broken", typeName)
	}
	return out
}

// TestTaskRunWaitFailsOnAFailedTask is the #1143 gate. `task run --wait` exited
// with the record's exit_code alone, and the wrapper writes the COMMAND's exit
// code even when it then marks the task failed: a declared output that failed to
// upload gives state=failed, retry_class=output_delivery_error, exit_code=0, and
// --wait exited 0. The documented intent (the issue's gate): for each failure
// retry class, --wait must exit non-zero; and any state other than completed is
// not a success. 1 matches `task status --check-complete`.
func TestTaskRunWaitFailsOnAFailedTask(t *testing.T) {
	classes := taskprotoConsts(t, "RetryClass")
	var sawOutputDelivery bool
	for _, c := range classes {
		if c == string(taskproto.RetryNone) {
			continue // no class: success, or unclassified
		}
		if c == string(taskproto.RetryOutputDeliveryError) {
			sawOutputDelivery = true
		}
		rec := &taskproto.CompletionRecord{State: taskproto.StateFailed, ExitCode: 0, RetryClass: taskproto.RetryClass(c)}
		if got := waitExitCode(rec); got != 1 {
			t.Errorf("state=failed retry_class=%s exit_code=0: --wait exits %d, want 1", c, got)
		}
	}
	if !sawOutputDelivery {
		t.Error("discovery did not find output_delivery_error, the class #1143 was filed for")
	}

	for _, s := range taskprotoConsts(t, "TaskState") {
		if s == string(taskproto.StateCompleted) {
			continue
		}
		rec := &taskproto.CompletionRecord{State: taskproto.TaskState(s), ExitCode: 0}
		if got := waitExitCode(rec); got != 1 {
			t.Errorf("state=%s exit_code=0: --wait exits %d, want 1 (only completed is success)", s, got)
		}
	}
	// A record with no state at all is not a completed one either.
	if got := waitExitCode(&taskproto.CompletionRecord{}); got != 1 {
		t.Errorf("record with empty state: --wait exits %d, want 1", got)
	}
}

// The task's own non-zero exit code still wins, as the --wait help promises,
// and a completed task with exit 0 still exits 0.
func TestTaskRunWaitKeepsTheTaskExitCode(t *testing.T) {
	for _, tc := range []struct {
		rec  taskproto.CompletionRecord
		want int
	}{
		{taskproto.CompletionRecord{State: taskproto.StateCompleted, ExitCode: 0}, 0},
		{taskproto.CompletionRecord{State: taskproto.StateFailed, ExitCode: 3, RetryClass: taskproto.RetryAppError}, 3},
		{taskproto.CompletionRecord{State: taskproto.StateFailed, ExitCode: 7, RetryClass: taskproto.RetryOutputDeliveryError}, 7},
	} {
		if got := waitExitCode(&tc.rec); got != tc.want {
			t.Errorf("%+v: --wait exits %d, want %d", tc.rec, got, tc.want)
		}
	}
}
