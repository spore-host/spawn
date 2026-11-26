package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"

	"github.com/spore-host/spawn/pkg/lifecyclemark"
)

// spawn#1672: the idle-teardown check says an identity cannot write the lease
// whenever a mark it made would be refused, including in a region that has
// no table yet, where DescribeTable alone proves nothing. It never says the
// first mark in such a region succeeds (its CreateTable and TagResource
// cannot be checked without creating the table), never calls a probe that
// proves nothing a pass or a refusal, and outside the aws partition neither
// probes the us-east-1 table Mark never marks from there nor points at the
// aws-only policy.
func TestProbeLease(t *testing.T) {
	const acct, who = "123456789012", "arn:aws:sts::123456789012:assumed-role/launcher/s"
	ctx := context.Background()

	marked := func(t *testing.T) *lifecycletest.Fake {
		t.Helper()
		f := lifecycletest.New()
		c, err := lifecycle.New(lifecycle.Options{DynamoDB: func(string) (lifecycle.API, error) { return f, nil }, ControlRegion: lifecyclemark.ControlRegion,
			Sleep: func(context.Context, time.Duration) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.MarkActive(ctx, lifecyclemark.Tool, lifecyclemark.ScopeGlobal); err != nil {
			t.Fatal(err)
		}
		return f
	}
	denied := func(f *lifecycletest.Fake, action string) *lifecycletest.Fake {
		f.Err = refuseLeaseAction(action)
		return f
	}

	throttled := func(f *lifecycletest.Fake) *lifecycletest.Fake {
		f.Err = func(op string, _ any) error {
			if op != "TransactWriteItems" {
				return nil
			}
			return &ddbtypes.TransactionCanceledException{Message: aws.String("Transaction cancelled"),
				CancellationReasons: []ddbtypes.CancellationReason{{Code: aws.String("ThrottlingError")}, {Code: aws.String("None")}}}
		}
		return f
	}
	// What the first mark in a table-less region needs and nothing checks.
	unchecked := []string{"dynamodb:CreateTable", "dynamodb:TagResource", "cannot be checked without creating it"}
	policy := "arn:aws:iam::" + acct + ":policy/" + lifecyclemark.UserPolicy

	cases := []struct {
		name    string
		region  string
		tables  map[string]*lifecycletest.Fake // nil entry: no table there
		warn    bool
		contain []string
		absent  []string
	}{
		{name: "writable here", region: "us-west-2",
			tables:  map[string]*lifecycletest.Fake{"us-west-2": marked(t)},
			contain: []string{"is writable in us-west-2"}},
		{name: "refused here", region: "us-west-2",
			tables: map[string]*lifecycletest.Fake{"us-west-2": denied(marked(t), "dynamodb:ConditionCheckItem")},
			warn:   true, contain: []string{who, "not authorized to perform: dynamodb:ConditionCheckItem", policy}},
		{name: "inconclusive here", region: "us-west-2",
			tables: map[string]*lifecycletest.Fake{"us-west-2": throttled(marked(t))},
			warn:   true, contain: []string{"not checked", "ThrottlingError"}, absent: []string{policy, "isn't writable"}},
		{name: "no table here, the control region's writable", region: "us-west-2",
			tables:  map[string]*lifecycletest.Fake{"us-west-2": nil, lifecyclemark.ControlRegion: marked(t)},
			contain: append([]string{"no spore-host-lifecycle table in us-west-2 yet", "allowed (checked on the us-east-1 table)"}, unchecked...),
			absent:  []string{"the first launch creates it"}},
		{name: "no table here, refused in the control region", region: "us-west-2",
			tables: map[string]*lifecycletest.Fake{"us-west-2": nil, lifecyclemark.ControlRegion: denied(marked(t), "dynamodb:UpdateItem")},
			warn:   true, contain: []string{who, "in us-east-1", "not authorized to perform: dynamodb:UpdateItem", policy}},
		{name: "no table here, inconclusive in the control region", region: "us-west-2",
			tables: map[string]*lifecycletest.Fake{"us-west-2": nil, lifecyclemark.ControlRegion: throttled(marked(t))},
			warn:   true, contain: append([]string{"not checked", "ThrottlingError"}, unchecked...), absent: []string{policy}},
		{name: "no table anywhere", region: "us-west-2",
			tables:  map[string]*lifecycletest.Fake{"us-west-2": nil, lifecyclemark.ControlRegion: nil},
			warn:    true,
			contain: append([]string{"not checked", "none in us-east-1 either", "spawn setup-account"}, unchecked...),
			absent:  []string{policy, "the first launch creates it"}},
		{name: "no table in the control region itself", region: lifecyclemark.ControlRegion,
			tables:  map[string]*lifecycletest.Fake{lifecyclemark.ControlRegion: nil},
			warn:    true,
			contain: append([]string{"not checked", "spawn setup-account"}, unchecked...), absent: []string{policy}},
		// Outside the aws partition Mark never marks the global scope, whose
		// table is in us-east-1, so the check never probes it (a probe of
		// a region missing from the fakes fails the case).
		{name: "GovCloud, writable here", region: "us-gov-west-1",
			tables:  map[string]*lifecycletest.Fake{"us-gov-west-1": marked(t)},
			contain: []string{"is writable in us-gov-west-1", "global scope", "not marked outside the aws partition"}},
		{name: "GovCloud, no table here", region: "us-gov-west-1",
			tables:  map[string]*lifecycletest.Fake{"us-gov-west-1": nil},
			warn:    true,
			contain: append([]string{"not checked", "not checked outside the aws partition"}, unchecked...),
			absent:  []string{"spawn setup-account`, which creates", policy}},
		{name: "China, refused here", region: "cn-north-1",
			tables:  map[string]*lifecycletest.Fake{"cn-north-1": denied(marked(t), "dynamodb:UpdateItem")},
			warn:    true,
			contain: []string{who, "in cn-north-1", "not authorized to perform: dynamodb:UpdateItem", "aws-partition ARNs"},
			absent:  []string{"arn:aws:iam::"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := func(region string) lifecycle.API {
				f, ok := tc.tables[region]
				if !ok {
					t.Fatalf("probed %s, which this case does not expect", region)
				}
				if f == nil {
					f = lifecycletest.New()
					tc.tables[region] = f
				}
				return f
			}
			before := map[string]int{}
			for r, f := range tc.tables {
				if f != nil {
					before[r] = len(f.Keys(lifecycle.TableName))
				}
			}
			detail, err := probeLease(ctx, acct, who, tc.region, api)
			got := detail
			if err != nil {
				got = err.Error()
			}
			if (err != nil) != tc.warn {
				t.Fatalf("warn = %v (%q), want %v", err != nil, got, tc.warn)
			}
			for _, want := range tc.contain {
				if !strings.Contains(got, want) {
					t.Errorf("%q does not say %q", got, want)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(got, bad) {
					t.Errorf("%q says %q", got, bad)
				}
			}
			for r, n := range before {
				if after := len(tc.tables[r].Keys(lifecycle.TableName)); after != n {
					t.Errorf("the check wrote to the %s table (%d -> %d keys)", r, n, after)
				}
			}
		})
	}
}

// refuseLeaseAction models IAM refusing one DynamoDB item action, for the
// single call or any transaction holding an item of that kind.
func refuseLeaseAction(action string) func(op string, in any) error {
	refuse := &smithy.GenericAPIError{Code: "AccessDeniedException",
		Message: "not authorized to perform: " + action + " on resource: table/" + lifecycle.TableName}
	return func(op string, in any) error {
		if "dynamodb:"+op == action {
			return refuse
		}
		tx, ok := in.(*dynamodb.TransactWriteItemsInput)
		if !ok {
			return nil
		}
		for _, ti := range tx.TransactItems {
			if (action == "dynamodb:ConditionCheckItem" && ti.ConditionCheck != nil) ||
				(action == "dynamodb:UpdateItem" && ti.Update != nil) {
				return refuse
			}
		}
		return nil
	}
}
