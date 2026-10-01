package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func instWithTags(tags map[string]string) *aws.InstanceInfo {
	return &aws.InstanceInfo{Tags: tags}
}

// TestFSxDRANoticeNamesTheRealConsequence is the visibility half of spawn#622.
//
// The association failure lived only in /var/log/spored.log on the box. It is
// non-fatal by design — spored mounts anyway so the job can run — but with
// --fsx-import-path the DRA is how the data ARRIVES, so the workload read an empty
// 1200 GiB filesystem while the launch printed success. The notice has to say the
// mount is empty, not merely that something "may not sync".
func TestFSxDRANoticeNamesTheRealConsequence(t *testing.T) {
	notice := fsxDRAStatusNotice(instWithTags(map[string]string{
		"spawn:fsx-dra-status": "failed",
		"spawn:fsx-dra-error":  "BadRequest: Amazon FSx is unable to create Service-Linked-Role",
		"spawn:fsx-id":         "fs-0355f3a3160d8618c",
	}))

	if notice == "" {
		t.Fatal("a failed association must produce a notice")
	}
	for _, want := range []string{
		"fs-0355f3a3160d8618c", // which filesystem
		"Service-Linked-Role",  // the actual error, not a paraphrase
		"EMPTY",                // the import consequence, stated plainly
		"NOT be copied",        // the export consequence
		"billing",              // it costs money regardless
		"spawn fsx delete",     // how to stop paying for it
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice must mention %q:\n%s", want, notice)
		}
	}
}

// TestFSxDRANoticeSilentWhenFine — a notice that fires on success trains users to
// ignore it.
func TestFSxDRANoticeSilentWhenFine(t *testing.T) {
	cases := map[string]map[string]string{
		"associated":       {"spawn:fsx-dra-status": "associated"},
		"no tag at all":    {},
		"empty status":     {"spawn:fsx-dra-status": ""},
		"unrelated tags":   {"spawn:managed": "true", "spawn:fsx-id": "fs-1"},
		"stale error only": {"spawn:fsx-dra-status": "associated", "spawn:fsx-dra-error": "old failure"},
	}
	for name, tags := range cases {
		t.Run(name, func(t *testing.T) {
			if got := fsxDRAStatusNotice(instWithTags(tags)); got != "" {
				t.Errorf("expected no notice, got:\n%s", got)
			}
		})
	}
}

// TestFSxDRANoticeWithoutDetail still reports, rather than going quiet because the
// error tag is missing — silence is the defect being fixed.
func TestFSxDRANoticeWithoutDetail(t *testing.T) {
	notice := fsxDRAStatusNotice(instWithTags(map[string]string{"spawn:fsx-dra-status": "failed"}))
	if notice == "" {
		t.Fatal("a failed association with no detail must still be reported")
	}
	if !strings.Contains(notice, "no detail reported") {
		t.Errorf("expected a placeholder for the missing detail:\n%s", notice)
	}
}

// TestFSxDRANoticeFallsBackToPendingID: the failure can happen while the filesystem
// is still only recorded as pending, so the notice should still name it.
func TestFSxDRANoticeFallsBackToPendingID(t *testing.T) {
	notice := fsxDRAStatusNotice(instWithTags(map[string]string{
		"spawn:fsx-dra-status": "failed",
		"spawn:fsx-pending":    "fs-pending123",
	}))
	if !strings.Contains(notice, "fs-pending123") {
		t.Errorf("notice should name the pending filesystem id:\n%s", notice)
	}
}
