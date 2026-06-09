package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestUploadISOToS3_RejectsNonUppercaseISO verifies the uppercase-.ISO guard
// fires before any S3 call (import-disk-image requires the key end in .ISO).
func TestUploadISOToS3_RejectsNonUppercaseISO(t *testing.T) {
	c := &Client{}
	for _, key := range []string{"win11.iso", "win11.Iso", "win11", "dir/win11.iso"} {
		err := c.UploadISOToS3(context.Background(), "us-east-1", "bucket", key, "/nonexistent")
		if err == nil {
			t.Errorf("key %q: expected uppercase-.ISO rejection, got nil", key)
			continue
		}
		if !strings.Contains(err.Error(), ".ISO") {
			t.Errorf("key %q: expected error about .ISO, got %v", key, err)
		}
	}
}

// TestUploadISOToS3_OpensFileForValidKey confirms a valid .ISO key passes the
// extension guard and proceeds to open the file (which fails for a missing path,
// proving we got past the guard).
func TestUploadISOToS3_OpensFileForValidKey(t *testing.T) {
	c := &Client{}
	err := c.UploadISOToS3(context.Background(), "us-east-1", "bucket", "win11.ISO", "/definitely/not/here.iso")
	if err == nil {
		t.Fatal("expected open error for missing file")
	}
	if !strings.Contains(err.Error(), "open ISO") {
		t.Errorf("valid .ISO key must pass the guard and hit the file open; got %v", err)
	}
}

// TestEnsureExists treats EntityAlreadyExists as success and propagates others.
func TestEnsureExists(t *testing.T) {
	if err := ensureExists("ok", nil); err != nil {
		t.Errorf("nil error must pass: %v", err)
	}
	if err := ensureExists("x", &fakeAPIErr{code: "EntityAlreadyExists"}); err != nil {
		t.Errorf("EntityAlreadyExists must be treated as success, got %v", err)
	}
	if err := ensureExists("x", &fakeAPIErr{code: "AccessDenied"}); err == nil {
		t.Error("non-already-exists error must propagate")
	}
}

// TestImportWindowsISO_Substrate exercises the import call path against a
// substrate-backed client. Substrate may not model Image Builder, so we only
// assert the method runs without panicking and returns (value or error).
func TestImportWindowsISO_Substrate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
	_, err := client.ImportWindowsISO(context.Background(), ImportWindowsISOInput{
		Name:                           "win11-test",
		SemanticVersion:                "1.0.0",
		URI:                            "s3://bucket/win11.ISO",
		InfrastructureConfigurationArn: "arn:aws:imagebuilder:us-east-1:000000000000:infrastructure-configuration/x",
	})
	t.Logf("ImportWindowsISO substrate result: %v", err)
}

// fakeAPIErr is a minimal smithy.APIError for testing ensureExists.
type fakeAPIErr struct{ code string }

func (e *fakeAPIErr) Error() string                 { return e.code }
func (e *fakeAPIErr) ErrorCode() string             { return e.code }
func (e *fakeAPIErr) ErrorMessage() string          { return e.code }
func (e *fakeAPIErr) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }
