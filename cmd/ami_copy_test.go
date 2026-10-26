package cmd

import (
	"testing"
	"time"
)

// --wait-timeout becomes a poll count; it is never zero, which the waiter would
// read as "use the default hour".
func TestAMICopyMaxPolls(t *testing.T) {
	for _, c := range []struct {
		timeout time.Duration
		want    int
	}{
		{time.Hour, 240},
		{time.Minute, 4},
		{time.Second, 1},
		{0, 1},
	} {
		if got := amiCopyMaxPolls(c.timeout, amiCopyPollInterval); got != c.want {
			t.Errorf("amiCopyMaxPolls(%s) = %d, want %d", c.timeout, got, c.want)
		}
	}
}

func TestAMICopyRequiresToRegion(t *testing.T) {
	f := amiCopyCmd.Flags().Lookup("to-region")
	if f == nil {
		t.Fatal("spawn ami copy has no --to-region flag")
	}
	if ann := f.Annotations["cobra_annotation_bash_completion_one_required_flag"]; len(ann) == 0 || ann[0] != "true" {
		t.Error("--to-region is not marked required")
	}
}
