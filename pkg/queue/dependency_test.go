package queue

import (
	"reflect"
	"strings"
	"testing"
)

func TestTopologicalSort(t *testing.T) {
	tests := []struct {
		name    string
		jobs    []JobConfig
		want    []string
		wantErr bool
		errMsg  string
	}{
		{
			name: "simple linear dependency",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m", DependsOn: []string{"job1"}},
				{JobID: "job3", Command: "cmd3", Timeout: "1m", DependsOn: []string{"job2"}},
			},
			want:    []string{"job1", "job2", "job3"},
			wantErr: false,
		},
		{
			name: "parallel jobs",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m"},
				{JobID: "job3", Command: "cmd3", Timeout: "1m"},
			},
			want:    nil, // Order doesn't matter for parallel jobs
			wantErr: false,
		},
		{
			name: "diamond dependency",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m", DependsOn: []string{"job1"}},
				{JobID: "job3", Command: "cmd3", Timeout: "1m", DependsOn: []string{"job1"}},
				{JobID: "job4", Command: "cmd4", Timeout: "1m", DependsOn: []string{"job2", "job3"}},
			},
			want:    nil, // job1 first, job4 last, job2 and job3 in between
			wantErr: false,
		},
		{
			name: "circular dependency simple",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m", DependsOn: []string{"job2"}},
				{JobID: "job2", Command: "cmd2", Timeout: "1m", DependsOn: []string{"job1"}},
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
		{
			name: "circular dependency complex",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m", DependsOn: []string{"job1"}},
				{JobID: "job3", Command: "cmd3", Timeout: "1m", DependsOn: []string{"job2"}},
				{JobID: "job4", Command: "cmd4", Timeout: "1m", DependsOn: []string{"job3"}},
				{JobID: "job5", Command: "cmd5", Timeout: "1m", DependsOn: []string{"job4", "job2"}},
				{JobID: "job6", Command: "cmd6", Timeout: "1m", DependsOn: []string{"job5"}},
				// Create cycle: job2 depends on job6
				{JobID: "job7", Command: "cmd7", Timeout: "1m", DependsOn: []string{"job6", "job1"}},
			},
			wantErr: false, // No cycle in this configuration
		},
		{
			name: "self dependency",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m", DependsOn: []string{"job1"}},
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
		{
			name: "multiple root jobs",
			jobs: []JobConfig{
				{JobID: "root1", Command: "cmd1", Timeout: "1m"},
				{JobID: "root2", Command: "cmd2", Timeout: "1m"},
				{JobID: "child1", Command: "cmd3", Timeout: "1m", DependsOn: []string{"root1"}},
				{JobID: "child2", Command: "cmd4", Timeout: "1m", DependsOn: []string{"root2"}},
				{JobID: "merge", Command: "cmd5", Timeout: "1m", DependsOn: []string{"child1", "child2"}},
			},
			want:    nil, // Root jobs first, merge last
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TopologicalSort(tt.jobs)
			if (err != nil) != tt.wantErr {
				t.Errorf("TopologicalSort() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr && err != nil && tt.errMsg != "" {
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errMsg)) {
					t.Errorf("TopologicalSort() error message = %v, should contain %q", err.Error(), tt.errMsg)
				}
			}

			// For successful cases with expected order
			if !tt.wantErr && tt.want != nil {
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("TopologicalSort() = %v, want %v", got, tt.want)
				}
			}

			// For successful cases, verify dependencies are respected
			if !tt.wantErr && got != nil {
				positions := make(map[string]int)
				for i, jobID := range got {
					positions[jobID] = i
				}

				// Check that all dependencies come before their dependents
				for _, job := range tt.jobs {
					jobPos := positions[job.JobID]
					for _, dep := range job.DependsOn {
						depPos, exists := positions[dep]
						if !exists {
							t.Errorf("Dependency %s not found in result", dep)
							continue
						}
						if depPos >= jobPos {
							t.Errorf("Dependency %s (pos %d) should come before %s (pos %d)", dep, depPos, job.JobID, jobPos)
						}
					}
				}
			}
		})
	}
}
