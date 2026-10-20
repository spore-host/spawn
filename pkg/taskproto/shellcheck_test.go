package taskproto

import (
	"testing"

	"github.com/spore-host/spawn/internal/shellcheckgate"
)

// shellcheck on the task wrapper, the pooled job script and the terminal-flush
// hook (#689 part C). The specs switch on every optional block (inputs,
// outputs, env, container, GPU) with values carrying spaces and quotes.
func TestTaskScripts_Shellcheck(t *testing.T) {
	opts := WrapperOptions{ResultsPrefix: "s3://spawn-results-123456789012-us-east-1/tasks/t-1", RunID: "run-1", Region: "us-east-1"}
	specs := map[string]*TaskSpec{
		"host": {
			TaskID:    "t-1",
			Command:   []string{"bash", "-c", `echo "it's a test" > "out file.txt"`},
			Inputs:    []Manifest{{Source: "s3://in/data dir/", Destination: "/work/in put/"}},
			Outputs:   []Manifest{{Source: "/work/out file.txt", Destination: "s3://out/results/"}},
			Env:       map[string]string{"GREETING": "hello 'world'", "N": "3"},
			Lifecycle: Lifecycle{TTL: "1h", OnComplete: "terminate"},
		},
		"container": {
			TaskID:    "t-2",
			Command:   []string{"python3", "-c", "print('hi')"},
			Container: "111111111111.dkr.ecr.us-east-1.amazonaws.com/tool:1",
			Inputs:    []Manifest{{Source: "s3://in/a.txt", Destination: "/data/a.txt"}},
			Outputs:   []Manifest{{Source: "/data/out/", Destination: "s3://out/run/"}},
			Env:       map[string]string{"X": "y z"},
			Resources: ResourceRequest{GPUs: 1},
			Lifecycle: Lifecycle{TTL: "1h", OnComplete: "terminate"},
		},
	}
	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			o := opts
			o.GPU = spec.Resources.GPUs > 0
			w, err := GenerateWrapper(spec, o)
			if err != nil {
				t.Fatal(err)
			}
			shellcheckgate.Check(t, "pkg/taskproto.generateWrapper", w)
			pooled, err := GeneratePooledJobScript(spec, o)
			if err != nil {
				t.Fatal(err)
			}
			shellcheckgate.Check(t, "pkg/taskproto.generateWrapper", pooled)
			flush, err := GenerateFlushScript(spec, o)
			if err != nil {
				t.Fatal(err)
			}
			shellcheckgate.Check(t, "pkg/taskproto.GenerateFlushScript", flush)
		})
	}
}
