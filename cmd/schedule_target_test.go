package cmd

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awsscheduler "github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1383: `spawn schedule create --region us-west-2` must target the
// scheduler-handler that exists. scripts/deploy-scheduler-handler.sh deploys
// it in one region only; --region is where the sweep's members launch, not
// where the handler is.
func TestScheduleTargetIsTheDeployedHandlerWhateverTheRegion(t *testing.T) {
	src, err := os.ReadFile("../scripts/deploy-scheduler-handler.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^REGION="\$\{SPAWN_LAMBDA_REGION:-([a-z0-9-]+)\}"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("cannot find the deploy region (REGION=...) in scripts/deploy-scheduler-handler.sh")
	}
	deployRegion := string(m[1])

	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	if _, err := env.DynamoClient().CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            awssdk.String(scheduler.TableName),
		BillingMode:          dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{{AttributeName: awssdk.String("schedule_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS}},
		KeySchema:            []dynamodbtypes.KeySchemaElement{{AttributeName: awssdk.String("schedule_id"), KeyType: dynamodbtypes.KeyTypeHash}},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	old := scheduleRegion
	t.Cleanup(func() { scheduleRegion = old })
	for _, region := range []string{"us-west-2", "eu-west-1", deployRegion} {
		scheduleRegion = region
		id := "sched-target-" + region
		if _, err := scheduleTargetClient(env.AWSConfig, "123456789012").CreateSchedule(ctx, &scheduler.ScheduleRecord{
			ScheduleID: id, ScheduleExpression: "cron(0 2 * * ? *)", ScheduleType: "recurring",
			Region: region, Status: "active",
		}); err != nil {
			t.Fatalf("--region %s: CreateSchedule: %v", region, err)
		}
		out, err := awsscheduler.NewFromConfig(env.AWSConfig).GetSchedule(ctx, &awsscheduler.GetScheduleInput{Name: awssdk.String(id)})
		if err != nil {
			t.Fatalf("GetSchedule: %v", err)
		}
		a, err := arn.Parse(awssdk.ToString(out.Target.Arn))
		if err != nil {
			t.Fatalf("target %q: %v", awssdk.ToString(out.Target.Arn), err)
		}
		if a.Region != deployRegion || !strings.HasSuffix(a.Resource, ":"+scheduler.HandlerFunctionName) {
			t.Errorf("--region %s: target %s is not the deployed scheduler-handler in %s; every firing fails delivery",
				region, awssdk.ToString(out.Target.Arn), deployRegion)
		}
	}
}
