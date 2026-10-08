package taskproto

import (
	"strings"
	"testing"
)

// TestEffectiveResultsPrefixDefaultIsUnchanged is the compatibility assertion:
// six workflow adapters poll this exact location, so a spec that says nothing
// must land where it always did (spawn#646).
func TestEffectiveResultsPrefixDefaultIsUnchanged(t *testing.T) {
	got := EffectiveResultsPrefix(&TaskSpec{TaskID: "align-42"}, "123456789012", "us-west-2")
	want := "s3://spawn-results-123456789012-us-west-2/tasks/align-42"
	if got != want {
		t.Errorf("default prefix = %q, want %q — adapters poll this path literally", got, want)
	}
}

func TestEffectiveResultsPrefixHonoursTheSpec(t *testing.T) {
	spec := &TaskSpec{TaskID: "align-42", ResultsPrefix: "s3://my-bucket/sessions/abc"}
	got := EffectiveResultsPrefix(spec, "123456789012", "us-west-2")
	if want := "s3://my-bucket/sessions/abc/align-42"; got != want {
		t.Errorf("prefix = %q, want %q", got, want)
	}
}

// TestEffectiveResultsPrefixTolerATrailingSlash: a caller writing
// "s3://b/runs/" should not get "s3://b/runs//id".
func TestEffectiveResultsPrefixToleratesATrailingSlash(t *testing.T) {
	spec := &TaskSpec{TaskID: "t1", ResultsPrefix: "s3://b/runs/"}
	if got := EffectiveResultsPrefix(spec, "a", "r"); got != "s3://b/runs/t1" {
		t.Errorf("prefix = %q, want s3://b/runs/t1", got)
	}
}

func TestSplitS3URI(t *testing.T) {
	cases := []struct {
		in          string
		bucket, key string
		ok          bool
	}{
		{"s3://bucket/a/b/c", "bucket", "a/b/c", true},
		{"s3://bucket", "bucket", "", true},
		{"s3://bucket/", "bucket", "", true},
		{"s3://bucket/a/", "bucket", "a", true},
		// Rejections. A caller who typos the scheme must get an error rather than a
		// bucket named "my-bucket/path".
		{"my-bucket/path", "", "", false},
		{"https://bucket/x", "", "", false},
		{"s3://", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		b, k, ok := SplitS3URI(tc.in)
		if ok != tc.ok || b != tc.bucket || k != tc.key {
			t.Errorf("SplitS3URI(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, b, k, ok, tc.bucket, tc.key, tc.ok)
		}
	}
}

// TestIsDefaultResultsBucket guards a safety decision, not a convenience.
//
// spawn creates its OWN results bucket before launch so a first-ever task can't
// hit NoSuchBucket. It must not create a bucket a caller named: a typo would
// leave a stray bucket behind and still report the run as a success, which is the
// opposite of what someone pointing at their own storage expects. So the create
// is gated on this, and a custom bucket that doesn't exist must fail loudly.
func TestIsDefaultResultsBucket(t *testing.T) {
	const acct, region = "123456789012", "us-west-2"
	if !IsDefaultResultsBucket("spawn-results-123456789012-us-west-2", acct, region) {
		t.Error("spawn's own bucket must be recognised, or it stops being auto-created")
	}
	for _, other := range []string{
		"my-bucket",
		"spawn-results-123456789012-us-east-1", // right account, WRONG region
		"spawn-results-999999999999-us-west-2", // right region, WRONG account
		"spawn-results",
		"",
	} {
		if IsDefaultResultsBucket(other, acct, region) {
			t.Errorf("%q must NOT be treated as spawn's own bucket — spawn would create a "+
				"bucket the caller named, so a typo leaves a stray bucket and reports success",
				other)
		}
	}
}

// TestGeneratorsUseTheSuppliedPrefix: the whole point of #646 is that the prefix
// is resolved ONCE by the caller and reaches the wrapper and the flush hook
// unchanged, rather than each deriving it from an account and region.
func TestGeneratorsUseTheSuppliedPrefix(t *testing.T) {
	spec := &TaskSpec{TaskID: "t1", Command: []string{"true"}}
	const prefix = "s3://my-bucket/sessions/abc/t1"

	opts := WrapperOptions{ResultsPrefix: prefix, Region: "us-east-1", RunID: "r"}
	w := mustScript(GenerateWrapper(spec, opts))
	if !strings.Contains(w, "RESULTS_PREFIX='"+prefix+"'") {
		t.Errorf("wrapper does not use the supplied prefix:\n%s", w)
	}
	f := mustScript(GenerateFlushScript(spec, opts))
	if !strings.Contains(f, "RESULTS_PREFIX='"+prefix+"'") {
		t.Errorf("flush hook does not use the supplied prefix:\n%s", f)
	}
	// And neither should reconstruct the old hardcoded shape.
	for name, script := range map[string]string{"wrapper": w, "flush": f} {
		if strings.Contains(script, "spawn-results-") {
			t.Errorf("%s still references the default bucket name despite a custom prefix", name)
		}
	}
}
