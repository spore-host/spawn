package instancerole

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/scheduler"
	"gopkg.in/yaml.v3"
)

// ScheduleScanner is the one DynamoDB call ScheduleSource makes.
type ScheduleScanner interface {
	Scan(ctx context.Context, in *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// ScheduleParamsGetter is the one S3 call ScheduleSource makes.
type ScheduleParamsGetter interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// ScheduleSource is the schedules `spawn schedule create` writes (#1378): the
// records in scheduler.TableName, and each one's params object, which is where
// the instance profile its members launch with is named.
//
// What counts. A schedule protects the profiles in its params when
// scheduler-handler would launch it if it fired now (ScheduleRecord.FiringAt
// is FiringLaunches, the handler's own check): status active, under its
// max-executions, before its end-after. A paused or cancelled schedule does
// not, and nor does a deleted one (its record is gone). A schedule that has run
// out (max executions reached, or end-after passed) does not either: the
// handler refuses every later firing of it, and no command raises those limits
// on an existing record. A one-time schedule that has already fired still
// counts: the handler would launch it again if EventBridge fired it, and
// keeping a role costs one role.
//
// What it reads in the params: the sweep orchestrator gives each member the
// instance profile named by iam_role, a row's own value over the defaults'
// (lambda/sweep-orchestrator spored.go), and nothing else names one. So
// defaults.iam_role and every row's iam_role are references. The record
// itself names no profile.
//
// It fails, so nothing is deleted, on anything it cannot read completely: a
// Scan error on any page (AccessDenied, a missing table, throttling), a record
// it cannot decode, and, for a schedule that counts, a params object that is
// missing, unreadable or undecodable. The schedule's ID is in the error, so an
// operator can fix or cancel it.
type ScheduleSource struct {
	// DB scans Table. Table must be in the account and region the CLI writes
	// schedules to (scheduler.TableRegion).
	DB    ScheduleScanner
	Table string
	// Params returns an S3 client for a schedule's region, where its params
	// bucket is.
	Params func(region string) ScheduleParamsGetter
	// BucketTemplate is the params bucket for a record that names none
	// (written before #804); scheduler.ParamsBucketTemplate when empty.
	BucketTemplate string
	// Now is the time a schedule's limits are checked against.
	Now func() time.Time
}

// NewScheduleSource reads the schedules where `spawn schedule create` writes
// them, with cfg's credentials: the table scheduler.TableName in
// scheduler.TableRegion, and each params object in its schedule's region.
// cfg's own region is not used, so the reaper (wherever it is deployed) and
// the CLI read the same table.
func NewScheduleSource(cfg aws.Config) *ScheduleSource {
	tc := cfg.Copy()
	tc.Region = scheduler.TableRegion
	return &ScheduleSource{
		DB:    dynamodb.NewFromConfig(tc),
		Table: scheduler.TableName,
		Params: func(region string) ScheduleParamsGetter {
			c := cfg.Copy()
			c.Region = region
			return s3.NewFromConfig(c)
		},
		Now: time.Now,
	}
}

// Describe implements ReferenceSource.
func (s *ScheduleSource) Describe() string {
	return fmt.Sprintf("schedules (DynamoDB table %s in %s)", s.Table, scheduler.TableRegion)
}

// ScheduleLabel is how a schedule names itself as a reference.
func ScheduleLabel(id string) string { return "schedule " + id }

// scheduleParams is the params file as scheduler-handler decodes it.
type scheduleParams struct {
	Defaults map[string]interface{}   `yaml:"defaults"`
	Params   []map[string]interface{} `yaml:"params"`
}

// ProfileReferences implements ReferenceSource.
func (s *ScheduleSource) ProfileReferences(ctx context.Context) (map[string][]string, error) {
	if s.DB == nil || s.Params == nil || s.Table == "" || s.Now == nil {
		return nil, errors.New("schedule source is not configured")
	}
	now := s.Now()
	tmpl := s.BucketTemplate
	if tmpl == "" {
		tmpl = scheduler.ParamsBucketTemplate
	}

	var records []scheduler.ScheduleRecord
	p := dynamodb.NewScanPaginator(s.DB, &dynamodb.ScanInput{
		TableName: aws.String(s.Table),
		// A schedule written just before the sweep must be seen.
		ConsistentRead: aws.Bool(true),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", s.Table, err)
		}
		for _, item := range page.Items {
			var rec scheduler.ScheduleRecord
			if err := attributevalue.UnmarshalMap(item, &rec); err != nil {
				return nil, fmt.Errorf("decode a schedule record in %s: %w", s.Table, err)
			}
			records = append(records, rec)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ScheduleID < records[j].ScheduleID })

	refs := map[string][]string{}
	for i := range records {
		rec := &records[i]
		if rec.FiringAt(now) != scheduler.FiringLaunches {
			continue
		}
		names, err := s.paramsProfiles(ctx, rec, tmpl)
		if err != nil {
			return nil, fmt.Errorf("schedule %s is active but its params cannot be read (fix it or `spawn schedule cancel %s`): %w",
				rec.ScheduleID, rec.ScheduleID, err)
		}
		for _, n := range names {
			refs[n] = append(refs[n], ScheduleLabel(rec.ScheduleID))
		}
	}
	return refs, nil
}

// paramsProfiles reads one schedule's params and returns every iam_role in it.
func (s *ScheduleSource) paramsProfiles(ctx context.Context, rec *scheduler.ScheduleRecord, tmpl string) ([]string, error) {
	if rec.S3ParamsKey == "" {
		return nil, errors.New("the record names no params key")
	}
	if rec.Region == "" {
		return nil, errors.New("the record names no region")
	}
	bucket := rec.ParamsBucketName(tmpl)
	out, err := s.Params(rec.Region).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(rec.S3ParamsKey),
	})
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", bucket, rec.S3ParamsKey, err)
	}
	defer func() { _ = out.Body.Close() }()
	var pf scheduleParams
	if err := yaml.NewDecoder(out.Body).Decode(&pf); err != nil {
		return nil, fmt.Errorf("decode s3://%s/%s: %w", bucket, rec.S3ParamsKey, err)
	}
	var names []string
	add := func(m map[string]interface{}) {
		if v, ok := m["iam_role"].(string); ok && normalizeProfileName(v) != "" {
			names = append(names, v)
		}
	}
	add(pf.Defaults)
	for _, row := range pf.Params {
		add(row)
	}
	return names, nil
}
