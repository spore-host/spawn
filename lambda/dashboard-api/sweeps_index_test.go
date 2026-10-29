package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// #1426: the personal sweeps list is a paginated Query on
// user_id-created_at-index, not a Scan of spawn-sweep-orchestration, and the
// dashboard role grants Query on exactly the indexes this package queries.

// dynamoCall is one DynamoDB request as the handler sent it.
type dynamoCall struct {
	Op                        string
	TableName                 string
	IndexName                 string
	KeyConditionExpression    string
	FilterExpression          string
	ExpressionAttributeValues map[string]map[string]string
}

// recordingDynamo records every request, then lets pagedDynamo answer it.
type recordingDynamo struct {
	*pagedDynamo
	mu    sync.Mutex
	calls []dynamoCall
}

func (r *recordingDynamo) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	var c dynamoCall
	_ = json.Unmarshal(b, &c)
	op := req.Header.Get("X-Amz-Target")
	c.Op = op[strings.LastIndex(op, ".")+1:]
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	req.Body = io.NopCloser(bytes.NewReader(b))
	return r.pagedDynamo.RoundTrip(req)
}

func newRecordingDynamo(pages [][]string) *recordingDynamo {
	return &recordingDynamo{pagedDynamo: &pagedDynamo{pages: map[string][][]string{dynamoSweepTable: pages}}}
}

func listPersonalSweeps(t *testing.T, r *recordingDynamo) (int, []SweepInfo) {
	t.Helper()
	cfg := r.cfg()
	cfg.HTTPClient = &http.Client{Transport: r}
	resp, _ := handleListSweeps(context.Background(), cfg, pagingOwner, "")
	var body SweepAPIResponse
	decodeBody(t, resp, &body)
	return resp.StatusCode, body.Sweeps
}

// The mechanism: every read of the sweep table is a Query on the user index,
// keyed on the caller, and none is a Scan.
func TestListPersonalSweepsQueriesTheUserIndex(t *testing.T) {
	r := newRecordingDynamo(threePages(2, sweepItem(pagingOwner, "")))
	if code, _ := listPersonalSweeps(t, r); code != 200 {
		t.Fatalf("status %d, want 200", code)
	}
	var queries int
	for _, c := range r.calls {
		if c.TableName != dynamoSweepTable {
			continue
		}
		switch c.Op {
		case "Scan":
			t.Errorf("listing personal sweeps Scanned %s (filter %q): that reads every user's sweeps; Query %s (#1426)", c.TableName, c.FilterExpression, sweepUserIndex)
		case "Query":
			queries++
			if c.IndexName != "user_id-created_at-index" {
				t.Errorf("Query on index %q, want user_id-created_at-index", c.IndexName)
			}
			m := regexp.MustCompile(`^user_id\s*=\s*(:\w+)$`).FindStringSubmatch(strings.TrimSpace(c.KeyConditionExpression))
			if m == nil {
				t.Errorf("KeyConditionExpression %q, want user_id = :v", c.KeyConditionExpression)
			} else if got := c.ExpressionAttributeValues[m[1]]["S"]; got != pagingOwner {
				t.Errorf("Query keyed on user_id %q, want the caller %q", got, pagingOwner)
			}
		default:
			t.Errorf("unexpected %s on %s", c.Op, c.TableName)
		}
	}
	if queries != 3 {
		t.Errorf("%d Query requests on %s, want one per page (3)", queries, dynamoSweepTable)
	}
}

// Every page is read, and a failing page is an error, not a short list.
func TestListPersonalSweepsReadsEveryIndexPage(t *testing.T) {
	r := newRecordingDynamo(threePages(2, sweepItem(pagingOwner, "")))
	code, sweeps := listPersonalSweeps(t, r)
	if code != 200 || len(sweeps) != 5 {
		t.Fatalf("got %d with %d sweeps, want 200 with all 5 across 3 index pages", code, len(sweeps))
	}
	if n := r.reads[dynamoSweepTable]; n != 3 {
		t.Errorf("served %d pages, want 3", n)
	}

	r = newRecordingDynamo(threePages(2, sweepItem(pagingOwner, "")))
	r.failAt = map[string]int{dynamoSweepTable: 2}
	if code, sweeps := listPersonalSweeps(t, r); code != 500 || len(sweeps) != 0 {
		t.Fatalf("a failed last page returned %d with %d sweeps; want a 500 and no list", code, len(sweeps))
	}
}

