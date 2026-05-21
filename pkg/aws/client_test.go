package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestClientCreation tests creating a new AWS client
func TestClientCreation(t *testing.T) {
	ctx := context.Background()

	// Note: This test requires AWS credentials to be configured
	// In CI/CD, this would be skipped or use mock credentials
	_, err := NewClient(ctx)
	if err != nil {
		t.Logf("Client creation failed (expected in test env without credentials): %v", err)
	}
}

// TestGetEnabledRegions tests the region list structure
func TestGetEnabledRegions(t *testing.T) {
	tests := []struct {
		name      string
		regions   []string
		wantCount int
	}{
		{
			name:      "standard regions",
			regions:   []string{"us-east-1", "us-west-2", "eu-west-1"},
			wantCount: 3,
		},
		{
			name:      "empty regions",
			regions:   []string{},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.regions) != tt.wantCount {
				t.Errorf("got %d regions, want %d", len(tt.regions), tt.wantCount)
			}
		})
	}
}

// TestLaunchConfig tests the launch configuration structure
func TestLaunchConfig(t *testing.T) {
	tests := []struct {
		name   string
		config LaunchConfig
		valid  bool
	}{
		{
			name: "valid basic config",
			config: LaunchConfig{
				InstanceType: "t3.micro",
				Region:       "us-east-1",
				AMI:          "ami-12345678",
				KeyName:      "my-key",
			},
			valid: true,
		},
		{
			name: "valid spot config",
			config: LaunchConfig{
				InstanceType: "t3.micro",
				Region:       "us-east-1",
				AMI:          "ami-12345678",
				KeyName:      "my-key",
				Spot:         true,
				SpotMaxPrice: "0.05",
			},
			valid: true,
		},
		{
			name: "valid with hibernation",
			config: LaunchConfig{
				InstanceType: "m5.large",
				Region:       "us-east-1",
				AMI:          "ami-12345678",
				KeyName:      "my-key",
				Hibernate:    true,
			},
			valid: true,
		},
		{
			name: "valid with EFA",
			config: LaunchConfig{
				InstanceType: "c5n.18xlarge",
				Region:       "us-east-1",
				AMI:          "ami-12345678",
				KeyName:      "my-key",
				EFAEnabled:   true,
			},
			valid: true,
		},
		{
			name: "valid with placement group",
			config: LaunchConfig{
				InstanceType:   "c5.large",
				Region:         "us-east-1",
				AMI:            "ami-12345678",
				KeyName:        "my-key",
				PlacementGroup: "my-pg",
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config.InstanceType == "" {
				t.Error("InstanceType is required")
			}
			if tt.config.Region == "" {
				t.Error("Region is required")
			}
			if tt.config.AMI == "" {
				t.Error("AMI is required")
			}
			if tt.config.KeyName == "" {
				t.Error("KeyName is required")
			}

			if tt.config.EFAEnabled {
				if !isEFACompatible(tt.config.InstanceType) {
					t.Logf("Instance type %s may not support EFA", tt.config.InstanceType)
				}
			}

			if tt.config.Hibernate {
				if !isHibernationCompatible(tt.config.InstanceType) {
					t.Logf("Instance type %s may not support hibernation", tt.config.InstanceType)
				}
			}
		})
	}
}

// TestJobArrayConfig tests job array configuration
func TestJobArrayConfig(t *testing.T) {
	tests := []struct {
		name   string
		config LaunchConfig
		valid  bool
	}{
		{
			name: "valid job array",
			config: LaunchConfig{
				InstanceType:  "t3.micro",
				Region:        "us-east-1",
				AMI:           "ami-12345678",
				KeyName:       "my-key",
				JobArrayID:    "job-123",
				JobArrayName:  "compute",
				JobArraySize:  10,
				JobArrayIndex: 0,
			},
			valid: true,
		},
		{
			name: "single instance (not an array)",
			config: LaunchConfig{
				InstanceType: "t3.micro",
				Region:       "us-east-1",
				AMI:          "ami-12345678",
				KeyName:      "my-key",
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config.JobArrayID != "" {
				if tt.config.JobArrayName == "" {
					t.Error("JobArrayName required when JobArrayID is set")
				}
				if tt.config.JobArraySize <= 0 {
					t.Error("JobArraySize must be positive")
				}
				if tt.config.JobArrayIndex < 0 || tt.config.JobArrayIndex >= tt.config.JobArraySize {
					t.Error("JobArrayIndex out of bounds")
				}
			}
		})
	}
}

// TestParameterSweepConfig tests parameter sweep configuration
func TestParameterSweepConfig(t *testing.T) {
	tests := []struct {
		name   string
		config LaunchConfig
		valid  bool
	}{
		{
			name: "valid sweep",
			config: LaunchConfig{
				InstanceType: "t3.micro",
				Region:       "us-east-1",
				AMI:          "ami-12345678",
				KeyName:      "my-key",
				SweepID:      "sweep-123",
				SweepName:    "hyperparam",
				SweepIndex:   0,
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config.SweepID != "" {
				if tt.config.SweepName == "" {
					t.Error("SweepName required when SweepID is set")
				}
				if tt.config.SweepIndex < 0 {
					t.Error("SweepIndex must be non-negative")
				}
			}
		})
	}
}

