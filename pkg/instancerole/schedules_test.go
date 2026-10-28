package instancerole

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/staging"
	"github.com/spore-host/spawn/pkg/testutil"
)

const schedAccount = "123456789012"

// schedEnv is a Substrate account holding the schedules table, written and
// read by the same code the CLI and the reaper use: scheduler.Client.
// SaveSchedule for the record, staging.UploadScheduleParams for the params,
// NewScheduleSource for the read.
type schedEnv struct {
	t      *testing.T
	env    *testutil.TestEnv
	db     *dynamodb.Client
	sched  *scheduler.Client
	stage  *staging.Client
	source *ScheduleSource
}

func newSchedEnv(t *testing.T, createTable bool) *schedEnv {
	t.Helper()
	env := testutil.SubstrateServer(t)
	e := &schedEnv{
		t: t, env: env, db: env.DynamoClient(),
		sched:  scheduler.NewClient(env.AWSConfig, "", "", schedAccount),
		stage:  staging.NewClient(env.AWSConfig, schedAccount),
		source: NewScheduleSource(env.AWSConfig),
	}
	if createTable {
		// The key schema of deployment/cloudformation/schedules-tables.yaml.
		if _, err := e.db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
			TableName:   aws.String(scheduler.TableName),
			BillingMode: dynamodbtypes.BillingModePayPerRequest,
			AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
				{AttributeName: aws.String("schedule_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			},
			KeySchema: []dynamodbtypes.KeySchemaElement{
				{AttributeName: aws.String("schedule_id"), KeyType: dynamodbtypes.KeyTypeHash},
			},
		}); err != nil {
			t.Fatalf("create schedules table: %v", err)
		}
	}
	return e
}

// schedule writes an active recurring schedule whose params are pf, as
// `spawn schedule create` does, and returns its record for adjustment.
// upload=false leaves the params object missing.
func (e *schedEnv) schedule(id string, pf map[string]interface{}, upload bool, adjust func(*scheduler.ScheduleRecord)) {
	e.t.Helper()
	ctx := context.Background()
	rec := &scheduler.ScheduleRecord{
		ScheduleID: id, UserID: schedAccount, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		ScheduleExpression: "cron(0 2 1 * ? *)", ScheduleType: string(scheduler.ScheduleTypeRecurring),
		Region: "us-east-1", AWSAccountID: "111122223333",
		Status: string(scheduler.ScheduleStatusActive), NextExecutionTime: time.Now().Add(30 * 24 * time.Hour),
		S3ParamsKey: "schedules/" + id + "/params.yaml",
	}
	if upload {
		b, err := json.Marshal(pf)
		if err != nil {
			e.t.Fatal(err)
		}
		p := filepath.Join(e.t.TempDir(), "params.json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			e.t.Fatal(err)
		}
		bucket, key, _, _, err := e.stage.UploadScheduleParams(ctx, p, id, rec.Region)
		if err != nil {
			e.t.Fatalf("upload params: %v", err)
		}
		rec.S3ParamsBucket, rec.S3ParamsKey = bucket, key
	}
	if adjust != nil {
		adjust(rec)
	}
	if err := e.sched.SaveSchedule(ctx, rec); err != nil {
		e.t.Fatalf("save schedule: %v", err)
	}
}

// sweep collects the schedule references and runs a reap sweep over roles,
// every one of them otherwise deletable, returning kept name -> reason and
// the deleted names.
func (e *schedEnv) sweep(roles ...string) (map[string]string, []string, error) {
	e.t.Helper()
	refs, err := CollectReferences(context.Background(), e.source)
	if err != nil {
		return nil, nil, err
	}
	f := &fakeIAM{roles: map[string]*fakeRole{}}
	for _, r := range roles {
		f.roles[r] = oldRole()
	}
	res, err := Sweep(context.Background(), f, map[string]bool{}, refs, time.Now(), true)
	if err != nil {
		e.t.Fatal(err)
	}
	kept := map[string]string{}
	var deleted []string
	for _, d := range res.Decisions {
		if d.Deleted {
			deleted = append(deleted, d.Name)
		} else {
			kept[d.Name] = d.Reason
		}
	}
	sort.Strings(deleted)
	return kept, deleted, nil
}