// Ownership is still checked per item: whatever the index returns, a sweep
// whose user_id is not the caller's (or is missing) is not listed.
func TestListPersonalSweepsOwnershipOnEveryIndexPage(t *testing.T) {
	pages := threePages(2, sweepItem(pagingOwner, ""))
	pages[2] = append(pages[2], sweepItem(pagingOther, "x")(2, 9), `{"sweep_id":{"S":"sw-no-owner"},"status":{"S":"RUNNING"}}`)
	r := newRecordingDynamo(pages)
	code, sweeps := listPersonalSweeps(t, r)
	if code != 200 || len(sweeps) != 5 {
		t.Errorf("got %d with %d sweeps, want the owner's 5", code, len(sweeps))
	}
	for _, s := range sweeps {
		if s.SweepID == "sw-x-2-9" || s.SweepID == "sw-no-owner" {
			t.Errorf("listed %s, which is not the caller's", s.SweepID)
		}
	}
}

// queriedIndexes discovers, from this package's sources, every
// dynamodb.QueryInput that names both a TableName and an IndexName, resolving
// package string constants. It returns table -> set of index names.
func queriedIndexes(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dashboardDir(t), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, file)
		ast.Inspect(file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						consts[name.Name], _ = strconv.Unquote(lit.Value)
					}
				}
			}
			return true
		})
	}
	// value resolves aws.String(x) where x is a literal or a package constant.
	value := func(e ast.Expr) string {
		call, ok := e.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return ""
		}
		switch a := call.Args[0].(type) {
		case *ast.BasicLit:
			s, _ := strconv.Unquote(a.Value)
			return s
		case *ast.Ident:
			return consts[a.Name]
		}
		return ""
	}
	out := map[string]map[string]bool{}
	for _, file := range parsed {
		ast.Inspect(file, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "QueryInput" {
				return true
			}
			var table, index string
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				switch k, _ := kv.Key.(*ast.Ident); {
				case k == nil:
				case k.Name == "TableName":
					table = value(kv.Value)
				case k.Name == "IndexName":
					index = value(kv.Value)
				}
			}
			if table != "" && index != "" {
				if out[table] == nil {
					out[table] = map[string]bool{}
				}
				out[table][index] = true
			}
			return true
		})
	}
	return out
}

// rolePolicyStatements returns the statements of the role policy the setup
// script writes, with every ${VAR} left as written.
func rolePolicyStatements(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dashboardDir(t), roleScript)) // #nosec G304 -- this package's own script
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start := strings.Index(src, "POLICY_DOCUMENT=$(cat <<EOF\n")
	if start < 0 {
		t.Fatal("no POLICY_DOCUMENT heredoc in the role script")
	}
	body := src[start+len("POLICY_DOCUMENT=$(cat <<EOF\n"):]
	end := strings.Index(body, "\nEOF\n")
	if end < 0 {
		t.Fatal("unterminated POLICY_DOCUMENT heredoc")
	}
	var doc struct {
		Statement []map[string]any
	}
	if err := json.Unmarshal([]byte(body[:end]), &doc); err != nil {
		t.Fatalf("role policy is not JSON: %v", err)
	}
	return doc.Statement
}

func strs(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// The role grants dynamodb:Query on every index this package queries on the
// sweep table, and on no other index of it (no wildcard, no extras).
func TestDashboardRoleGrantsExactlyTheQueriedSweepIndexes(t *testing.T) {
	used := queriedIndexes(t)[dynamoSweepTable]
	if !used[sweepUserIndex] || !used["team_id-index"] {
		t.Fatalf("discovered sweep-table indexes %v; want %s (the personal listing, #1426) and team_id-index (team sweeps); either the listing stopped querying them or the discovery is broken", used, sweepUserIndex)
	}
	prefix := "${T}/" + dynamoSweepTable + "/index/"
	granted := map[string]bool{}
	for _, st := range rolePolicyStatements(t) {
		if st["Effect"] != "Allow" {
			continue
		}
		query := false
		for _, a := range strs(st["Action"]) {
			if a == "dynamodb:Query" || a == "dynamodb:*" {
				query = true
			}
		}
		for _, r := range strs(st["Resource"]) {
			if !strings.HasPrefix(r, prefix) {
				continue
			}
			idx := strings.TrimPrefix(r, prefix)
			if strings.ContainsAny(idx, "*?") {
				t.Errorf("statement %v grants %s: scope it to the indexes queried", st["Sid"], r)
				continue
			}
			if query {
				granted[idx] = true
			}
		}
	}
	diff := func(a, b map[string]bool) []string {
		var out []string
		for k := range a {
			if !b[k] {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	for _, idx := range diff(used, granted) {
		t.Errorf("the dashboard queries %s/index/%s but the role grants no dynamodb:Query on it: AccessDenied in production (#1426)", dynamoSweepTable, idx)
	}
	for _, idx := range diff(granted, used) {
		t.Errorf("the role grants dynamodb:Query on %s/index/%s, which the dashboard never queries", dynamoSweepTable, idx)
	}
}
