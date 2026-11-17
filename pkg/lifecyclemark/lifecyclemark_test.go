package lifecyclemark

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"

	"github.com/spore-host/spawn/pkg/footprintspawn"
)

var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// fakes is one account: a lifecycletest.Fake per region.
type fakes map[string]*lifecycletest.Fake

func (fs fakes) factory(_ aws.Config, region string) (lifecycle.API, error) {
	f, ok := fs[region]
	if !ok {
		f = lifecycletest.New()
		fs[region] = f
	}
	return f, nil
}

// install points the process's marker at fs, its user-visible output at buf,
// and its diagnostic log at the buffer it returns, with a fixed clock and no
// real sleeps.
func install(t *testing.T, f Factory, buf *bytes.Buffer) *bytes.Buffer {
	t.Helper()
	debug := &bytes.Buffer{}
	restore := Override(f, buf, func(format string, args ...any) { fmt.Fprintf(debug, format+"\n", args...) })
	std.mu.Lock()
	std.now = func() time.Time { return testNow }
	std.sleep = func(context.Context, time.Duration) error { return nil }
	std.mu.Unlock()
	t.Cleanup(restore)
	return debug
}

func cfgFor(key string) aws.Config {
	return aws.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider(key, "secret", "")}
}

func item(t *testing.T, f *lifecycletest.Fake, scope string) map[string]types.AttributeValue {
	t.Helper()
	if f == nil {
		t.Fatalf("no table was touched for %s", scope)
	}
	it := f.Item(lifecycle.TableName, lifecycle.ItemKey(Tool, scope))
	if it == nil {
		t.Fatalf("no spawn item for %s", scope)
	}
	return it
}

func attr(it map[string]types.AttributeValue, k string) string {
	switch v := it[k].(type) {
	case *types.AttributeValueMemberS:
		return v.Value
	case *types.AttributeValueMemberN:
		return v.Value
	}
	return ""
}

// A mark is MarkActive: spawn ACTIVE at epoch 1, the region's item in that
// region's table and the global item in ControlRegion's, nowhere else.
func TestMarkMarksActive(t *testing.T) {
	fs := fakes{}
	var buf bytes.Buffer
	install(t, fs.factory, &buf)

	Mark(context.Background(), cfgFor("AKIA1"), "eu-west-1", ScopeGlobal)

	for scope, region := range map[string]string{"eu-west-1": "eu-west-1", ScopeGlobal: ControlRegion} {
		it := item(t, fs[region], scope)
		if attr(it, "state") != string(lifecycle.StateActive) || attr(it, "epoch") != "1" {
			t.Errorf("%s in %s: state %q epoch %q, want ACTIVE at 1", scope, region, attr(it, "state"), attr(it, "epoch"))
		}
	}
	if len(fs) != 2 {
		t.Errorf("tables touched in %d regions, want 2 (eu-west-1 and %s)", len(fs), ControlRegion)
	}
	if strings.Contains(buf.String(), "warning") {
		t.Errorf("a successful mark warned: %s", buf.String())
	}
}

// The same identity and scope within the hour costs no second write; another
// identity, or another scope, does.
func TestMarkCaches(t *testing.T) {
	fs := fakes{}
	install(t, fs.factory, &bytes.Buffer{})
	ctx := context.Background()

	Mark(ctx, cfgFor("AKIA1"), "us-east-2")
	// The first mark's write is tried twice: before and after creating the table.
	base := fs["us-east-2"].Calls("TransactWriteItems")
	Mark(ctx, cfgFor("AKIA1"), "us-east-2")
	if n := fs["us-east-2"].Calls("TransactWriteItems"); n != base {
		t.Errorf("a second mark of one scope by one identity made %d more writes, want 0", n-base)
	}
	Mark(ctx, cfgFor("AKIA2"), "us-east-2")
	if n := fs["us-east-2"].Calls("TransactWriteItems"); n != base+1 {
		t.Errorf("another identity's mark made %d more writes, want 1", n-base)
	}
	std.mu.Lock()
	std.now = func() time.Time { return testNow.Add(cacheFor + time.Minute) }
	std.mu.Unlock()
	Mark(ctx, cfgFor("AKIA1"), "us-east-2")
	if n := fs["us-east-2"].Calls("TransactWriteItems"); n != base+2 {
		t.Errorf("a mark after the cache expired made %d more writes, want 2 in all", n-base)
	}
}

// A refused identity prints nothing the user sees: a user without the lease
// actions is the normal case, and each CLI invocation is a new process, so
// even a once-per-process warning would be one on every launch. It logs one
// diagnostic line naming what is missing, is not asked again, and the call
// returns normally every time, so the launch goes on.
func TestMarkDeniedIsSilentLogsOnceAndContinues(t *testing.T) {
	denied := lifecycletest.New()
	denied.Err = func(string, any) error {
		return &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform dynamodb:DescribeTable"}
	}
	calls := 0
	var buf bytes.Buffer
	debug := install(t, func(aws.Config, string) (lifecycle.API, error) { calls++; return denied, nil }, &buf)

	for i := 0; i < 3; i++ {
		Mark(context.Background(), cfgFor("AKIA1"), "us-east-1", ScopeGlobal)
	}
	if buf.Len() != 0 {
		t.Fatalf("a denied mark printed to the user:\n%s", buf.String())
	}
	log := debug.String()
	if n := strings.Count(log, "could not mark spawn active"); n != 1 {
		t.Fatalf("%d diagnostic lines, want exactly 1:\n%s", n, log)
	}
	for _, want := range append([]string{lifecycle.TableName, "never tears it down", "spawn doctor", TableARNPattern}, RequiredActions...) {
		if !strings.Contains(log, want) {
			t.Errorf("the diagnostic line does not say %q:\n%s", want, log)
		}
	}
	if calls != 1 {
		t.Errorf("the lease client was built %d times; a refused identity must not be asked again", calls)
	}
}

