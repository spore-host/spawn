package arrayrec

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

func sampleRecord() Record {
	return Record{
		ArrayID:       "data-proc-20260719-abc123",
		Name:          "data-proc",
		Size:          8,
		Region:        "us-east-1",
		Command:       "run.sh",
		InstanceNames: "worker-{index}",
		CreatedAt:     time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Base: aws.LaunchConfig{
			InstanceType:     "c7i.large",
			SubnetID:         "subnet-abc",
			SecurityGroupIDs: []string{"sg-1", "sg-2"},
			UserData:         "IyEvYmluL2Jhc2gK", // base64, faithfully round-tripped
			TTL:              "4h",
			Region:           "us-east-1",
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sampleRecord()
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	byID, err := LoadByID(dir, want.ArrayID)
	if err != nil {
		t.Fatalf("LoadByID: %v", err)
	}
	assertRecordEqual(t, byID, want)

	byName, err := LoadByName(dir, want.Name)
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	assertRecordEqual(t, byName, want)
}

func TestLoadByNamePointsAtLatest(t *testing.T) {
	dir := t.TempDir()
	first := sampleRecord()
	if err := Save(dir, first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	// Relaunching the same name writes a new id and should move the pointer.
	second := sampleRecord()
	second.ArrayID = "data-proc-20260719-def456"
	second.Base.InstanceType = "c7i.xlarge"
	if err := Save(dir, second); err != nil {
		t.Fatalf("Save second: %v", err)
	}

	got, err := LoadByName(dir, "data-proc")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if got.ArrayID != second.ArrayID {
		t.Errorf("LoadByName resolved to %q, want latest %q", got.ArrayID, second.ArrayID)
	}
	if got.Base.InstanceType != "c7i.xlarge" {
		t.Errorf("latest record not returned: InstanceType = %q", got.Base.InstanceType)
	}
	// The first id record is still readable directly.
	if _, err := LoadByID(dir, first.ArrayID); err != nil {
		t.Errorf("LoadByID(first) after overwrite: %v", err)
	}
}

func TestLoadByNameMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadByName(dir, "never-launched")
	if err == nil {
		t.Fatal("expected error for a name with no record")
	}
}

func TestSaveRequiresIDAndName(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Record{Name: "x"}); err == nil {
		t.Error("expected error when ArrayID is empty")
	}
	if err := Save(dir, Record{ArrayID: "x"}); err == nil {
		t.Error("expected error when Name is empty")
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	r := sampleRecord()
	if err := Save(dir, r); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(idPath(dir, r.ArrayID))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("record perms = %o, want 0600", fi.Mode().Perm())
	}
}

func TestSafeFileContainsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	r := sampleRecord()
	r.Name = "../../evil/name" // path traversal attempt
	if err := Save(dir, r); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Nothing should be written outside dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("unexpected subdirectory created: %s", e.Name())
		}
	}
	// And it still resolves back.
	got, err := LoadByName(dir, "../../evil/name")
	if err != nil {
		t.Fatalf("LoadByName after safeFile: %v", err)
	}
	if got.ArrayID != r.ArrayID {
		t.Errorf("round trip failed: got %q", got.ArrayID)
	}
	// Confirm the parent of dir got nothing.
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "evil")); err == nil {
		t.Error("path traversal escaped the record dir")
	}
}

func assertRecordEqual(t *testing.T, got, want Record) {
	t.Helper()
	if got.ArrayID != want.ArrayID || got.Name != want.Name || got.Size != want.Size ||
		got.Region != want.Region || got.Command != want.Command || got.InstanceNames != want.InstanceNames {
		t.Errorf("scalar mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.Base.InstanceType != want.Base.InstanceType || got.Base.SubnetID != want.Base.SubnetID ||
		got.Base.UserData != want.Base.UserData || got.Base.TTL != want.Base.TTL {
		t.Errorf("Base mismatch:\n got=%+v\nwant=%+v", got.Base, want.Base)
	}
	if len(got.Base.SecurityGroupIDs) != len(want.Base.SecurityGroupIDs) {
		t.Errorf("SecurityGroupIDs = %v, want %v", got.Base.SecurityGroupIDs, want.Base.SecurityGroupIDs)
	}
}
