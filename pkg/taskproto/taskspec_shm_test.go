package taskproto

import (
	"math"
	"strings"
	"testing"
)

// #1111: resources.shm_gib must be a positive number, needs a container, and
// may not exceed the instance's memory (the requested memory_gib with headroom,
// or the container floor when that is larger).
func TestValidate_ShmGiB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*TaskSpec)
		wantErr string // "" = valid
	}{
		{"unset", func(s *TaskSpec) { s.Resources.ShmGiB = nil }, ""},
		{"within memory_gib", func(s *TaskSpec) { s.Resources.ShmGiB = shmPtr(64) }, ""},
		{"equal to memory_gib", func(s *TaskSpec) { s.Resources.ShmGiB = shmPtr(128) }, ""},
		{"zero", func(s *TaskSpec) { s.Resources.ShmGiB = shmPtr(0) }, "must be a positive number"},
		{"negative", func(s *TaskSpec) { s.Resources.ShmGiB = shmPtr(-1) }, "must be a positive number"},
		{"NaN", func(s *TaskSpec) { s.Resources.ShmGiB = shmPtr(math.NaN()) }, "must be a positive number"},
		{"over memory_gib", func(s *TaskSpec) { s.Resources.ShmGiB = shmPtr(129) }, "exceeds the instance's 128 GiB"},
		{"within headroom", func(s *TaskSpec) {
			s.Resources.MemoryGiB, s.Resources.MemoryHeadroomPercent, s.Resources.ShmGiB = 4, 50, shmPtr(6)
		}, ""},
		{"no memory_gib: floor bounds it", func(s *TaskSpec) {
			s.Resources.MemoryGiB, s.Resources.ShmGiB = 0, shmPtr(ContainerMemoryFloorGiB)
		}, ""},
		{"no memory_gib: over the floor", func(s *TaskSpec) {
			s.Resources.MemoryGiB, s.Resources.ShmGiB = 0, shmPtr(ContainerMemoryFloorGiB+1)
		}, "exceeds the instance's 2 GiB"},
		{"memory_gib below floor: floor bounds it", func(s *TaskSpec) {
			s.Resources.MemoryGiB, s.Resources.ShmGiB = 1, shmPtr(1.5)
		}, ""},
		{"host task", func(s *TaskSpec) { s.Container = ""; s.Resources.ShmGiB = shmPtr(1) }, "applies only to a container task"},
		// A pin's memory is unknown offline; cmd checks it at launch.
		{"pinned type", func(s *TaskSpec) {
			s.Resources.InstanceType, s.Resources.MemoryGiB, s.Resources.ShmGiB = "c8g.16xlarge", 0, shmPtr(64)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := shmSpec(nil)
			tc.mutate(s)
			err := s.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("accepted; want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q lacks %q", err, tc.wantErr)
			}
		})
	}
}

// An explicit "shm_gib": 0 in JSON is rejected, not read as "unset".
func TestParseSpec_ShmGiBZeroRejected(t *testing.T) {
	base := `{"task_id":"t","command":["true"],"container":"img:1","lifecycle":{"ttl":"1h"},"resources":{"memory_gib":8%s}}`
	if _, err := ParseSpec([]byte(strings.Replace(base, "%s", "", 1))); err != nil {
		t.Fatalf("control spec rejected: %v", err)
	}
	if _, err := ParseSpec([]byte(strings.Replace(base, "%s", `,"shm_gib":0`, 1))); err == nil || !strings.Contains(err.Error(), "shm_gib") {
		t.Errorf(`"shm_gib": 0 accepted or misreported: %v`, err)
	}
	spec, err := ParseSpec([]byte(strings.Replace(base, "%s", `,"shm_gib":4`, 1)))
	if err != nil || spec.Resources.ShmGiB == nil || *spec.Resources.ShmGiB != 4 {
		t.Errorf(`"shm_gib": 4 did not parse to 4: spec=%+v err=%v`, spec, err)
	}
}

func TestCheckShmAgainstMemory(t *testing.T) {
	s := shmSpec(shmPtr(64))
	if msg := s.CheckShmAgainstMemory(32); !strings.Contains(msg, "exceeds the instance's 32 GiB") {
		t.Errorf("64 GiB shm on a 32 GiB box: %q", msg)
	}
	if msg := s.CheckShmAgainstMemory(64); msg != "" {
		t.Errorf("64 GiB shm on a 64 GiB box: %q", msg)
	}
	if msg := s.CheckShmAgainstMemory(0); msg != "" {
		t.Errorf("unknown memory must pass: %q", msg)
	}
}
