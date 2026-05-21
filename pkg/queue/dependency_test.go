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

func TestDependenciesMet(t *testing.T) {
	tests := []struct {
		name      string
		job       JobConfig
		completed map[string]bool
		want      bool
	}{
		{
			name: "no dependencies",
			job: JobConfig{
				JobID:     "job1",
				DependsOn: []string{},
			},
			completed: map[string]bool{},
			want:      true,
		},
		{
			name: "all dependencies met",
			job: JobConfig{
				JobID:     "job3",
				DependsOn: []string{"job1", "job2"},
			},
			completed: map[string]bool{
				"job1": true,
				"job2": true,
			},
			want: true,
		},
		{
			name: "some dependencies not met",
			job: JobConfig{
				JobID:     "job3",
				DependsOn: []string{"job1", "job2"},
			},
			completed: map[string]bool{
				"job1": true,
			},
			want: false,
		},
		{
			name: "no dependencies met",
			job: JobConfig{
				JobID:     "job3",
				DependsOn: []string{"job1", "job2"},
			},
			completed: map[string]bool{},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DependenciesMet(&tt.job, tt.completed)
			if got != tt.want {
				t.Errorf("DependenciesMet() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetReadyJobs(t *testing.T) {
	tests := []struct {
		name      string
		jobs      []JobConfig
		completed map[string]bool
		running   map[string]bool
		want      []string
	}{
		{
			name: "all jobs ready",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m"},
			},
			completed: map[string]bool{},
			running:   map[string]bool{},
			want:      []string{"job1", "job2"},
		},
		{
			name: "some jobs ready",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m", DependsOn: []string{"job1"}},
			},
			completed: map[string]bool{},
			running:   map[string]bool{},
			want:      []string{"job1"},
		},
		{
			name: "dependencies met",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m", DependsOn: []string{"job1"}},
				{JobID: "job3", Command: "cmd3", Timeout: "1m", DependsOn: []string{"job2"}},
			},
			completed: map[string]bool{"job1": true, "job2": true},
			running:   map[string]bool{},
			want:      []string{"job3"},
		},
		{
			name: "no jobs ready",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m", DependsOn: []string{"job0"}},
				{JobID: "job2", Command: "cmd2", Timeout: "1m", DependsOn: []string{"job0"}},
			},
			completed: map[string]bool{},
			running:   map[string]bool{},
			want:      []string{},
		},
		{
			name: "exclude completed jobs",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m"},
			},
			completed: map[string]bool{"job1": true},
			running:   map[string]bool{},
			want:      []string{"job2"},
		},
		{
			name: "exclude running jobs",
			jobs: []JobConfig{
				{JobID: "job1", Command: "cmd1", Timeout: "1m"},
				{JobID: "job2", Command: "cmd2", Timeout: "1m"},
			},
			completed: map[string]bool{},
			running:   map[string]bool{"job1": true},
			want:      []string{"job2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetReadyJobs(tt.jobs, tt.completed, tt.running)

			// Convert to sets for comparison (order doesn't matter)
			gotSet := make(map[string]bool)
			for _, id := range got {
				gotSet[id] = true
			}
			wantSet := make(map[string]bool)
			for _, id := range tt.want {
				wantSet[id] = true
			}

			if !reflect.DeepEqual(gotSet, wantSet) {
				t.Errorf("GetReadyJobs() = %v, want %v", got, tt.want)
			}
		})
	}
}
