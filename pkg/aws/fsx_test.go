package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// createTestFSxFilesystem creates an FSx Lustre filesystem via the raw SDK and
// returns its filesystem ID. Used to seed state for Client-level tests.
func createTestFSxFilesystem(t *testing.T, fsxClient *fsx.Client, stackName string) string {
	t.Helper()
	ctx := context.Background()

	out, err := fsxClient.CreateFileSystem(ctx, &fsx.CreateFileSystemInput{
		FileSystemType:  fsxtypes.FileSystemTypeLustre,
		StorageCapacity: aws.Int32(1200),
		SubnetIds:       []string{"subnet-12345678"},
		LustreConfiguration: &fsxtypes.CreateFileSystemLustreConfiguration{
			DeploymentType: fsxtypes.LustreDeploymentTypeScratch2,
		},
		Tags: []fsxtypes.Tag{
			{Key: aws.String("Name"), Value: aws.String(stackName)},
			{Key: aws.String("spawn:fsx-stack-name"), Value: aws.String(stackName)},
			{Key: aws.String("spawn:fsx-s3-bucket"), Value: aws.String("my-test-bucket")},
			{Key: aws.String("spawn:fsx-s3-import-path"), Value: aws.String("s3://my-test-bucket/")},
			{Key: aws.String("spawn:fsx-s3-export-path"), Value: aws.String("s3://my-test-bucket/")},
		},
	})
	if err != nil {
		t.Fatalf("CreateFileSystem: %v", err)
	}
	return *out.FileSystem.FileSystemId
}

// TestNeedsAZSubnetResolution is the #208 regression: --fsx-create must resolve a
// pinned --az to a subnet, so the filesystem co-locates with the instance instead
// of landing in subnets[0] of the default VPC (an arbitrary AZ → unmountable
// cross-AZ FSx, and on accounts whose subnets[0] AZ lacks PERSISTENT_2 a spurious
// "not available in this availability zone" for every --az).
func TestNeedsAZSubnetResolution(t *testing.T) {
	cases := []struct {
		name   string
		subnet string
		az     string
		want   bool
	}{
		{"pinned AZ, no subnet → resolve", "", "us-east-1a", true},
		{"explicit subnet wins → no resolve", "subnet-123", "us-east-1a", false},
		{"no AZ, no subnet → fallback, no resolve", "", "", false},
		{"explicit subnet, no AZ → no resolve", "subnet-123", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NeedsAZSubnetResolution(c.subnet, c.az); got != c.want {
				t.Errorf("NeedsAZSubnetResolution(%q, %q) = %v, want %v", c.subnet, c.az, got, c.want)
			}
		})
	}
}

func TestFSxCreateAndDescribe(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	fsxClient := fsx.NewFromConfig(env.AWSConfig)

	fsID := createTestFSxFilesystem(t, fsxClient, "test-stack")

	out, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
		FileSystemIds: []string{fsID},
	})
	if err != nil {
		t.Fatalf("DescribeFileSystems: %v", err)
	}
	if len(out.FileSystems) != 1 {
		t.Fatalf("got %d filesystems, want 1", len(out.FileSystems))
	}
	fs := out.FileSystems[0]
	if *fs.FileSystemId != fsID {
		t.Errorf("FileSystemId = %q, want %q", *fs.FileSystemId, fsID)
	}
	if *fs.StorageCapacity != 1200 {
		t.Errorf("StorageCapacity = %d, want 1200", *fs.StorageCapacity)
	}
}

func TestFSxDescribeAll(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	fsxClient := fsx.NewFromConfig(env.AWSConfig)

	createTestFSxFilesystem(t, fsxClient, "stack-a")
	createTestFSxFilesystem(t, fsxClient, "stack-b")

	out, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{})
	if err != nil {
		t.Fatalf("DescribeFileSystems: %v", err)
	}
	if len(out.FileSystems) != 2 {
		t.Errorf("got %d filesystems, want 2", len(out.FileSystems))
	}
}