func params(defaultsRole string, rowRoles ...string) map[string]interface{} {
	d := map[string]interface{}{"instance_type": "t3.micro"}
	if defaultsRole != "" {
		d["iam_role"] = defaultsRole
	}
	rows := []map[string]interface{}{{"x": 0}}
	for i, r := range rowRoles {
		rows = append(rows, map[string]interface{}{"x": i + 1, "iam_role": r})
	}
	return map[string]interface{}{"defaults": d, "params": rows}
}

// Gate (#1378): an idle spawn-instance-* role named by an enabled schedule is
// kept, whether the defaults or a row names it, and the report says which
// schedule. A paused, cancelled or run-out schedule protects nothing.
func TestScheduleReferencedRolesAreKept(t *testing.T) {
	e := newSchedEnv(t, true)
	e.schedule("sched-defaults", params("spawn-instance-dflt"), true, nil)
	// ensureDetachedInstanceProfile seeds the shared profile into defaults
	// when a row has no iam_role of its own.
	e.schedule("sched-row", params(SharedProfileName, "spawn-instance-row"), true, nil)
	e.schedule("sched-paused", params("spawn-instance-paused"), true, func(r *scheduler.ScheduleRecord) {
		r.Status = string(scheduler.ScheduleStatusPaused)
	})
	e.schedule("sched-cancelled", params("spawn-instance-cancelled"), true, func(r *scheduler.ScheduleRecord) {
		r.Status = string(scheduler.ScheduleStatusCancelled)
	})
	e.schedule("sched-maxed", params("spawn-instance-maxed"), true, func(r *scheduler.ScheduleRecord) {
		r.MaxExecutions, r.ExecutionCount = 3, 3
	})
	e.schedule("sched-ended", params("spawn-instance-ended"), true, func(r *scheduler.ScheduleRecord) {
		r.EndAfter = time.Now().Add(-time.Hour)
	})

	kept, deleted, err := e.sweep("spawn-instance-dflt", "spawn-instance-row", "spawn-instance-paused",
		"spawn-instance-cancelled", "spawn-instance-maxed", "spawn-instance-ended", "spawn-instance-idle")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kept["spawn-instance-dflt"], "referenced by schedule sched-defaults"; got != want {
		t.Errorf("role named by an enabled schedule's defaults: kept for %q, want %q", got, want)
	}
	if got, want := kept["spawn-instance-row"], "referenced by schedule sched-row"; got != want {
		t.Errorf("role named by an enabled schedule's row: kept for %q, want %q", got, want)
	}
	want := []string{"spawn-instance-cancelled", "spawn-instance-ended", "spawn-instance-idle", "spawn-instance-maxed", "spawn-instance-paused"}
	if strings.Join(deleted, ",") != strings.Join(want, ",") {
		t.Errorf("deleted %q, want %q (kept: %v)", deleted, want, kept)
	}
}

