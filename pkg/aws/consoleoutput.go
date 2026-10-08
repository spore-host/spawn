package aws

import (
	"context"
	"encoding/base64"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// GetConsoleOutput returns an instance's serial console output.
//
// The only way to read a diagnostic off an instance that no longer exists.
// Measured behaviour, which the callers depend on:
//
//   - It OUTLIVES the instance's visibility in DescribeInstances. Instances that
//     DescribeInstances no longer returns at all still answered here more than
//     twelve hours later.
//   - The post-termination capture is NOT immediate: it takes roughly four to
//     five minutes to populate, and before then the API returns an empty or
//     boot-only dump rather than a partial one. A caller must not report that as
//     "no log" — an immediate read is indistinguishable from a missing one.
//   - Content is capped (~64 KB) and a baseline boot already consumes 13-17 KB,
//     which is why spored writes a 50-line tail rather than a whole log (#736).
//
// Deliberately NOT using Latest: the default returns the cached capture, which is
// what survives termination. Latest asks the running instance, which is useless
// for the case this exists to serve, and is unsupported on some instance types.
//
// The SDK returns Output BASE64-ENCODED and does not decode it — "If you are
// using a command line tool, the tool decodes the output for you" is the AWS
// CLI, not this. Decoding here means every caller does not have to remember
// that, and the first draft of this function got it wrong by assuming the
// `aws ec2 get-console-output --query Output --output text` behaviour carried
// over.
func (c *Client) GetConsoleOutput(ctx context.Context, region, instanceID string) (string, error) {
	out, err := c.regionalEC2(region).GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{
		InstanceId: awssdk.String(instanceID),
	})
	if err != nil {
		return "", fmt.Errorf("get console output for %s: %w", instanceID, err)
	}
	raw := awssdk.ToString(out.Output)
	if raw == "" {
		// Not an error: an instance whose capture has not populated yet answers
		// successfully with nothing. The caller distinguishes that from
		// "no log" using the instance's termination time.
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode console output for %s: %w", instanceID, err)
	}
	return string(decoded), nil
}
