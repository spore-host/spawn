package pipeline

import (
	"path/filepath"
	"strings"
	"testing"
)

// ---- #989: the keys spawn pipeline collect reads -----------------------------

// ParseStageOutputKey inverts StageOutputPrefix and StageInstanceOutputPrefix,
// and reports every other key under the run, the markers and the stage config
// among them, as not an output.
func TestParseStageOutputKey(t *testing.T) {
	for _, prefix := range []string{"pfx/runs/r1", "pfx", "a/b/c/runs/20261015t000000z-0a1b2c3d"} {
		outputs := map[string][2]string{
			StageInstanceOutputPrefix(prefix, "prep", 1, 0) + "a.csv":     {"prep", "a.csv"},
			StageInstanceOutputPrefix(prefix, "prep", 1, 0) + "d/b.csv":   {"prep", "d/b.csv"},
			StageInstanceOutputPrefix(prefix, "train", 4, 3) + "best.h5":  {"train", "3/best.h5"},
			StageInstanceOutputPrefix(prefix, "train", 4, 0) + "COMPLETE": {"train", "0/COMPLETE"}, // a file the stage output, named like a marker
		}
		for key, want := range outputs {
			stage, rel, ok := ParseStageOutputKey(prefix, key)
			if !ok || stage != want[0] || rel != want[1] {
				t.Errorf("ParseStageOutputKey(%q, %q) = %q, %q, %v; want %q, %q, true", prefix, key, stage, rel, ok, want[0], want[1])
			}
		}
		for _, key := range []string{
			StageMarkerKey(prefix, "prep", MarkerComplete),
			StageMarkerKey(prefix, "prep", MarkerFailed),
			StageInstanceMarkerKey(prefix, "train", 2, MarkerComplete),
			StageInstanceMarkerKey(prefix, "train", 2, MarkerFailed),
			StageConfigKey(prefix, "ml"),
			StageOutputPrefix(prefix, "prep"), // the prefix itself
			StageOutputPrefix("other", "prep") + "a.csv",
		} {
			if stage, rel, ok := ParseStageOutputKey(prefix, key); ok {
				t.Errorf("ParseStageOutputKey(%q, %q) = %q, %q, true; it is not a stage output", prefix, key, stage, rel)
			}
		}
	}
}

func TestResultBucketIsReportedDeprecated(t *testing.T) {
	if d := (&Pipeline{ResultS3Bucket: "r"}).Deprecations(); len(d) != 1 || !strings.Contains(d[0], "result_s3_bucket") {
		t.Errorf("Deprecations() = %q, want one naming result_s3_bucket", d)
	}
	if d := (&Pipeline{ResultS3Prefix: "p"}).Deprecations(); len(d) != 1 {
		t.Errorf("Deprecations() with only result_s3_prefix = %q, want one", d)
	}
	if d := (&Pipeline{S3Bucket: "b"}).Deprecations(); len(d) != 0 {
		t.Errorf("Deprecations() = %q, want none", d)
	}
}

// No example pipeline uses a deprecated field: three set result_s3_bucket, and
// `spawn pipeline collect` found nothing on each of them (#989).
func TestExamplePipelinesUseNoDeprecatedFields(t *testing.T) {
	files, err := filepath.Glob("../../examples/pipelines/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("found no example pipelines; the glob is broken, not the examples clean")
	}
	for _, f := range files {
		p, err := LoadPipelineFromFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, d := range p.Deprecations() {
			t.Errorf("%s: %s", f, d)
		}
	}
}
