package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// pagedDynamo serves DynamoDB Query and Scan in pages, the way DynamoDB does
// once a result passes 1 MB: each response but the last carries a
// LastEvaluatedKey, and the next request must send it back as
// ExclusiveStartKey. A handler that reads one page sees only pages[table][0].
type pagedDynamo struct {
	mu      sync.Mutex
	pages   map[string][][]string // table -> pages -> items (DynamoDB JSON)
	failAt  map[string]int        // table -> page index that fails (non-retryable)
	getItem map[string]string     // table -> the item GetItem returns
	deletes []string              // DeleteItem request bodies
	reads   map[string]int        // table -> Query/Scan pages served
}

func (d *pagedDynamo) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	op := req.Header.Get("X-Amz-Target")
	var in struct {
		TableName         string
		ExclusiveStartKey map[string]map[string]string
	}
	_ = json.Unmarshal(b, &in)

	d.mu.Lock()
	defer d.mu.Unlock()
	status, body := 200, "{}"
	switch {
	case strings.HasSuffix(op, ".Query"), strings.HasSuffix(op, ".Scan"):
		page := 0
		if k, ok := in.ExclusiveStartKey["page"]; ok {
			page, _ = strconv.Atoi(k["N"])
		}
		if d.reads == nil {
			d.reads = map[string]int{}
		}
		d.reads[in.TableName]++
		if f, ok := d.failAt[in.TableName]; ok && f == page {
			status = 400
			body = `{"__type":"com.amazonaws.dynamodb.v20120810#ValidationException","message":"page failed"}`
			break
		}
		pages := d.pages[in.TableName]
		var items []string
		if page < len(pages) {
			items = pages[page]
		}
		body = fmt.Sprintf(`{"Items":[%s],"Count":%d`, strings.Join(items, ","), len(items))
		if page+1 < len(pages) {
			body += fmt.Sprintf(`,"LastEvaluatedKey":{"page":{"N":"%d"}}`, page+1)
		}
		body += "}"
	case strings.HasSuffix(op, ".GetItem"):
		if it, ok := d.getItem[in.TableName]; ok {
			body = `{"Item":` + it + `}`
		}
	case strings.HasSuffix(op, ".DeleteItem"):
		d.deletes = append(d.deletes, string(b))
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    req,
	}, nil
}

func (d *pagedDynamo) cfg() aws.Config {
	return aws.Config{
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "not-a-secret", ""),
		HTTPClient:       &http.Client{Transport: d},
		RetryMaxAttempts: 1,
	}
}

const (
	pagingOwner = "arn:aws:iam::435415984226:user/alice"
	pagingOther = "arn:aws:iam::435415984226:user/mallory"
)

// threePages spreads n items per page over three pages, each built by mk(page,
// i). The middle page is empty, as a filtered Scan's often is: an empty page
// with a LastEvaluatedKey is not the end.
func threePages(n int, mk func(p, i int) string) [][]string {
	pages := make([][]string, 3)
	for _, p := range []int{0, 2} {
		for i := 0; i < n; i++ {
			pages[p] = append(pages[p], mk(p, i))
		}
	}
	pages[1] = append(pages[1], mk(1, 0))
	return pages
}

func decodeBody(t *testing.T, resp events.APIGatewayProxyResponse, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(resp.Body), v); err != nil {
		t.Fatalf("decode %d response %q: %v", resp.StatusCode, resp.Body, err)
	}
}

func watchItem(owner string) func(p, i int) string {
	return func(p, i int) string {
		return fmt.Sprintf(`{"watch_id":{"S":"w-%d-%d"},"user_id":{"S":"%s"},"status":{"S":"active"}}`, p, i, owner)
	}
}

// ── /api/watches ─────────────────────────────────────────────────────────────

func TestListWatchesReadsEveryPage(t *testing.T) {
	d := &pagedDynamo{pages: map[string][][]string{defaultWatchesTable: threePages(2, watchItem(pagingOwner))}}
	resp, _ := handleListWatches(context.Background(), d.cfg(), pagingOwner)
	var body WatchesResponse
	decodeBody(t, resp, &body)
	if resp.StatusCode != 200 || len(body.Watches) != 5 {
		t.Fatalf("got %d with %d watches, want 200 with all 5 across 3 pages (#1421)", resp.StatusCode, len(body.Watches))
	}
}

