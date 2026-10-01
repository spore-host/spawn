package agent

import (
	"strings"
	"testing"
)

// TestDRAImpactWording is spawn#622's sharpest point: the old log line said
// "results may not auto-export to S3", which is a fair description of the EXPORT
// failure and a badly misleading one for the IMPORT failure. With
// --fsx-import-path the association is how the data arrives, so what actually
// happened was that the workload read an empty 1200 GiB filesystem.
func TestDRAImpactWording(t *testing.T) {
	t.Run("import only says the filesystem will be empty", func(t *testing.T) {
		got := draImpact("s3://b/inputs/idx", "")
		if !strings.Contains(got, "EMPTY") {
			t.Errorf("import failure must say the filesystem will be EMPTY, got: %s", got)
		}
		if !strings.Contains(got, "s3://b/inputs/idx") {
			t.Errorf("should name the import path, got: %s", got)
		}
		if strings.Contains(got, "export") {
			t.Errorf("no export path was given, so don't mention export: %s", got)
		}
	})

	t.Run("export only talks about results", func(t *testing.T) {
		got := draImpact("", "s3://b/out")
		if strings.Contains(got, "EMPTY") {
			t.Errorf("no import path, so the filesystem is not expected to be empty: %s", got)
		}
		if !strings.Contains(got, "s3://b/out") {
			t.Errorf("should name the export path, got: %s", got)
		}
	})

	t.Run("both reports both", func(t *testing.T) {
		got := draImpact("s3://b/in", "s3://b/out")
		for _, want := range []string{"EMPTY", "s3://b/in", "s3://b/out"} {
			if !strings.Contains(got, want) {
				t.Errorf("expected %q in: %s", want, got)
			}
		}
	})
}

func TestTruncateTagValue(t *testing.T) {
	// EC2 caps tag values at 256 chars; a rejected CreateTags would lose the whole
	// signal, which is the silence #622 is about.
	long := strings.Repeat("x", 400)
	got := truncateTagValue(long, 255)
	if len(got) != 255 {
		t.Errorf("len = %d, want 255", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Error("a truncated value must be marked, so it isn't mistaken for the whole error")
	}
	if short := truncateTagValue("abc", 255); short != "abc" {
		t.Errorf("a short value must pass through unchanged, got %q", short)
	}
}
