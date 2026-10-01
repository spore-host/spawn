package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
)

// TestFSxInfoFromCreatingFilesystem is the #618 regression guard.
//
// A filesystem still CREATING has no DNSName and no LustreConfiguration at all.
// The mapping used to dereference DNSName, LustreConfiguration.MountName and
// StorageCapacity unconditionally, so reaching it during provisioning panicked
// rather than returning something usable — and that is precisely the window
// spawn#613 was reported from. Against the pre-fix code this test panics.
func TestFSxInfoFromCreatingFilesystem(t *testing.T) {
	// Exactly the shape FSx returns while provisioning.
	fs := fsxtypes.FileSystem{
		FileSystemId:    aws.String("fs-0d0d29cb9e4be40df"),
		Lifecycle:       fsxtypes.FileSystemLifecycleCreating,
		DNSName:         nil, // not assigned until AVAILABLE
		StorageCapacity: nil, // optional in the API
		// LustreConfiguration is absent entirely while CREATING.
	}

	info := fsxInfoFrom(fs) // must not panic

	if info == nil {
		t.Fatal("expected an FSxInfo, got nil")
	}
	if info.FileSystemID != "fs-0d0d29cb9e4be40df" {
		t.Errorf("FileSystemID = %q, want fs-0d0d29cb9e4be40df", info.FileSystemID)
	}
	// Zero values are the meaningful answer here: a mount cannot be built without
	// a DNS name and mount name, so empty says "not ready" rather than lying.
	if info.DNSName != "" {
		t.Errorf("DNSName = %q, want empty for a CREATING filesystem", info.DNSName)
	}
	if info.MountName != "" {
		t.Errorf("MountName = %q, want empty for a CREATING filesystem", info.MountName)
	}
	if info.StorageCapacity != 0 {
		t.Errorf("StorageCapacity = %d, want 0 when unreported", info.StorageCapacity)
	}
}

// TestFSxInfoFromToleratesNilTagFields covers the same unconditional-deref shape in
// the tag scan: Tag.Key and Tag.Value are both optional, so a tag missing either
// used to panic.
func TestFSxInfoFromToleratesNilTagFields(t *testing.T) {
	fs := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-1"),
		Tags: []fsxtypes.Tag{
			{Key: nil, Value: aws.String("orphan value")},
			{Key: aws.String("spawn:fsx-s3-bucket"), Value: nil},
			{Key: aws.String("spawn:fsx-s3-import-path"), Value: aws.String("s3://b/in")},
		},
	}

	info := fsxInfoFrom(fs) // must not panic

	if info.S3Bucket != "" {
		t.Errorf("S3Bucket = %q, want empty when the tag value is nil", info.S3Bucket)
	}
	if info.S3ImportPath != "s3://b/in" {
		t.Errorf("S3ImportPath = %q, want s3://b/in", info.S3ImportPath)
	}
}

// TestFSxInfoFromAvailableFilesystem pins the fully-populated path, so making the
// CREATING case safe cannot have quietly broken the normal one.
func TestFSxInfoFromAvailableFilesystem(t *testing.T) {
	fs := fsxtypes.FileSystem{
		FileSystemId:    aws.String("fs-abc"),
		Lifecycle:       fsxtypes.FileSystemLifecycleAvailable,
		DNSName:         aws.String("fs-abc.fsx.us-west-2.amazonaws.com"),
		StorageCapacity: aws.Int32(1200),
		LustreConfiguration: &fsxtypes.LustreFileSystemConfiguration{
			MountName: aws.String("mszmnb4v"),
		},
		Tags: []fsxtypes.Tag{
			{Key: aws.String("spawn:fsx-s3-bucket"), Value: aws.String("my-bucket")},
			{Key: aws.String("spawn:fsx-s3-import-path"), Value: aws.String("s3://my-bucket/in")},
			{Key: aws.String("spawn:fsx-s3-export-path"), Value: aws.String("s3://my-bucket/out")},
		},
	}

	info := fsxInfoFrom(fs)

	for _, c := range []struct{ name, got, want string }{
		{"FileSystemID", info.FileSystemID, "fs-abc"},
		{"DNSName", info.DNSName, "fs-abc.fsx.us-west-2.amazonaws.com"},
		{"MountName", info.MountName, "mszmnb4v"},
		{"S3Bucket", info.S3Bucket, "my-bucket"},
		{"S3ImportPath", info.S3ImportPath, "s3://my-bucket/in"},
		{"S3ExportPath", info.S3ExportPath, "s3://my-bucket/out"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if info.StorageCapacity != 1200 {
		t.Errorf("StorageCapacity = %d, want 1200", info.StorageCapacity)
	}
}
