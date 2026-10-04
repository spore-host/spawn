package cmd

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// decodeUserDataOnce is what cloud-init does: base64-decode, then gunzip ONE
// layer. A correctly built user-data yields the script text; a double-gzipped one
// yields more gzip, which cloud-init reports as
// "Unhandled non-multipart (text/x-not-multipart) userdata".
func decodeUserDataOnce(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("user-data is not valid base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("user-data is not gzip: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip failed: %v", err)
	}
	return out
}

// TestJobArrayMemberUserDataIsSingleGzipped is spawn#671.
//
// buildJobArrayMemberConfig base64-decoded baseConfig.UserData — which
// encodeUserData had already GZIPPED — appended the MPI script to those
// compressed bytes, and encoded again. What shipped was
// gzip(gzip(bootstrap) + "\n" + mpiScript).
//
// cloud-init unwraps one gzip layer, finds binary, and skips the whole thing:
//
//	__init__.py[WARNING]: Unhandled non-multipart (text/x-not-multipart)
//	userdata: 'b'\x1f\x8b\x08\x00...
//
// So NO job-array or MPI instance ever ran its bootstrap: no spored, no peers
// file, no hostfile. And with spored absent, nothing on-instance enforces
// spawn:ttl either — the reporter's two c8g.48xlarge ($7.66/hr each) sat running
// until terminated by hand.
func TestJobArrayMemberUserDataIsSingleGzipped(t *testing.T) {
	const script = "#!/bin/bash\nset -e\necho bootstrap-marker\n"
	base := &aws.LaunchConfig{
		Region:   "us-east-1",
		UserData: encodeUserData(script),
	}

	got, err := buildJobArrayMemberConfig(base, memberParams{
		name: "cohort", size: 2, instanceNames: "w-{index}", mpi: true,
	}, "ja-123", 0, nil)
	if err != nil {
		t.Fatalf("buildJobArrayMemberConfig: %v", err)
	}

	decoded := decodeUserDataOnce(t, got.UserData)

	// The decisive assertion: after ONE gunzip, cloud-init must see a script.
	if !bytes.HasPrefix(decoded, []byte("#!")) {
		preview := decoded
		if len(preview) > 16 {
			preview = preview[:16]
		}
		t.Fatalf("after one gunzip the user-data must start with #! — cloud-init skips "+
			"anything else and the instance never bootstraps.\ngot first bytes: %q", preview)
	}
	// It must still contain a second gzip header nowhere in it.
	if bytes.Contains(decoded, []byte{0x1f, 0x8b, 0x08}) {
		t.Error("the decoded user-data still contains a gzip header — it was encoded twice")
	}

	s := string(decoded)
	if !strings.Contains(s, "echo bootstrap-marker") {
		t.Error("the base bootstrap is missing from the combined user-data")
	}
	if !strings.Contains(s, "MPI") && !strings.Contains(s, "mpi") {
		t.Error("the MPI block is missing from the combined user-data")
	}
}

// TestJobArrayMemberWithoutMPIIsUntouched: the non-MPI, non-storage path must not
// re-encode at all, so a member config keeps the base encoding byte-for-byte.
func TestJobArrayMemberWithoutMPIIsUntouched(t *testing.T) {
	encoded := encodeUserData("#!/bin/bash\necho hi\n")
	base := &aws.LaunchConfig{Region: "us-east-1", UserData: encoded}

	got, err := buildJobArrayMemberConfig(base, memberParams{
		name: "cohort", size: 2, instanceNames: "w-{index}",
	}, "ja-123", 1, nil)
	if err != nil {
		t.Fatalf("buildJobArrayMemberConfig: %v", err)
	}
	if got.UserData != encoded {
		t.Error("a member with no MPI/storage must carry the base user-data unchanged")
	}
	// And it must still be decodable in one step.
	if !bytes.HasPrefix(decodeUserDataOnce(t, got.UserData), []byte("#!")) {
		t.Error("base user-data is not single-gzipped")
	}
}

// TestCohortFailureAlwaysDrains is the cost-safety half of spawn#671.
//
// The drain used to require ReachedPhase == PhaseCohortAssembly — phase 5. Phase
// is ORDERED, and an instance exists and bills from PhaseLaunchAcked onward, so a
// cohort dying at `running` (2) or `enrolled` (3) had launched instances and left
// every one running. The reporter's two c8g.48xlarge ($7.66/hr each) went terminal
// at phase=enrolled and stayed up until terminated by hand; the placement group
// then could not be deleted because they were still in it.
//
// A phase predicate cannot be made safe here: PhaseLaunchAcked is iota 0, so a
// member that never launched is indistinguishable from one that did by
// ReachedPhase alone. Asserted against the source because the failure path needs
// a live cohort reconciler and an AWS client to exercise directly.
func TestCohortFailureAlwaysDrains(t *testing.T) {
	src, err := os.ReadFile("launch_jobarray.go")
	if err != nil {
		t.Fatalf("read launch_jobarray.go: %v", err)
	}
	s := string(src)

	// The gated form must be gone.
	if strings.Contains(s, "assemblyReached") {
		t.Error("the drain is still gated on reaching cohort-assembly. A cohort that dies at " +
			"`running` or `enrolled` has launched instances, and gating on phase 5 leaves them " +
			"running (spawn#671).")
	}
	if strings.Contains(s, "ReachedPhase == cohort.PhaseCohortAssembly") {
		t.Error("ReachedPhase == PhaseCohortAssembly is an equality on one phase; instances " +
			"bill from PhaseLaunchAcked onward")
	}

	// And the drain must be reachable on the failure path, not behind a condition.
	drainIdx := strings.Index(s, "drainJobArray(ctx, awsClient, baseConfig.Region, jobArrayID)")
	if drainIdx < 0 {
		t.Fatal("the failure path no longer drains at all")
	}
	// Walk back to the nearest control-flow keyword; it must not be an `if`
	// guarding the drain on a phase.
	before := s[:drainIdx]
	lastIf := strings.LastIndex(before, "\n\t\tif ")
	lastFmt := strings.LastIndex(before, "\n\t\tfmt.Fprintf")
	if lastIf > lastFmt {
		t.Errorf("the drain still sits behind a conditional at offset %d — on cohort failure it "+
			"must run unconditionally, because drainJobArray filters by the job-array-id tag "+
			"and is a no-op when nothing launched", lastIf)
	}
}
