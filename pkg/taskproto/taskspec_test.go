package taskproto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodSpec = `{
  "task_id": "align-42",
  "command": ["bwa", "mem", "ref.fa"],
  "resources": {"cpu": 16, "memory_gib": 64, "architecture": "x86_64", "purchase": "spot"},
  "inputs":  [{"source": "s3://b/in.fq", "destination": "/work/in.fq"}],
  "outputs": [{"source": "/work/out.bam", "destination": "s3://b/out.bam"}],
  "lifecycle": {"ttl": "4h", "on_complete": "terminate"}
}`

func TestParseSpecFile_Good(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "task.json")
	if err := os.WriteFile(p, []byte(goodSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := ParseSpecFile(p)
	if err != nil {
		t.Fatalf("ParseSpecFile: %v", err)
	}
	if spec.TaskID != "align-42" || len(spec.Command) != 3 {
		t.Errorf("unexpected parse: %+v", spec)
	}
	if spec.EffectiveOnComplete() != "terminate" {
		t.Errorf("EffectiveOnComplete = %q", spec.EffectiveOnComplete())
	}
}

func TestValidate_Failures(t *testing.T) {
	cases := []struct {
		name string
		spec TaskSpec
		want string
	}{
		{"no task_id", TaskSpec{Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}}, "task_id is required"},
		{"no command", TaskSpec{TaskID: "t", Lifecycle: Lifecycle{TTL: "1h"}}, "command is required"},
		{"no ttl", TaskSpec{TaskID: "t", Command: []string{"x"}}, "lifecycle.ttl is required"},
		{"bad ttl", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "soon"}}, "not a valid duration"},
		{"bad arch", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{Architecture: "sparc"}}, "architecture"},
		{"bad purchase", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{Purchase: "layaway"}}, "purchase"},
		{"bad on_complete", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h", OnComplete: "explode"}}, "on_complete"},
		{"headroom range", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Resources: ResourceRequest{MemoryHeadroomPercent: 250}}, "memory_headroom_percent"},
		{"bad input manifest", TaskSpec{TaskID: "t", Command: []string{"x"}, Lifecycle: Lifecycle{TTL: "1h"}, Inputs: []Manifest{{Source: "s3://b/x"}}}, "inputs[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidate_Good(t *testing.T) {
	s := TaskSpec{TaskID: "t", Command: []string{"echo", "hi"}, Lifecycle: Lifecycle{TTL: "2h"}}
	if err := s.Validate(); err != nil {
		t.Errorf("valid minimal spec rejected: %v", err)
	}
}