// Gate (#1378): the same role, idle, is kept while its schedule is active and
// deleted once the schedule is paused or deleted.
func TestDisablingAScheduleReleasesItsRole(t *testing.T) {
	const role = "spawn-instance-weekly"
	for _, c := range []struct {
		name    string
		disable func(e *schedEnv)
	}{
		{"paused", func(e *schedEnv) {
			e.schedule("sched-weekly", params(role), true, func(r *scheduler.ScheduleRecord) {
				r.Status = string(scheduler.ScheduleStatusPaused)
			})
		}},
		{"deleted", func(e *schedEnv) {
			if _, err := e.db.DeleteItem(context.Background(), &dynamodb.DeleteItemInput{
				TableName: aws.String(scheduler.TableName),
				Key:       map[string]dynamodbtypes.AttributeValue{"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: "sched-weekly"}},
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newSchedEnv(t, true)
			e.schedule("sched-weekly", params(role), true, nil)
			kept, deleted, err := e.sweep(role)
			if err != nil {
				t.Fatal(err)
			}
			if len(deleted) != 0 || kept[role] != "referenced by schedule sched-weekly" {
				t.Fatalf("active: deleted %q, kept %v; want it kept as referenced", deleted, kept)
			}
			c.disable(e)
			kept, deleted, err = e.sweep(role)
			if err != nil {
				t.Fatal(err)
			}
			if len(deleted) != 1 || deleted[0] != role {
				t.Errorf("%s schedule: deleted %q, kept %v; want %s deleted", c.name, deleted, kept, role)
			}
		})
	}
}

// Gate (#1378): an enabled schedule whose params object is missing, or a
// schedules table that is not there, makes the whole collection fail, so no
// role is deleted; the error names the schedule. A paused schedule's missing
// params do not matter: nothing would launch from them.
func TestScheduleReferencesFailClosedOnSubstrate(t *testing.T) {
	t.Run("missing params object", func(t *testing.T) {
		e := newSchedEnv(t, true)
		e.schedule("sched-ok", params("spawn-instance-a"), true, nil)
		e.schedule("sched-lost", nil, false, func(r *scheduler.ScheduleRecord) {
			r.S3ParamsBucket = "spawn-schedules-us-east-1"
		})
		_, _, err := e.sweep("spawn-instance-idle")
		if err == nil || !strings.Contains(err.Error(), "sched-lost") {
			t.Errorf("a missing params object gave %v, want an error naming sched-lost", err)
		}
	})
	t.Run("missing params of a paused schedule", func(t *testing.T) {
		e := newSchedEnv(t, true)
		e.schedule("sched-lost", nil, false, func(r *scheduler.ScheduleRecord) {
			r.S3ParamsBucket = "spawn-schedules-us-east-1"
			r.Status = string(scheduler.ScheduleStatusPaused)
		})
		if _, deleted, err := e.sweep("spawn-instance-idle"); err != nil || len(deleted) != 1 {
			t.Errorf("paused schedule with no params: deleted %q, err %v; want the idle role deleted", deleted, err)
		}
	})
	t.Run("no schedules table", func(t *testing.T) {
		e := newSchedEnv(t, false)
		_, _, err := e.sweep("spawn-instance-idle")
		var re *ReferencesError
		if !errors.As(err, &re) {
			t.Errorf("a missing schedules table gave %v, want a ReferencesError", err)
		}
	})
}

// scripted is a ScheduleScanner returning pages, then failing with err.
type scripted struct {
	pages [][]map[string]dynamodbtypes.AttributeValue
	err   error
	calls int
}

func (s *scripted) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	s.calls++
	if !aws.ToBool(in.ConsistentRead) {
		return nil, errors.New("eventually consistent scan: a schedule written just now could be missed")
	}
	i := s.calls - 1
	if i >= len(s.pages) {
		return nil, s.err
	}
	out := &dynamodb.ScanOutput{Items: s.pages[i]}
	if i < len(s.pages)-1 || s.err != nil {
		out.LastEvaluatedKey = map[string]dynamodbtypes.AttributeValue{"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: "next"}}
	}
	return out, nil
}

// Gate (#1378): AccessDenied on the table, or a failure on a later page, is a
// failed collection, never the schedules read so far.
func TestScheduleReferencesFailClosedOnScanErrors(t *testing.T) {
	paused, err := attributevalue.MarshalMap(scheduler.ScheduleRecord{ScheduleID: "sched-p", Status: "paused"})
	if err != nil {
		t.Fatal(err)
	}
	src := func(s *scripted) *ScheduleSource {
		return &ScheduleSource{DB: s, Table: scheduler.TableName, Now: time.Now,
			Params: func(string) ScheduleParamsGetter { t.Fatal("read params of a paused schedule"); return nil }}
	}
	for name, s := range map[string]*scripted{
		"access denied":   {err: errors.New("AccessDeniedException: not authorized to perform dynamodb:Scan")},
		"second page":     {pages: [][]map[string]dynamodbtypes.AttributeValue{{paused}}, err: errors.New("ProvisionedThroughputExceededException")},
		"undecodable row": {pages: [][]map[string]dynamodbtypes.AttributeValue{{{"schedule_id": &dynamodbtypes.AttributeValueMemberN{Value: "1"}, "created_at": &dynamodbtypes.AttributeValueMemberBOOL{Value: true}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CollectReferences(context.Background(), src(s)); err == nil {
				t.Errorf("%s: collected references anyway", name)
			}
		})
	}
	ok := &scripted{pages: [][]map[string]dynamodbtypes.AttributeValue{{paused}, {paused}}}
	if _, err := CollectReferences(context.Background(), src(ok)); err != nil || ok.calls != 2 {
		t.Errorf("two clean pages: err %v after %d scans, want nil after 2", err, ok.calls)
	}
}