// A passing failure (throttling) is also silent and logged once, but does
// not stop the next mark from trying.
func TestMarkTransientFailureIsSilent(t *testing.T) {
	f := lifecycletest.New()
	fail := true
	f.Err = func(string, any) error {
		if fail {
			return &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}
		}
		return nil
	}
	var buf bytes.Buffer
	debug := install(t, func(aws.Config, string) (lifecycle.API, error) { return f, nil }, &buf)
	Mark(context.Background(), cfgFor("AKIA1"), "us-east-1")
	Mark(context.Background(), cfgFor("AKIA1"), "us-east-1")
	fail = false
	Mark(context.Background(), cfgFor("AKIA1"), "us-east-1")
	if buf.Len() != 0 {
		t.Errorf("a failed mark printed to the user:\n%s", buf.String())
	}
	if n := strings.Count(debug.String(), "could not mark spawn active"); n != 1 {
		t.Errorf("%d diagnostic lines, want 1:\n%s", n, debug.String())
	}
	if it := item(t, f, "us-east-1"); attr(it, "state") != string(lifecycle.StateActive) {
		t.Errorf("after the throttling cleared, state %q, want ACTIVE", attr(it, "state"))
	}
}

// In a test binary the default factory never reaches AWS: a test gets marks
// only by installing a Factory.
func TestDefaultFactoryRefusesInTests(t *testing.T) {
	if _, err := defaultFactory(cfgFor("AKIA1"), "us-east-1"); err != errInTest {
		t.Fatalf("defaultFactory in a test binary: %v, want errInTest", err)
	}
	var buf bytes.Buffer
	restore := Override(defaultFactory, &buf, func(string, ...any) {})
	defer restore()
	Mark(context.Background(), cfgFor("AKIA1"), "us-east-1")
	if buf.Len() != 0 {
		t.Errorf("the test-binary no-op printed %q", buf.String())
	}
}

// Nothing is marked in spore.host's infra account: no idle teardown runs
// there, so a mark would only leave a lifecycle table beside the service.
func TestMarkSkipsTheInfraAccount(t *testing.T) {
	fs := fakes{}
	var buf bytes.Buffer
	install(t, fs.factory, &buf)
	std.mu.Lock()
	prev := std.accountOf
	std.accountOf = func(context.Context, aws.Config) (string, error) { return footprintspawn.InfraAccountID, nil }
	std.mu.Unlock()
	t.Cleanup(func() { std.mu.Lock(); std.accountOf = prev; std.mu.Unlock() })

	Mark(context.Background(), cfgFor("AKIA1"), "us-east-1", ScopeGlobal)
	if len(fs) != 0 || buf.Len() != 0 {
		t.Errorf("marked in the infra account: tables in %d regions, output %q", len(fs), buf.String())
	}
}

// A long wait on the lease (a teardown holds it) is the one thing a user
// sees, and only past waitNotice; the table being created on a region's first
// mark is a short wait and only logged.
func TestMarkSaysWhenItWaitsLong(t *testing.T) {
	var buf bytes.Buffer
	debug := install(t, fakes{}.factory, &buf)
	sleep := std.sleeper([]string{"us-east-1"})
	for waited := time.Duration(0); waited < waitNotice-lifecycle.DefaultTablePollInterval; waited += lifecycle.DefaultTablePollInterval {
		_ = sleep(context.Background(), lifecycle.DefaultTablePollInterval)
	}
	if buf.Len() != 0 {
		t.Fatalf("a short wait printed to the user: %q", buf.String())
	}
	if !strings.Contains(debug.String(), "waiting on the") {
		t.Errorf("the wait was not logged: %q", debug.String())
	}
	for i := 0; i < 5; i++ {
		_ = sleep(context.Background(), lifecycle.DefaultPollInterval)
	}
	if n := strings.Count(buf.String(), "waiting for spawn's teardown lease"); n != 1 {
		t.Errorf("a long wait said so %d times, want once: %q", n, buf.String())
	}
}

// Probe (spawn doctor) tells a usable table from a refused one and an absent
// one, and writes nothing.
func TestProbe(t *testing.T) {
	ctx := context.Background()
	f := lifecycletest.New()
	if err := Probe(ctx, f); !errors.Is(err, ErrNoTable) {
		t.Fatalf("no table: %v, want ErrNoTable", err)
	}
	c, err := lifecycle.New(lifecycle.Options{DynamoDB: func(string) (lifecycle.API, error) { return f, nil },
		Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MarkActive(ctx, Tool, "us-east-1"); err != nil {
		t.Fatal(err)
	}
	before := len(f.Keys(lifecycle.TableName))
	if err := Probe(ctx, f); err != nil {
		t.Fatalf("a usable table: %v", err)
	}
	if after := len(f.Keys(lifecycle.TableName)); after != before {
		t.Errorf("the probe wrote an item (%d -> %d keys): it would keep the table from ever closing", before, after)
	}
	f.Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: dynamodb:UpdateItem"}
		}
		return nil
	}
	if err := Probe(ctx, f); err == nil || errors.Is(err, ErrNoTable) || !strings.Contains(err.Error(), "UpdateItem") {
		t.Errorf("a refused write: %v, want the UpdateItem refusal", err)
	}
}
