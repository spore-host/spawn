package provider

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// spawn:burst=true — the tag `spawn burst` writes — is what marks an EC2
// job-array member as hybrid, so spored registers it with the hybrid registry
// (#904). Its absence (an ordinary `spawn launch --count` member) must not.
func TestLoadConfigFromEC2Tags_Burst(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *string
		want  bool
	}{
		{"spawn:burst=true", aws.String("true"), true},
		{"spawn:burst=false", aws.String("false"), false},
		{"no spawn:burst tag", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, id := newTestEC2Provider(t)
			tags := []ec2types.Tag{{Key: aws.String(tagprefix.Tag("job-array-id")), Value: aws.String("ja-1")}}
			if tc.value != nil {
				tags = append(tags, ec2types.Tag{Key: aws.String(tagprefix.Tag("burst")), Value: tc.value})
			}
			if _, err := p.ec2Client.CreateTags(context.Background(), &ec2.CreateTagsInput{
				Resources: []string{id}, Tags: tags,
			}); err != nil {
				t.Fatalf("CreateTags: %v", err)
			}
			cfg, _, err := loadConfigFromEC2Tags(context.Background(), p.ec2Client, id)
			if err != nil {
				t.Fatalf("loadConfigFromEC2Tags: %v", err)
			}
			if cfg.JobArrayID != "ja-1" {
				t.Fatalf("JobArrayID = %q, want ja-1", cfg.JobArrayID)
			}
			if cfg.Burst != tc.want {
				t.Errorf("Burst = %v, want %v", cfg.Burst, tc.want)
			}
		})
	}
}
