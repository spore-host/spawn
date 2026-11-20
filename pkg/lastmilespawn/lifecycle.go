package lastmilespawn

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spore-host/libs/lifecycle"

	"github.com/spore-host/spawn/pkg/lifecyclemark"
)

// Owner is how the last mile's lifecycle client names itself. It takes no
// lease, so the name appears nowhere in a table.
const Owner = "spawn last mile (report)"

// TableAPI is the DynamoDB client of one region's lifecycle table.
type TableAPI func(region string) (lifecycle.API, error)

// RegionalTables is TableAPI over cfg: a DynamoDB client per region.
func RegionalTables(cfg aws.Config) TableAPI {
	return func(region string) (lifecycle.API, error) {
		c := cfg.Copy()
		c.Region = region
		return dynamodb.NewFromConfig(c), nil
	}
}

// NewLifecycleReader is the last mile's lifecycle client: each region's
// table through tables, wrapped by ReadOnlyLifecycle with one latch for all
// of them, and the global scope in lifecyclemark.ControlRegion, where spawn's
// creators and the reaper read and write it.
func NewLifecycleReader(tables TableAPI, now func() time.Time) (*lifecycle.Client, *Latch, error) {
	latch := &Latch{What: "read of the " + lifecycle.TableName + " table"}
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB: func(region string) (lifecycle.API, error) {
			a, err := tables(region)
			if err != nil {
				return nil, err
			}
			return ReadOnlyLifecycle{API: a, Latch: latch}, nil
		},
		ControlRegion: lifecyclemark.ControlRegion,
		Owner:         Owner,
		Now:           now,
	})
	return c, latch, err
}

// ReadOnlyLifecycle passes the lifecycle table's reads through and refuses
// its writes with ErrReportOnly, so the last mile changes nothing even if
// libs/lastmile tried. Once IAM has refused one read (Latch), it answers the
// rest itself.
type ReadOnlyLifecycle struct {
	lifecycle.API
	Latch *Latch
}

func (r ReadOnlyLifecycle) latch() *Latch {
	if r.Latch == nil {
		return &Latch{What: "read of the " + lifecycle.TableName + " table"}
	}
	return r.Latch
}

func (r ReadOnlyLifecycle) GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	l := r.latch()
	if err := l.Err(); err != nil {
		return nil, err
	}
	out, err := r.API.GetItem(ctx, in, opts...)
	l.Note(err)
	return out, err
}

func (r ReadOnlyLifecycle) Scan(ctx context.Context, in *dynamodb.ScanInput, opts ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	l := r.latch()
	if err := l.Err(); err != nil {
		return nil, err
	}
	out, err := r.API.Scan(ctx, in, opts...)
	l.Note(err)
	return out, err
}

func (r ReadOnlyLifecycle) DescribeTable(ctx context.Context, in *dynamodb.DescribeTableInput, opts ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	l := r.latch()
	if err := l.Err(); err != nil {
		return nil, err
	}
	out, err := r.API.DescribeTable(ctx, in, opts...)
	l.Note(err)
	return out, err
}

func (ReadOnlyLifecycle) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return nil, ErrReportOnly
}

func (ReadOnlyLifecycle) UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return nil, ErrReportOnly
}

func (ReadOnlyLifecycle) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return nil, ErrReportOnly
}

func (ReadOnlyLifecycle) TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	return nil, ErrReportOnly
}

func (ReadOnlyLifecycle) CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return nil, ErrReportOnly
}

func (ReadOnlyLifecycle) DeleteTable(context.Context, *dynamodb.DeleteTableInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error) {
	return nil, ErrReportOnly
}
