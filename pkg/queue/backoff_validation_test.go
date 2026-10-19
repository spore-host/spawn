package queue

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// implementedBackoffs discovers the strategies CalculateBackoff implements by
// reading the case labels of its `switch cfg.Backoff` from retry.go, so this
// gate covers a strategy added to the switch without anyone editing a list
// here (#1119).
func implementedBackoffs(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "retry.go", nil, 0)
	if err != nil {
		t.Fatalf("parse retry.go: %v", err)
	}
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "CalculateBackoff" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			sel, ok := sw.Tag.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Backoff" {
				return true
			}
			for _, stmt := range sw.Body.List {
				for _, e := range stmt.(*ast.CaseClause).List {
					lit, ok := e.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("CalculateBackoff case label %T is not a string literal; teach this gate to read it", e)
					}
					s, _ := strconv.Unquote(lit.Value)
					if s != "" { // "" is the nil-config default, not a nameable strategy
						out = append(out, s)
					}
				}
			}
			return false
		})
	}
	if len(out) == 0 {
		t.Fatal("found no backoff strategies in CalculateBackoff's switch; discovery is broken, not the repo clean")
	}
	return out
}

func retryQueue(r *RetryConfig) *QueueConfig {
	return &QueueConfig{
		QueueID:   "q",
		QueueName: "q",
		Jobs:      []JobConfig{{JobID: "j", Command: "true", Timeout: "5m", Retry: r}},
	}
}

// TestValidateQueue_AcceptsEveryImplementedBackoff: a strategy the retry loop
// can run must not be refused at load time (#1119).
func TestValidateQueue_AcceptsEveryImplementedBackoff(t *testing.T) {
	for _, b := range implementedBackoffs(t) {
		if err := ValidateQueue(retryQueue(&RetryConfig{MaxAttempts: 3, Backoff: b, Jitter: 0.5})); err != nil {
			t.Errorf("backoff %q is implemented by CalculateBackoff but ValidateQueue rejects it: %v", b, err)
		}
	}
}

// The documented field comment names the strategies too; keep it in step with
// the implementation so a reader of RetryConfig is not misled.
func TestRetryConfigBackoffCommentNamesEveryStrategy(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "queue.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var comment string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "RetryConfig" {
			return true
		}
		for _, fld := range ts.Type.(*ast.StructType).Fields.List {
			if len(fld.Names) == 1 && fld.Names[0].Name == "Backoff" && fld.Comment != nil {
				comment = fld.Comment.Text()
			}
		}
		return false
	})
	if comment == "" {
		t.Fatal("RetryConfig.Backoff has no trailing comment to check")
	}
	for _, b := range implementedBackoffs(t) {
		if !strings.Contains(comment, strconv.Quote(b)) {
			t.Errorf("RetryConfig.Backoff comment %q does not name implemented strategy %q", strings.TrimSpace(comment), b)
		}
	}
}

func TestValidateQueue_RejectsUnknownOrMissingBackoff(t *testing.T) {
	implemented := implementedBackoffs(t)
	for _, b := range []string{"", "linear", "Exponential", "exponential_jitter"} {
		err := ValidateQueue(retryQueue(&RetryConfig{MaxAttempts: 3, Backoff: b}))
		if err == nil {
			t.Errorf("backoff %q accepted, want rejection", b)
			continue
		}
		// The rejection tells the user what they may write instead.
		for _, want := range implemented {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("backoff %q: error %q does not name supported strategy %q", b, err, want)
			}
		}
	}
}

func TestValidateQueue_JitterRange(t *testing.T) {
	for _, tc := range []struct {
		jitter float64
		ok     bool
	}{{0, true}, {0.25, true}, {1, true}, {-0.1, false}, {1.5, false}} {
		err := ValidateQueue(retryQueue(&RetryConfig{MaxAttempts: 3, Backoff: "exponential-jitter", Jitter: tc.jitter}))
		if (err == nil) != tc.ok {
			t.Errorf("jitter %v: err = %v, want ok=%v", tc.jitter, err, tc.ok)
		}
	}
}

// TestLoadConfig_ExponentialJitter is the documented example from
// docs/guides/batch-queue.md: a queue file naming exponential-jitter loads.
func TestLoadConfig_ExponentialJitter(t *testing.T) {
	path := t.TempDir() + "/q.json"
	body := `{"queue_id":"q","queue_name":"q","jobs":[{"job_id":"j","command":"true","timeout":"5m",
	  "retry":{"max_attempts":3,"backoff":"exponential-jitter","base_delay":"2s","max_delay":"5m","jitter":0.2,
	  "retry_on_codes":[1,137],"dont_retry_on_codes":[2]}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Jobs[0].Retry.Backoff; got != "exponential-jitter" {
		t.Fatalf("backoff = %q", got)
	}
}