func TestListWatchesFailingPageIsAnError(t *testing.T) {
	d := &pagedDynamo{
		pages:  map[string][][]string{defaultWatchesTable: threePages(2, watchItem(pagingOwner))},
		failAt: map[string]int{defaultWatchesTable: 1},
	}
	resp, _ := handleListWatches(context.Background(), d.cfg(), pagingOwner)
	var body WatchesResponse
	decodeBody(t, resp, &body)
	if resp.StatusCode != 500 || body.Success || len(body.Watches) != 0 {
		t.Fatalf("a failed second page returned %d success=%v with %d watches; want a 500 and no list", resp.StatusCode, body.Success, len(body.Watches))
	}
}

func TestListWatchesOwnershipAppliesOnEveryPage(t *testing.T) {
	pages := threePages(2, watchItem(pagingOwner))
	pages[2] = append(pages[2],
		watchItem(pagingOther)(2, 9),
		`{"watch_id":{"S":"w-no-owner"},"status":{"S":"active"}}`)
	d := &pagedDynamo{pages: map[string][][]string{defaultWatchesTable: pages}}
	resp, _ := handleListWatches(context.Background(), d.cfg(), pagingOwner)
	var body WatchesResponse
	decodeBody(t, resp, &body)
	if len(body.Watches) != 5 {
		t.Errorf("got %d watches, want the owner's 5", len(body.Watches))
	}
	for _, w := range body.Watches {
		if w.WatchID == "w-2-9" || w.WatchID == "w-no-owner" {
			t.Errorf("returned %s, which is not the caller's (#821)", w.WatchID)
		}
	}
}

// ── /api/watches/history ─────────────────────────────────────────────────────

func matchItem(owner string) func(p, i int) string {
	return func(p, i int) string {
		return fmt.Sprintf(`{"watch_id":{"S":"m-%d-%d"},"user_id":{"S":"%s"},"region":{"S":"us-east-1"}}`, p, i, owner)
	}
}

func TestWatchHistoryReadsEveryPage(t *testing.T) {
	d := &pagedDynamo{pages: map[string][][]string{defaultHistoryTable: threePages(3, matchItem(pagingOwner))}}
	resp, _ := handleWatchHistory(context.Background(), d.cfg(), pagingOwner)
	var body WatchesResponse
	decodeBody(t, resp, &body)
	if resp.StatusCode != 200 || len(body.Matches) != 7 || body.Truncated {
		t.Fatalf("got %d with %d matches truncated=%v, want all 7 across 3 pages, not truncated", resp.StatusCode, len(body.Matches), body.Truncated)
	}
}

// The bound is the documented "newest 100", reached across pages, and a longer
// history says so instead of passing for complete.
func TestWatchHistoryBoundIsReportedAsTruncated(t *testing.T) {
	d := &pagedDynamo{pages: map[string][][]string{defaultHistoryTable: threePages(60, matchItem(pagingOwner))}}
	resp, _ := handleWatchHistory(context.Background(), d.cfg(), pagingOwner)
	var body WatchesResponse
	decodeBody(t, resp, &body)
	if len(body.Matches) != int(watchHistoryLimit) || !body.Truncated {
		t.Fatalf("121 matches over 3 pages gave %d truncated=%v; want the newest %d and truncated=true", len(body.Matches), body.Truncated, watchHistoryLimit)
	}

	exact := &pagedDynamo{pages: map[string][][]string{defaultHistoryTable: {nil, nil}}}
	for i := 0; i < int(watchHistoryLimit); i++ {
		exact.pages[defaultHistoryTable][i%2] = append(exact.pages[defaultHistoryTable][i%2], matchItem(pagingOwner)(i%2, i))
	}
	resp, _ = handleWatchHistory(context.Background(), exact.cfg(), pagingOwner)
	body = WatchesResponse{}
	decodeBody(t, resp, &body)
	if len(body.Matches) != int(watchHistoryLimit) || body.Truncated {
		t.Fatalf("exactly %d matches gave %d truncated=%v; want all of them, not truncated", watchHistoryLimit, len(body.Matches), body.Truncated)
	}
}

func TestWatchHistoryFailingPageIsAnError(t *testing.T) {
	d := &pagedDynamo{
		pages:  map[string][][]string{defaultHistoryTable: threePages(3, matchItem(pagingOwner))},
		failAt: map[string]int{defaultHistoryTable: 1},
	}
	resp, _ := handleWatchHistory(context.Background(), d.cfg(), pagingOwner)
	var body WatchesResponse
	decodeBody(t, resp, &body)
	if resp.StatusCode != 500 || len(body.Matches) != 0 {
		t.Fatalf("a failed second page returned %d with %d matches; want a 500 and no list", resp.StatusCode, len(body.Matches))
	}
}

