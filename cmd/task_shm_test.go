package cmd

import (
	"context"
	"strings"
	"testing"
)

// #1111: a pinned instance_type's memory bounds resources.shm_gib at launch.
func TestCheckPinnedShm(t *testing.T) {
	spec := containerCPUOnlySpec()
	spec.Resources.InstanceType = "c8g.16xlarge"
	shm := 64.0
	spec.Resources.ShmGiB = &shm

	f := &fakeMemoryLookup{mib: 32 * 1024, ok: true}
	err := checkPinnedShm(context.Background(), f, "us-east-1", spec)
	if err == nil || !strings.Contains(err.Error(), "shm_gib") || !strings.Contains(err.Error(), "c8g.16xlarge") {
		t.Errorf("64 GiB shm on a 32 GiB pin: err = %v", err)
	}
	if f.seen != "c8g.16xlarge" {
		t.Errorf("looked up %q, want the pinned type", f.seen)
	}
	for name, l := range map[string]*fakeMemoryLookup{
		"fits":    {mib: 128 * 1024, ok: true},
		"unknown": {ok: false},
	} {
		if err := checkPinnedShm(context.Background(), l, "us-east-1", spec); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	spec.Resources.ShmGiB = nil
	if err := checkPinnedShm(context.Background(), f, "us-east-1", spec); err != nil {
		t.Errorf("shm unset: %v", err)
	}
}
