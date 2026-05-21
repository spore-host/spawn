package locality

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

func TestDetectEFSRegion_Found(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	result, err := env.EFSClient().CreateFileSystem(ctx, &efs.CreateFileSystemInput{})
	if err != nil {
		t.Fatalf("CreateFileSystem: %v", err)
	}

	efsID := *result.FileSystemId

	region, err := DetectEFSRegion(ctx, env.AWSConfig, efsID)
	if err != nil {
		t.Fatalf("DetectEFSRegion: %v", err)
	}

	if region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", region)
	}
}

func TestDetectEFSRegion_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	if _, err := DetectEFSRegion(ctx, env.AWSConfig, "fs-nonexistent"); err == nil {
		t.Error("DetectEFSRegion with nonexistent ID should return error")
	}
}

func TestDetectFSxRegion_Found(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	result, err := env.FSxClient().CreateFileSystem(ctx, &fsx.CreateFileSystemInput{
		FileSystemType:  fsxtypes.FileSystemTypeLustre,
		StorageCapacity: ptrInt32(1200),
		SubnetIds:       []string{"subnet-00000001"},
	})
	if err != nil {
		t.Fatalf("CreateFileSystem: %v", err)
	}

	fsxID := *result.FileSystem.FileSystemId

	region, err := DetectFSxRegion(ctx, env.AWSConfig, fsxID)
	if err != nil {
		t.Fatalf("DetectFSxRegion: %v", err)
	}

	if region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", region)
	}
}

func TestDetectFSxRegion_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	if _, err := DetectFSxRegion(ctx, env.AWSConfig, "fs-nonexistent"); err == nil {
		t.Error("DetectFSxRegion with nonexistent ID should return error")
	}
}

func TestCheckDataLocality_NoWarning(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	result, err := env.EFSClient().CreateFileSystem(ctx, &efs.CreateFileSystemInput{})
	if err != nil {
		t.Fatalf("CreateFileSystem: %v", err)
	}

	efsID := *result.FileSystemId

	warning, err := CheckDataLocality(ctx, env.AWSConfig, "us-east-1", efsID, "")
	if err != nil {
		t.Fatalf("CheckDataLocality: %v", err)
	}

	if warning.HasMismatches {
		t.Errorf("expected no mismatches when EFS and launch region match")
	}
}

func ptrInt32(v int32) *int32 {
	return &v
}