func TestWatchHistoryOwnershipAppliesOnEveryPage(t *testing.T) {
	pages := threePages(3, matchItem(pagingOwner))
	pages[2] = append(pages[2], matchItem(pagingOther)(2, 9), `{"watch_id":{"S":"m-no-owner"}}`)
	d := &pagedDynamo{pages: map[string][][]string{defaultHistoryTable: pages}}
	resp, _ := handleWatchHistory(context.Background(), d.cfg(), pagingOwner)
	var body WatchesResponse
	decodeBody(t, resp, &body)
	if len(body.Matches) != 7 {
		t.Errorf("got %d matches, want the owner's 7", len(body.Matches))
	}
	if strings.Contains(resp.Body, "m-2-9") || strings.Contains(resp.Body, "m-no-owner") {
		t.Error("history returned a match that is not the caller's")
	}
	if strings.Contains(resp.Body, "user_id") {
		t.Error("history serialized user_id, an authorization input")
	}
}

// ── /api/cost-history ────────────────────────────────────────────────────────

func costItem(owner string) func(p, i int) string {
	return func(p, i int) string {
		return fmt.Sprintf(`{"user_id":{"S":"%s"},"timestamp":{"S":"2026-10-%02dT%02d:00:00Z"},"hourly_cost":{"N":"1"}}`, owner, 10+p, i)
	}
}

func TestCostHistoryReadsEveryPageAndFailsClosed(t *testing.T) {
	pages := threePages(4, costItem(pagingOwner))
	pages[2] = append(pages[2], costItem(pagingOther)(2, 23))
	d := &pagedDynamo{pages: map[string][][]string{dynamoCostHistoryTable: pages}}
	resp, _ := handleGetCostHistory(context.Background(), d.cfg(), 30, pagingOwner)
	var body CostHistoryAPIResponse
	decodeBody(t, resp, &body)
	if resp.StatusCode != 200 || len(body.History) != 9 {
		t.Fatalf("got %d with %d points, want the owner's 9 across 3 pages", resp.StatusCode, len(body.History))
	}

	d.failAt = map[string]int{dynamoCostHistoryTable: 2}
	resp, _ = handleGetCostHistory(context.Background(), d.cfg(), 30, pagingOwner)
	if resp.StatusCode != 500 {
		t.Fatalf("a failed third page returned %d; want a 500", resp.StatusCode)
	}
}

// ── /api/sweeps ──────────────────────────────────────────────────────────────

func sweepItem(owner, team string) func(p, i int) string {
	return func(p, i int) string {
		return fmt.Sprintf(`{"sweep_id":{"S":"sw-%s-%d-%d"},"user_id":{"S":"%s"},"team_id":{"S":"%s"},"status":{"S":"RUNNING"}}`,
			team, p, i, owner, team)
	}
}

func TestListSweepsReadsEveryPageAndFailsClosed(t *testing.T) {
	pages := threePages(2, sweepItem(pagingOwner, ""))
	pages[2] = append(pages[2], sweepItem(pagingOther, "x")(2, 9))
	d := &pagedDynamo{pages: map[string][][]string{dynamoSweepTable: pages}}
	resp, _ := handleListSweeps(context.Background(), d.cfg(), pagingOwner, "")
	var body SweepAPIResponse
	decodeBody(t, resp, &body)
	if resp.StatusCode != 200 || len(body.Sweeps) != 5 {
		t.Fatalf("got %d with %d sweeps, want the owner's 5 across 3 Scan pages", resp.StatusCode, len(body.Sweeps))
	}

	d.failAt = map[string]int{dynamoSweepTable: 1}
	resp, _ = handleListSweeps(context.Background(), d.cfg(), pagingOwner, "")
	if resp.StatusCode != 500 {
		t.Fatalf("a failed second Scan page returned %d; want a 500", resp.StatusCode)
	}
}

