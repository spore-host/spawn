package lastmilespawn

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"

	"github.com/spore-host/spawn/pkg/lifecyclemark"
)

const testAccount = "123456789012"

// stsHTTP answers every request with a GetCallerIdentity response naming
// account, and counts the requests. No network is involved.
type stsHTTP struct {
	mu      sync.Mutex
	account string
	n       int
}

func (s *stsHTTP) Do(*http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.n++
	acct := s.account
	s.mu.Unlock()
	body := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
		`<Arn>arn:aws:iam::` + acct + `:user/u</Arn><UserId>AIDAEXAMPLE</UserId><Account>` + acct + `</Account>` +
		`</GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/xml"}},
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (s *stsHTTP) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func stsConfig(h aws.HTTPClient, opts ...func(*middleware.Stack) error) aws.Config {
	return aws.Config{Region: "us-east-1", HTTPClient: h, Credentials: aws.AnonymousCredentials{}, RetryMaxAttempts: 1, APIOptions: opts}
}

func TestAccountObserverRecordsTheCallersOwnCall(t *testing.T) {
	o := &AccountObserver{}
	h := &stsHTTP{account: testAccount}
	// Twice on the stack: it adds itself once.
	cfg := stsConfig(h, o.Middleware, o.Middleware)
	if o.Account() != "" {
		t.Fatal("an account before any call")
	}
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err != nil {
		t.Fatal(err)
	}
	if got := o.Account(); got != testAccount {
		t.Errorf("Account = %q, want %s", got, testAccount)
	}
	if n := h.requests(); n != 1 {
		t.Errorf("%d request(s); the observer must add none", n)
	}

	// The same account again changes nothing; another one is a conflict.
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err != nil {
		t.Fatal(err)
	}
	if got := o.Account(); got != testAccount {
		t.Errorf("after the same account again: %q", got)
	}
	h.account = "210987654321"
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err != nil {
		t.Fatal(err)
	}
	if got := o.Account(); got != "" {
		t.Errorf("two accounts gave %q, want \"\"", got)
	}
	o.Reset()
	if got := o.Account(); got != "" {
		t.Errorf("after Reset: %q", got)
	}
	o.record("")
	if got := o.Account(); got != "" {
		t.Errorf("an empty account was recorded: %q", got)
	}
}

// A failed call records nothing.
func TestAccountObserverIgnoresFailures(t *testing.T) {
	o := &AccountObserver{}
	cfg := stsConfig(refusingHTTP{}, o.Middleware)
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err == nil {
		t.Fatal("a refused call succeeded")
	}
	if got := o.Account(); got != "" {
		t.Errorf("a failed call recorded %q", got)
	}
}

type refusingHTTP struct{}

func (refusingHTTP) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("refused: no network in tests")
}

func TestLatchAndNotPermitted(t *testing.T) {
	for msg, want := range map[string]bool{
		"api error AccessDeniedException: no":              true,
		"UnauthorizedOperation: you are not":               true,
		"User x is not authorized to perform: iam:GetRole": true,
		"ThrottlingException: slow down":                   false,
	} {
		if NotPermitted(msg) != want {
			t.Errorf("NotPermitted(%q) = %v", msg, !want)
		}
	}
	var l Latch
	l.Note(nil)
	l.Note(errors.New("throttled"))
	if l.Err() != nil {
		t.Fatalf("latched on a non-refusal: %v", l.Err())
	}
	denied := errors.New("AccessDenied: no")
	l.Note(denied)
	l.Note(errors.New("AccessDenied: second"))
	err := l.Err()
	if err == nil || !errors.Is(err, denied) || !strings.HasPrefix(err.Error(), NotAsked+" request was refused") {
		t.Errorf("Err = %v, want the not-asked answer wrapping the first refusal", err)
	}
	named := Latch{What: "IAM read"}
	named.Note(denied)
	if !strings.Contains(named.Err().Error(), "IAM read was refused") {
		t.Errorf("Err = %v", named.Err())
	}
}

// ---- the lifecycle client --------------------------------------------------

type failingTable struct {
	lifecycle.API
	err error
	mu  sync.Mutex
	n   int
}

func (f *failingTable) fail() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.err
}

func (f *failingTable) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *failingTable) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return nil, f.fail()
}

func (f *failingTable) Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return nil, f.fail()
}

func (f *failingTable) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return nil, f.fail()
}

var accessDenied = &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: dynamodb:GetItem"}

func TestReadOnlyLifecycleRefusesEveryWrite(t *testing.T) {
	ctx := context.Background()
	inner := lifecycletest.New()
	r := ReadOnlyLifecycle{API: inner, Latch: &Latch{}}
	writes := map[string]func() error{
		"PutItem":            func() error { _, err := r.PutItem(ctx, &dynamodb.PutItemInput{}); return err },
		"UpdateItem":         func() error { _, err := r.UpdateItem(ctx, &dynamodb.UpdateItemInput{}); return err },
		"DeleteItem":         func() error { _, err := r.DeleteItem(ctx, &dynamodb.DeleteItemInput{}); return err },
		"TransactWriteItems": func() error { _, err := r.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{}); return err },
		"CreateTable":        func() error { _, err := r.CreateTable(ctx, &dynamodb.CreateTableInput{}); return err },
		"DeleteTable":        func() error { _, err := r.DeleteTable(ctx, &dynamodb.DeleteTableInput{}); return err },
	}
	for op, call := range writes {
		if err := call(); !errors.Is(err, ErrReportOnly) {
			t.Errorf("%s = %v, want ErrReportOnly", op, err)
		}
		if n := inner.Calls(op); n != 0 {
			t.Errorf("%s reached the table %d time(s)", op, n)
		}
	}
	// Reads pass through (no table: not found).
	if _, err := r.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(lifecycle.TableName)}); err == nil {
		t.Error("DescribeTable of a missing table succeeded")
	}
	_, _ = r.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(lifecycle.TableName)})
	_, _ = r.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(lifecycle.TableName)})
	for _, op := range []string{"DescribeTable", "GetItem", "Scan"} {
		if inner.Calls(op) != 1 {
			t.Errorf("%s reached the table %d time(s), want 1", op, inner.Calls(op))
		}
	}
	// Without a latch it still reads.
	if _, err := (ReadOnlyLifecycle{API: inner}).DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(lifecycle.TableName)}); err == nil {
		t.Error("DescribeTable without a latch succeeded on a missing table")
	}
}

func TestReadOnlyLifecycleStopsAskingAfterARefusal(t *testing.T) {
	ctx := context.Background()
	denied := &failingTable{err: accessDenied}
	latch := &Latch{What: "read of the table"}
	a, b := ReadOnlyLifecycle{API: denied, Latch: latch}, ReadOnlyLifecycle{API: denied, Latch: latch}
	if _, err := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{}); err == nil {
		t.Fatal("a denied read succeeded")
	}
	_, gerr := b.GetItem(ctx, &dynamodb.GetItemInput{})
	_, serr := b.Scan(ctx, &dynamodb.ScanInput{})
	_, derr := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{})
	for _, err := range []error{gerr, serr, derr} {
		if err == nil || !strings.Contains(err.Error(), NotAsked) || !NotPermitted(err.Error()) {
			t.Errorf("after a refusal: %v, want the not-asked answer naming it", err)
		}
	}
	if n := denied.requests(); n != 1 {
		t.Errorf("%d request(s) after one refusal, want 1", n)
	}
	// A throttle does not stop the asking.
	thr := &failingTable{err: errors.New("ThrottlingException")}
	c := ReadOnlyLifecycle{API: thr, Latch: &Latch{}}
	_, _ = c.Scan(ctx, &dynamodb.ScanInput{})
	_, _ = c.GetItem(ctx, &dynamodb.GetItemInput{})
	if n := thr.requests(); n != 2 {
		t.Errorf("a throttle stopped the asking: %d request(s), want 2", n)
	}
}

func TestNewLifecycleReader(t *testing.T) {
	fakes := map[string]*lifecycletest.Fake{}
	tables := func(region string) (lifecycle.API, error) {
		if region == "eu-north-1" {
			return nil, errors.New("no endpoint")
		}
		if fakes[region] == nil {
			fakes[region] = lifecycletest.New()
		}
		return fakes[region], nil
	}
	c, latch, err := NewLifecycleReader(tables, func() time.Time { return time.Unix(0, 0) })
	if err != nil || c == nil || latch == nil {
		t.Fatalf("NewLifecycleReader = %v, %v, %v", c, latch, err)
	}
	if r, err := c.RegionFor(lifecycle.ScopeGlobal); err != nil || r != lifecyclemark.ControlRegion {
		t.Errorf("control region %q, %v; want %s, where spawn's creators write the global scope", r, err, lifecyclemark.ControlRegion)
	}
	// Writes through the client are refused before any table sees them.
	if err := c.MarkActive(context.Background(), tags.ToolSpawn, "us-west-2"); !errors.Is(err, ErrReportOnly) {
		t.Errorf("MarkActive = %v, want ErrReportOnly", err)
	}
	for _, op := range []string{"PutItem", "UpdateItem", "TransactWriteItems", "CreateTable"} {
		if n := fakes["us-west-2"].Calls(op); n != 0 {
			t.Errorf("%s reached the table %d time(s)", op, n)
		}
	}
	if _, _, err := c.Get(context.Background(), tags.ToolSpawn, "eu-north-1"); err == nil || !strings.Contains(err.Error(), "no endpoint") {
		t.Errorf("Get through a table that cannot be built = %v", err)
	}
}

func TestRegionalTables(t *testing.T) {
	api, err := RegionalTables(aws.Config{Region: "us-east-1"})("ap-south-1")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := api.(*dynamodb.Client)
	if !ok {
		t.Fatalf("%T, want a DynamoDB client", api)
	}
	if r := c.Options().Region; r != "ap-south-1" {
		t.Errorf("region %q, want ap-south-1", r)
	}
}

// ---- the infra probe -------------------------------------------------------

type scanFake struct {
	errs  map[string]error
	asked []string
}

func (s *scanFake) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	s.asked = append(s.asked, aws.ToString(in.TableName))
	if aws.ToInt32(in.Limit) != 1 {
		return nil, errors.New("not a one-item scan")
	}
	return &dynamodb.ScanOutput{}, s.errs[aws.ToString(in.TableName)]
}

func TestInfraSignalsReadable(t *testing.T) {
	ctx := context.Background()
	want := []string{"spawn-sweep-orchestration", "spawn-pipeline-orchestration", "spawn-schedules"}
	ok := &scanFake{}
	if err := InfraSignalsReadable(ctx, ok); err != nil {
		t.Errorf("all readable: %v", err)
	}
	if strings.Join(ok.asked, ",") != strings.Join(want, ",") || strings.Join(InfraTables(), ",") != strings.Join(want, ",") {
		t.Errorf("read %v (InfraTables %v), want the reaper's three tables %v", ok.asked, InfraTables(), want)
	}
	missing := &scanFake{errs: map[string]error{"spawn-schedules": &ddbtypes.ResourceNotFoundException{Message: aws.String("no table")}}}
	if err := InfraSignalsReadable(ctx, missing); err != nil {
		t.Errorf("a missing table is no orchestrator, not unreadable: %v", err)
	}
	denied := &scanFake{errs: map[string]error{"spawn-pipeline-orchestration": accessDenied}}
	if err := InfraSignalsReadable(ctx, denied); err == nil || !strings.Contains(err.Error(), "spawn-pipeline-orchestration") {
		t.Errorf("a refused table = %v, want an error naming it", err)
	}
	if err := InfraSignalsReadable(ctx, nil); err == nil {
		t.Error("no client is readable")
	}
	if err := InfraSignalsReadableIn(ctx, stsConfig(refusingHTTP{})); err == nil {
		t.Error("tables read through a refusing client")
	}
}
