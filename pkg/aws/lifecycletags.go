package aws

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// CreatedTagKey is the creation timestamp the reaper's net-resource sweep reads
// to decide whether a resource is past its grace period.
//
// It is NOT the same key as spawn:created-at (written by `spawn bot`) or
// spawn:created-from (written by `spawn create-ami`). Those are unrelated, and
// the near-collision is a good reason to go through LifecycleTags below rather
// than hand-writing the string.
const CreatedTagKey = "spawn:created"

// LifecycleTags returns the tags every spawn-created network resource must carry
// to be reclaimable: the managed rail, a purpose, and a creation timestamp.
//
// The creation timestamp is the one that was missing. Neither
// DescribeSecurityGroups nor DescribePlacementGroups returns a creation time, so
// spawn's own tag is the ONLY source — and nothing wrote it. The reaper's sweep
// therefore skipped every resource it found, deliberately:
//
//	// No creation stamp. Skipped rather than guessed: an untimed group
//	// could be seconds old, and deleting a group a launch is about to use
//	// would break that launch.
//
// That reasoning is correct in isolation. Combined with no writer, it made every
// orphan permanently uncollectable: the creating path could not delete them
// (#752) and the reaper would not. A live sweep found 21 such security groups
// across three regions — all `eni=0`, all `spawn:managed=true`, none with a
// creation stamp — which is essentially the full set #685 originally reported,
// still present after the reaper shipped.
//
// Returned as a slice so a caller can append its own tags, and kept in one place
// so a new creation path cannot forget the stamp. TestEveryNetResourceCreationIsTagged
// enforces that.
func LifecycleTags(name, purpose string, now time.Time) []types.Tag {
	tags := []types.Tag{
		{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		{Key: aws.String(CreatedTagKey), Value: aws.String(now.UTC().Format(time.RFC3339))},
	}
	if name != "" {
		tags = append(tags, types.Tag{Key: aws.String("Name"), Value: aws.String(name)})
	}
	if purpose != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:purpose"), Value: aws.String(purpose)})
	}
	return tags
}