// The team Query shares the table with the personal Scan. The fake serves both
// the same three pages, all owned by another user, so the personal Scan
// contributes nothing (fail-closed ownership) and every listed sweep comes from
// the team Query. Reads 1-3 are the Scan, 4-6 the Query.
func TestListSweepsTeamQueryReadsEveryPageAndFailureIsAnError(t *testing.T) {
	const team = "team-1"
	membership := `{"team_id":{"S":"` + team + `"},"member_arn":{"S":"` + pagingOwner + `"},"role":{"S":"member"}}`
	d := &pagedDynamo{
		pages:   map[string][][]string{dynamoSweepTable: threePages(2, sweepItem(pagingOther, team))},
		getItem: map[string]string{dynamoMembershipsTable: membership},
	}
	// Personal sweeps: none of these are the caller's, so all that are listed
	// come from the team Query, which must read all three pages.
	resp, _ := handleListSweeps(context.Background(), d.cfg(), pagingOwner, team)
	var body SweepAPIResponse
	decodeBody(t, resp, &body)
	if resp.StatusCode != 200 || len(body.Sweeps) != 5 {
		t.Fatalf("got %d with %d sweeps, want the team's 5 across 3 Query pages", resp.StatusCode, len(body.Sweeps))
	}

	// Fail only the team Query's last page: the personal Scan has finished by
	// then, so a 200 here would be the personal list presented as complete.
	d2 := &failNthRead{pagedDynamo: pagedDynamo{
		pages:   d.pages,
		getItem: d.getItem,
	}, failRead: 6}
	resp, _ = handleListSweeps(context.Background(), d2.cfg(), pagingOwner, team)
	if resp.StatusCode != 500 {
		t.Fatalf("a failed team-sweeps page returned %d; want a 500, not the personal sweeps alone", resp.StatusCode)
	}
}

// failNthRead fails the nth Query/Scan request overall (1-based), whatever its
// table and page.
type failNthRead struct {
	pagedDynamo
	n        int
	failRead int
}

