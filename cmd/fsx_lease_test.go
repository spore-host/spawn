package cmd

import (
	"strings"
	"testing"
)

// spawn#613: `spawn terminate` said "Terminate request sent!" and nothing else,
// even when the instance was the only thing holding a 1.2 TiB ephemeral
// filesystem alive. These tests pin the notice to instance TAGS alone, so no AWS
// client is involved.

func TestFSxLeaseFromTags_MountedLease(t *testing.T) {
	lease, ok := fsxLeaseFromTags(map[string]string{
		"spawn:fsx-id":          "fs-0d0d29cb9e4be40df",
		"spawn:fsx-mount-point": "/mnt/fsx",
	})
	if !ok {
		t.Fatal("spawn:fsx-id must be recognized as a lease")
	}
	if lease.FilesystemID != "fs-0d0d29cb9e4be40df" {
		t.Errorf("FilesystemID = %q", lease.FilesystemID)
	}
	if lease.Pending {
		t.Error("a spawn:fsx-id lease is mounted, not pending")
	}
	if lease.MountPoint != "/mnt/fsx" {
		t.Errorf("MountPoint = %q", lease.MountPoint)
	}
}

func TestFSxLeaseFromTags_PendingLease(t *testing.T) {
	lease, ok := fsxLeaseFromTags(map[string]string{
		"spawn:fsx-pending": "fs-0d0d29cb9e4be40df",
	})
	if !ok {
		t.Fatal("spawn:fsx-pending must be recognized as a lease — it is the window the leak happened in")
	}
	if !lease.Pending {
		t.Error("Pending = false for a spawn:fsx-pending lease")
	}
	if lease.FilesystemID != "fs-0d0d29cb9e4be40df" {
		t.Errorf("FilesystemID = %q", lease.FilesystemID)
	}
}

func TestFSxLeaseFromTags_NeitherTagMeansNoNotice(t *testing.T) {
	for name, tags := range map[string]map[string]string{
		"no tags":          nil,
		"unrelated tags":   {"spawn:managed": "true", "spawn:ttl": "75m"},
		"empty lease tags": {"spawn:fsx-id": "", "spawn:fsx-pending": ""},
	} {
		if lease, ok := fsxLeaseFromTags(tags); ok {
			t.Errorf("%s: got a lease (%+v); an instance with no FSx must print nothing", name, lease)
		}
		if notice := renderFSxLeaseNotice(fsxLease{}, "terminate"); notice != "" {
			t.Errorf("%s: empty lease rendered a notice: %q", name, notice)
		}
	}
}

func TestRenderFSxLeaseNotice_TerminateSaysNotDeletedAndHowToConfirm(t *testing.T) {
	notice := renderFSxLeaseNotice(fsxLease{
		FilesystemID: "fs-0d0d29cb9e4be40df",
		CapacityGiB:  1200,
		Status:       "AVAILABLE",
		Lifecycle:    "ephemeral",
	}, "terminate")

	for _, want := range []string{
		"fs-0d0d29cb9e4be40df",
		"1200 GiB",
		"AVAILABLE",
		"NOT deleted by this command",
		"asynchronously",
		"$174.00/month",
		"spawn fsx list",
		"spawn fsx delete fs-0d0d29cb9e4be40df",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("terminate notice is missing %q; got:\n%s", want, notice)
		}
	}
}

func TestRenderFSxLeaseNotice_PendingLeaseIsCalledOut(t *testing.T) {
	notice := renderFSxLeaseNotice(fsxLease{
		FilesystemID: "fs-0d0d29cb9e4be40df",
		Pending:      true,
		Status:       "CREATING",
		Lifecycle:    "ephemeral",
	}, "terminate")

	if !strings.Contains(notice, "still provisioning") {
		t.Errorf("a pending (CREATING) lease must say so — that is the state spawn#613 hit; got:\n%s", notice)
	}
	if !strings.Contains(notice, "size unknown") {
		t.Errorf("unknown capacity must be stated, not printed as 0 GiB; got:\n%s", notice)
	}
	if !strings.Contains(notice, "NOT deleted by this command") {
		t.Errorf("pending lease notice lost the 'not deleted' statement; got:\n%s", notice)
	}
}

func TestRenderFSxLeaseNotice_UntaggedFilesystemIsNotClaimedAsReaped(t *testing.T) {
	// --fsx-id mounts a filesystem spawn did not create: it carries no
	// spawn:fsx-lifecycle tag and spawn will never reclaim it. Saying "reclaimed
	// automatically" here would be a second false promise.
	notice := renderFSxLeaseNotice(fsxLease{
		FilesystemID: "fs-0abc1234",
		CapacityGiB:  1200,
		Status:       "AVAILABLE",
	}, "terminate")

	if !strings.Contains(notice, "never reclaim") {
		t.Errorf("a filesystem with no lifecycle tag must be described as never reclaimed by spawn; got:\n%s", notice)
	}
}

func TestRenderFSxLeaseNotice_StopSaysItKeepsBilling(t *testing.T) {
	notice := renderFSxLeaseNotice(fsxLease{
		FilesystemID: "fs-0d0d29cb9e4be40df",
		CapacityGiB:  1200,
		Status:       "AVAILABLE",
		Lifecycle:    "ephemeral",
	}, "stop")

	if !strings.Contains(notice, "keeps running and billing") {
		t.Errorf("stop notice must say the filesystem keeps billing while the instance is stopped; got:\n%s", notice)
	}
	if strings.Contains(notice, "NOT deleted by this command") {
		t.Errorf("stop is not terminate — it should not borrow terminate's wording; got:\n%s", notice)
	}
	if !strings.Contains(notice, "spawn fsx delete fs-0d0d29cb9e4be40df") {
		t.Errorf("stop notice must still say how to delete it; got:\n%s", notice)
	}
}