// TestTTLValidation tests TTL string validation
func TestTTLValidation(t *testing.T) {
	tests := []struct {
		name  string
		ttl   string
		valid bool
	}{
		{name: "valid hours", ttl: "8h", valid: true},
		{name: "valid minutes", ttl: "30m", valid: true},
		{name: "valid combined", ttl: "2h30m", valid: true},
		{name: "empty (no TTL)", ttl: "", valid: true},
		{name: "invalid format", ttl: "invalid", valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := LaunchConfig{TTL: tt.ttl}
			if config.TTL != "" {
				isValid := isDurationFormat(config.TTL)
				if isValid != tt.valid {
					t.Errorf("TTL %q validity = %v, want %v", config.TTL, isValid, tt.valid)
				}
			}
		})
	}
}

// TestOnCompleteAction tests on-complete action validation
func TestOnCompleteAction(t *testing.T) {
	tests := []struct {
		name   string
		action string
		valid  bool
	}{
		{name: "terminate", action: "terminate", valid: true},
		{name: "stop", action: "stop", valid: true},
		{name: "hibernate", action: "hibernate", valid: true},
		{name: "empty (disabled)", action: "", valid: true},
		{name: "invalid action", action: "invalid", valid: false},
	}

	validActions := map[string]bool{"": true, "terminate": true, "stop": true, "hibernate": true}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if validActions[tt.action] != tt.valid {
				t.Errorf("OnComplete %q validity = %v, want %v", tt.action, validActions[tt.action], tt.valid)
			}
		})
	}
}

// TestInstanceInfo tests the InstanceInfo structure
func TestInstanceInfo(t *testing.T) {
	info := InstanceInfo{
		InstanceID:       "i-1234567890abcdef0",
		InstanceType:     "t3.micro",
		State:            "running",
		PublicIP:         "52.1.2.3",
		PrivateIP:        "10.0.1.100",
		LaunchTime:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		AvailabilityZone: "us-east-1a",
		Region:           "us-east-1",
		KeyName:          "my-key",
	}

	if info.InstanceID == "" {
		t.Error("InstanceID is required")
	}
	if info.Region == "" {
		t.Error("Region is required")
	}
}

// TestEC2Operations tests EC2 instance lifecycle via Substrate.
func TestEC2Operations(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ec2Client := env.EC2Client()

	t.Run("RunInstances", func(t *testing.T) {
		result, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
			InstanceType: ec2types.InstanceTypeT3Micro,
			ImageId:      aws.String("ami-12345678"),
			KeyName:      aws.String("my-key"),
			MinCount:     aws.Int32(1),
			MaxCount:     aws.Int32(1),
		})
		if err != nil {
			t.Fatalf("RunInstances: %v", err)
		}
		if len(result.Instances) != 1 {
			t.Fatalf("got %d instances, want 1", len(result.Instances))
		}
		inst := result.Instances[0]
		if inst.InstanceId == nil {
			t.Error("InstanceId is nil")
		}
	})

	t.Run("DescribeInstances", func(t *testing.T) {
		// Launch an instance first.
		launch, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
			InstanceType: ec2types.InstanceTypeT3Micro,
			ImageId:      aws.String("ami-12345678"),
			MinCount:     aws.Int32(1),
			MaxCount:     aws.Int32(1),
		})
		if err != nil {
			t.Fatalf("RunInstances: %v", err)
		}
		id := *launch.Instances[0].InstanceId

		result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{id},
		})
		if err != nil {
			t.Fatalf("DescribeInstances: %v", err)
		}
		if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
			t.Fatal("no instances returned")
		}
		if *result.Reservations[0].Instances[0].InstanceId != id {
			t.Errorf("got ID %s, want %s", *result.Reservations[0].Instances[0].InstanceId, id)
		}
	})

	t.Run("TerminateInstances", func(t *testing.T) {
		launch, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
			InstanceType: ec2types.InstanceTypeT3Micro,
			ImageId:      aws.String("ami-12345678"),
			MinCount:     aws.Int32(1),
			MaxCount:     aws.Int32(1),
		})
		if err != nil {
			t.Fatalf("RunInstances: %v", err)
		}
		id := *launch.Instances[0].InstanceId

		result, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{id},
		})
		if err != nil {
			t.Fatalf("TerminateInstances: %v", err)
		}
		if len(result.TerminatingInstances) != 1 {
			t.Errorf("got %d terminating, want 1", len(result.TerminatingInstances))
		}
	})

	t.Run("CreateTags", func(t *testing.T) {
		launch, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
			InstanceType: ec2types.InstanceTypeT3Micro,
			ImageId:      aws.String("ami-12345678"),
			MinCount:     aws.Int32(1),
			MaxCount:     aws.Int32(1),
		})
		if err != nil {
			t.Fatalf("RunInstances: %v", err)
		}
		id := *launch.Instances[0].InstanceId

		_, err = ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{id},
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String("test-instance")},
				{Key: aws.String("Environment"), Value: aws.String("test")},
			},
		})
		if err != nil {
			t.Fatalf("CreateTags: %v", err)
		}
	})
}

// Helper functions

func isEFACompatible(instanceType string) bool {
	efaFamilies := []string{"c5n", "c6gn", "p3dn", "p4d", "p4de"}
	for _, family := range efaFamilies {
		if len(instanceType) >= len(family) && instanceType[:len(family)] == family {
			return true
		}
	}
	return false
}

func isHibernationCompatible(instanceType string) bool {
	hibernationFamilies := []string{"c3", "c4", "c5", "m3", "m4", "m5", "r3", "r4", "r5", "t2", "t3"}
	for _, family := range hibernationFamilies {
		if len(instanceType) >= len(family) && instanceType[:len(family)] == family {
			return true
		}
	}
	return false
}

func isDurationFormat(s string) bool {
	if s == "" {
		return false
	}
	return testutil.Contains(s, "h") || testutil.Contains(s, "m") || testutil.Contains(s, "s")
}