func (f *failNthRead) RoundTrip(req *http.Request) (*http.Response, error) {
	op := req.Header.Get("X-Amz-Target")
	if strings.HasSuffix(op, ".Query") || strings.HasSuffix(op, ".Scan") {
		f.n++
		if f.n == f.failRead {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"__type":"com.amazonaws.dynamodb.v20120810#ValidationException","message":"page failed"}`)),
				Request:    req,
			}, nil
		}
	}
	return f.pagedDynamo.RoundTrip(req)
}

func (f *failNthRead) cfg() aws.Config {
	c := f.pagedDynamo.cfg()
	c.HTTPClient = &http.Client{Transport: f}
	return c
}

// ── /teams ───────────────────────────────────────────────────────────────────

func membershipItem(member string) func(p, i int) string {
	return func(p, i int) string {
		return fmt.Sprintf(`{"team_id":{"S":"t-%d-%d"},"member_arn":{"S":"%s"},"role":{"S":"member"}}`, p, i, member)
	}
}

func TestListMyTeamsReadsEveryPageAndFailsClosed(t *testing.T) {
	pages := threePages(2, membershipItem(pagingOwner))
	pages[2] = append(pages[2], membershipItem(pagingOther)(2, 9))
	d := &pagedDynamo{
		pages:   map[string][][]string{dynamoMembershipsTable: pages},
		getItem: map[string]string{dynamoTeamsTable: `{"team_id":{"S":"t"},"team_name":{"S":"T"}}`},
	}
	resp, _ := handleListMyTeams(context.Background(), d.cfg(), pagingOwner)
	var body struct{ Teams []json.RawMessage }
	decodeBody(t, resp, &body)
	if resp.StatusCode != 200 || len(body.Teams) != 5 {
		t.Fatalf("got %d with %d teams, want the caller's 5 across 3 pages", resp.StatusCode, len(body.Teams))
	}

	d.failAt = map[string]int{dynamoMembershipsTable: 1}
	resp, _ = handleListMyTeams(context.Background(), d.cfg(), pagingOwner)
	if resp.StatusCode != 500 {
		t.Fatalf("a failed second page returned %d; want a 500", resp.StatusCode)
	}
}

func teamMemberItem(team string) func(p, i int) string {
	return func(p, i int) string {
		return fmt.Sprintf(`{"team_id":{"S":"%s"},"member_arn":{"S":"arn:aws:iam::435415984226:user/m-%d-%d"},"role":{"S":"member"}}`, team, p, i)
	}
}

func teamFixture(team string) *pagedDynamo {
	return &pagedDynamo{
		pages: map[string][][]string{dynamoMembershipsTable: threePages(2, teamMemberItem(team))},
		getItem: map[string]string{
			dynamoMembershipsTable: `{"team_id":{"S":"` + team + `"},"member_arn":{"S":"` + pagingOwner + `"},"role":{"S":"owner"}}`,
			dynamoTeamsTable:       `{"team_id":{"S":"` + team + `"},"team_name":{"S":"T"}}`,
		},
	}
}

func TestGetTeamReadsEveryMemberPageAndFailsClosed(t *testing.T) {
	const team = "team-1"
	d := teamFixture(team)
	d.pages[dynamoMembershipsTable][2] = append(d.pages[dynamoMembershipsTable][2], teamMemberItem("team-other")(2, 9))
	resp, _ := handleGetTeam(context.Background(), d.cfg(), team, pagingOwner)
	var body struct{ Members []TeamMemberRecord }
	decodeBody(t, resp, &body)
	if resp.StatusCode != 200 || len(body.Members) != 5 {
		t.Fatalf("got %d with %d members, want the team's 5 across 3 pages", resp.StatusCode, len(body.Members))
	}

	d.failAt = map[string]int{dynamoMembershipsTable: 2}
	resp, _ = handleGetTeam(context.Background(), d.cfg(), team, pagingOwner)
	if resp.StatusCode != 500 {
		t.Fatalf("a failed third page returned %d; want a 500", resp.StatusCode)
	}
}

// Deleting a team removes every membership, not the first page's, and a failed
// page deletes nothing: the team record must not go while memberships remain.
func TestDeleteTeamRemovesEveryMembershipPage(t *testing.T) {
	const team = "team-1"
	d := teamFixture(team)
	resp, _ := handleDeleteTeam(context.Background(), d.cfg(), team, pagingOwner)
	if resp.StatusCode != 200 {
		t.Fatalf("delete returned %d %s", resp.StatusCode, resp.Body)
	}
	var memberDeletes int
	for _, b := range d.deletes {
		if strings.Contains(b, dynamoMembershipsTable) {
			memberDeletes++
		}
	}
	if memberDeletes != 5 {
		t.Errorf("deleted %d memberships, want all 5 across 3 pages (the rest would be orphaned)", memberDeletes)
	}

	f := teamFixture(team)
	f.failAt = map[string]int{dynamoMembershipsTable: 1}
	resp, _ = handleDeleteTeam(context.Background(), f.cfg(), team, pagingOwner)
	if resp.StatusCode != 500 || len(f.deletes) != 0 {
		t.Fatalf("a failed second page returned %d after %d deletes; want a 500 and nothing deleted", resp.StatusCode, len(f.deletes))
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// scanFirst backs the Slack token rotation's workspace lookup, a filtered
// Scan: the match can be on a page after empty ones.
func TestScanFirstFollowsEmptyPages(t *testing.T) {
	const table = "spore-slack-workspaces"
	d := &pagedDynamo{pages: map[string][][]string{table: {nil, nil, {`{"workspace_key":{"S":"T1"}}`}}}}
	client := dynamodb.NewFromConfig(d.cfg())
	item, err := scanFirst(context.Background(), client, &dynamodb.ScanInput{TableName: aws.String(table)})
	if err != nil || item == nil {
		t.Fatalf("scanFirst = %v, %v; want the match on the third page", item, err)
	}

	d.failAt = map[string]int{table: 1}
	item, err = scanFirst(context.Background(), client, &dynamodb.ScanInput{TableName: aws.String(table)})
	if err == nil || item != nil {
		t.Fatalf("a failed second page gave %v, %v; want an error, not \"not found\"", item, err)
	}
}

// TestDashboardDynamoCallsArePaginated is the gate for every call site: a
// direct .Query( or .Scan( call on a DynamoDB client reads ONE page, so each
// must go through the paging helpers (paging.go) or carry a `single-page:`
// comment, on the line above, saying why one page is a complete answer.
// Discovered from the package's sources, not listed.
func TestDashboardDynamoCallsArePaginated(t *testing.T) {
	dir := dashboardDir(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var direct, paged int
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) // #nosec G304 -- this package's own sources
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		file, err := parser.ParseFile(fset, f, src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "NewQueryPaginator", "NewScanPaginator":
				paged++
			case "Query", "Scan":
				if len(call.Args) < 2 {
					return true
				}
				direct++
				line := fset.Position(call.Pos()).Line
				if !commentAbove(lines, line, "single-page:") {
					bad = append(bad, fmt.Sprintf("%s:%d", filepath.Base(f), line))
				}
			}
			return true
		})
	}
	if direct+paged == 0 {
		t.Fatal("discovered no DynamoDB Query/Scan calls and no paginators; the discovery is broken")
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s: a direct Query/Scan reads one page; use queryAll/queryUpTo/scanAll/scanFirst, or justify with a `// single-page:` comment above it (#1421)", b)
	}
}

// commentAbove reports whether the comment block ending on the line above
// line (1-based) contains marker.
func commentAbove(lines []string, line int, marker string) bool {
	for i := line - 2; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "//") {
			return false
		}
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}
