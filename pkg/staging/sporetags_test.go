package staging

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	libtags "github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/s3tags"
	"github.com/spore-host/libs/tags/tagtest"

	"github.com/spore-host/spawn/internal/sdkanswer"
)

// A schedules bucket ensureBucket creates carries spore-host:managed-by=spawn
// whatever it is for (#1652); only the schedule-params one is also the
// service's (spore-host#750). Checked by the tagtest Detector on a real
// client.
func TestSporeTags_SchedulesBucket(t *testing.T) {
	for _, tc := range []struct {
		kind        paramsKind
		wantService bool
	}{{kindQueueConfig, false}, {kindScheduleParams, true}} {
		var put map[string]string
		cfg := sdkanswer.Config("us-west-2", func(service, op string, in any) (any, error) {
			switch v := in.(type) {
			case *s3.HeadBucketInput:
				return nil, sdkanswer.Err("NotFound")
			case *s3.PutBucketTaggingInput:
				put = s3tags.ToMap(v.Tagging.TagSet)
			}
			return nil, nil
		})
		d := tagtest.New(t, libtags.ToolSpawn, tagtest.Allow{Service: "S3", Operation: "CreateBucket",
			Reason: "the schedules bucket is tagged by the PutBucketTagging that follows, which the Detector checks"})
		d.Install(&cfg)
		c := NewClient(cfg, "123456789012")
		if _, err := c.ensureBucket(context.Background(), "spawn-schedules-us-west-2", "us-west-2", tc.kind); err != nil {
			t.Fatal(err)
		}
		if put[libtags.ManagedBy] != "spawn" || put[libtags.Scope] != "us-west-2" {
			t.Errorf("kind %d: bucket tags = %v, want managed-by=spawn scoped to the region", tc.kind, put)
		}
		if got := put[libtags.Role] == libtags.RoleService; got != tc.wantService {
			t.Errorf("kind %d: role=service is %t, want %t", tc.kind, got, tc.wantService)
		}
	}
}
