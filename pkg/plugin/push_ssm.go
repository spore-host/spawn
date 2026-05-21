package plugin

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMPushClient pushes plugin values via AWS SSM Parameter Store.
// Used as a fallback when SSH is not available (e.g., hardened environments).
//
// Parameters are stored at:
//
//	/spore-host/plugins/<instanceID>/<pluginName>/<key>
//
// spored polls for these parameters when in StatusWaitingForPush.
type SSMPushClient struct {
	instanceID string
	client     *ssm.Client
}

// NewSSMPushClient creates a push client backed by SSM Parameter Store.
func NewSSMPushClient(ctx context.Context, instanceID, region string) (*SSMPushClient, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &SSMPushClient{
		instanceID: instanceID,
		client:     ssm.NewFromConfig(cfg),
	}, nil
}

// Push stores the key/value as a SecureString in SSM Parameter Store.
func (c *SSMPushClient) Push(ctx context.Context, pluginName, key, value string) error {
	name := fmt.Sprintf("/spore-host/plugins/%s/%s/%s", c.instanceID, pluginName, key)

	_, err := c.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(name),
		Value:     aws.String(value),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("put SSM parameter %s: %w", name, err)
	}
	return nil
}
