package lastmilespawn

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/sweep"
)

// ScanAPI is the DynamoDB read InfraSignalsReadable makes.
// *dynamodb.Client satisfies it.
type ScanAPI interface {
	Scan(ctx context.Context, in *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// InfraTables are the infra account's tables whose records say an
// orchestrator is working for an account: sweeps, pipelines and schedules.
// The reaper's account-idle sweep reads the same three
// (lambda/ttl-reaper/accountidle.go). All are in scheduler.TableRegion.
func InfraTables() []string {
	return []string{sweep.DefaultResourceNames().DynamoTableName, pipeline.DefaultTableName, scheduler.TableName}
}

// InfraSignalsReadable reports whether api (the infra account's DynamoDB, on
// the caller's credentials) can read each of InfraTables: nil when every one
// answers a one-item scan, or is not there (as the reaper's sweep reads a
// missing table: no orchestrator works from it). It is one condition of the
// last mile's FullVisibility: a caller that cannot read these cannot see the
// launches no CLI marks.
func InfraSignalsReadable(ctx context.Context, api ScanAPI) error {
	if api == nil {
		return errors.New("no client for the infra account's tables")
	}
	var errs []error
	for _, table := range InfraTables() {
		_, err := api.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(table), Limit: aws.Int32(1)})
		var nf *ddbtypes.ResourceNotFoundException
		if err != nil && !errors.As(err, &nf) {
			errs = append(errs, fmt.Errorf("scan %s: %w", table, err))
		}
	}
	return errors.Join(errs...)
}

// InfraSignalsReadableIn is InfraSignalsReadable over cfg (the infra
// account's config, spawnconfig.LoadInfraAWSConfig).
func InfraSignalsReadableIn(ctx context.Context, cfg aws.Config) error {
	return InfraSignalsReadable(ctx, dynamodb.NewFromConfig(cfg))
}