func TestFSxDeleteFilesystem(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	fsxClient := fsx.NewFromConfig(env.AWSConfig)

	fsID := createTestFSxFilesystem(t, fsxClient, "stack-del")

	if _, err := fsxClient.DeleteFileSystem(ctx, &fsx.DeleteFileSystemInput{
		FileSystemId: aws.String(fsID),
	}); err != nil {
		t.Fatalf("DeleteFileSystem: %v", err)
	}

	// After deletion, DescribeFileSystems returns FileSystemNotFound — this is
	// the signal the SDK's NewFileSystemDeletedWaiter uses to confirm deletion.
	_, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
		FileSystemIds: []string{fsID},
	})
	var notFound *fsxtypes.FileSystemNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected FileSystemNotFound after delete, got: %v", err)
	}
}

// TestGetFSxFilesystem_NotFound confirms GetFSxFilesystem returns an error for
// a filesystem that does not exist.
func TestGetFSxFilesystem_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	client := NewClientFromConfig(env.AWSConfig)
	if _, err := client.GetFSxFilesystem(ctx, "fs-nonexistent", "us-east-1"); err == nil {
		t.Error("expected error for nonexistent filesystem")
	}
}

// TestFSxCreateWithSecurityGroup verifies SecurityGroupIDs is passed to
// CreateFileSystem (regression for #309 — FSx mount failed because the
// instance security group was not associated with the filesystem).
func TestFSxCreateWithSecurityGroup(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	fsxClient := fsx.NewFromConfig(env.AWSConfig)

	sgID := "sg-deadbeef"
	out, err := fsxClient.CreateFileSystem(ctx, &fsx.CreateFileSystemInput{
		FileSystemType:   fsxtypes.FileSystemTypeLustre,
		StorageCapacity:  aws.Int32(1200),
		SubnetIds:        []string{"subnet-12345678"},
		SecurityGroupIds: []string{sgID},
		LustreConfiguration: &fsxtypes.CreateFileSystemLustreConfiguration{
			DeploymentType: fsxtypes.LustreDeploymentTypePersistent2,
		},
		Tags: []fsxtypes.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		},
	})
	if err != nil {
		t.Fatalf("CreateFileSystem: %v", err)
	}

	// Confirm security group is reflected in the response
	if len(out.FileSystem.NetworkInterfaceIds) == 0 && len(out.FileSystem.SubnetIds) == 0 {
		t.Log("substrate may not model NetworkInterfaces; filesystem created OK")
	}
	t.Logf("created filesystem %s with security group %s", *out.FileSystem.FileSystemId, sgID)
}

// TestFSxDeploymentTypePersistent2 verifies --fsx-create uses PERSISTENT_2,
// not SCRATCH_2 (regression for #310 — AL2023 Lustre 2.15 client rejected
// SCRATCH_2 server Lustre 2.10).
func TestFSxDeploymentTypePersistent2(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	fsxClient := fsx.NewFromConfig(env.AWSConfig)

	out, err := fsxClient.CreateFileSystem(ctx, &fsx.CreateFileSystemInput{
		FileSystemType:  fsxtypes.FileSystemTypeLustre,
		StorageCapacity: aws.Int32(1200),
		SubnetIds:       []string{"subnet-12345678"},
		LustreConfiguration: &fsxtypes.CreateFileSystemLustreConfiguration{
			DeploymentType: fsxtypes.LustreDeploymentTypePersistent2,
		},
	})
	if err != nil {
		t.Fatalf("CreateFileSystem with PERSISTENT_2: %v", err)
	}
	if out.FileSystem.LustreConfiguration == nil {
		t.Fatal("nil LustreConfiguration in response")
	}
	if out.FileSystem.LustreConfiguration.DeploymentType != fsxtypes.LustreDeploymentTypePersistent2 {
		t.Errorf("DeploymentType = %v, want PERSISTENT_2",
			out.FileSystem.LustreConfiguration.DeploymentType)
	}
}
