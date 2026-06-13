package cmd

import "testing"

// TestParseAttachVolumes covers the --attach-volume parser (#144):
// snap-xxx:/mount/point[:ro|:rw].
func TestParseAttachVolumes(t *testing.T) {
	t.Run("snapshot and mount, default read-write", func(t *testing.T) {
		specs, err := parseAttachVolumes([]string{"snap-0abc:/opt/databases/kraken2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(specs) != 1 {
			t.Fatalf("got %d specs, want 1", len(specs))
		}
		if specs[0].SnapshotID != "snap-0abc" || specs[0].MountPoint != "/opt/databases/kraken2" {
			t.Errorf("parsed = %+v", specs[0])
		}
		if specs[0].ReadOnly {
			t.Error("default should be read-write")
		}
	})

	t.Run("explicit ro and rw", func(t *testing.T) {
		specs, err := parseAttachVolumes([]string{"snap-aaa:/ref:ro", "snap-bbb:/data:rw"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !specs[0].ReadOnly {
			t.Error("snap-aaa should be read-only")
		}
		if specs[1].ReadOnly {
			t.Error("snap-bbb should be read-write")
		}
	})

	t.Run("rejects bad forms", func(t *testing.T) {
		bad := []string{
			"snap-0abc",               // no mount
			"snap-0abc:relative/path", // mount not absolute
			"vol-0abc:/mnt",           // not a snapshot id
			"snap-0abc:/mnt:badmode",  // bad mode
			"snap-0abc:/mnt:ro:extra", // too many parts
		}
		for _, in := range bad {
			if _, err := parseAttachVolumes([]string{in}); err == nil {
				t.Errorf("expected error for %q, got nil", in)
			}
		}
	})
}

// TestAttachedVolumesUserData verifies the launch config specs map to the
// storage user-data mount list with the matching EC2 device names (#144).
func TestAttachedVolumesUserData(t *testing.T) {
	specs, err := parseAttachVolumes([]string{"snap-aaa:/ref:ro", "snap-bbb:/data"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	vols := attachedVolumesUserData(specs)
	if len(vols) != 2 {
		t.Fatalf("got %d vols, want 2", len(vols))
	}
	if vols[0].DeviceName != "/dev/sdf" || vols[1].DeviceName != "/dev/sdg" {
		t.Errorf("device names = %q,%q, want /dev/sdf,/dev/sdg", vols[0].DeviceName, vols[1].DeviceName)
	}
	if vols[0].MountPoint != "/ref" || !vols[0].ReadOnly {
		t.Errorf("vol[0] = %+v", vols[0])
	}
	if vols[1].MountPoint != "/data" || vols[1].ReadOnly {
		t.Errorf("vol[1] = %+v", vols[1])
	}
}
