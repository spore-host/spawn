package provenance

import "testing"

// old is the fixed setting before #1324: one file at a time, 5 concurrent
// 5 MiB parts. No box may be tuned below it.
var old = UploadTuning{Workers: 1, Requests: 5, PartSize: 5 * mib}

const gib = int64(1) << 30

// The size-class table as the file comment states it, on a box with CPU and
// memory to spare: a NIC is matched to the first row it fits, an unknown NIC
// gets the modest row, and the figures grow with the NIC.
func TestTuneUploadFollowsTheSizeClassTable(t *testing.T) {
	plenty := int64(2048) * gib / 1024 // KiB for 2 TiB
	for _, c := range []struct {
		gbps int
		want UploadTuning
	}{
		{0, UploadTuning{4, 32, 8 * mib}},
		{15, UploadTuning{8, 64, 16 * mib}},
		{25, UploadTuning{16, 128, 16 * mib}},
		{30, UploadTuning{16, 256, 16 * mib}},
		{50, UploadTuning{16, 256, 16 * mib}},
		{200, UploadTuning{32, 512, 16 * mib}},
	} {
		if got := TuneUpload(c.gbps, 192, plenty); got != c.want {
			t.Errorf("%d Gb/s: %+v, want %+v", c.gbps, got, c.want)
		}
	}
	for i := 1; i < len(uploadClasses); i++ {
		a, b := uploadClasses[i-1], uploadClasses[i]
		if b.MaxGbps <= a.MaxGbps || b.Workers < a.Workers || b.Requests < a.Requests || b.PartSize < a.PartSize {
			t.Errorf("row %d (%+v) is not above row %d (%+v)", i, b, i-1, a)
		}
	}
	if r := uploadClasses[0]; r.Workers <= old.Workers || r.Requests <= old.Requests {
		t.Errorf("the unknown-NIC row %+v is not above the old setting %+v", r, old)
	}
}

// The memory bound: the most part buffer a tuning can hold is at most an
// eighth of MemTotal, except where that would go below the old setting, which
// is then used exactly. Swept over every class, CPU count and memory size.
func TestTuneUploadHoldsBuffersToAnEighthOfMemory(t *testing.T) {
	cases := 0
	for _, gbps := range []int{0, 15, 25, 50, 100, 200} {
		for _, vcpus := range []int{0, 1, 2, 4, 8, 16, 64, 192} {
			for mem := int64(256) << 10; mem <= int64(4)<<30; mem *= 2 { // KiB: 256 MiB .. 4 TiB
				cases++
				got := TuneUpload(gbps, vcpus, mem)
				if got.Workers < 1 || got.Requests < got.Workers || got.Requests < old.Requests || got.PartSize < old.PartSize {
					t.Errorf("%d Gb/s, %d vCPU, %d KiB: %+v is below the old setting", gbps, vcpus, mem, got)
				}
				if got.MaxBufferBytes() > mem*1024/8 && got != old {
					t.Errorf("%d Gb/s, %d vCPU, %d KiB: %+v holds %d MiB, over an eighth of memory (%d MiB)",
						gbps, vcpus, mem, got, got.MaxBufferBytes()>>20, mem/8>>10)
				}
			}
		}
	}
	if cases < 100 {
		t.Fatalf("swept %d cases", cases)
	}
	// Unknown memory is budgeted as a 1 GiB box.
	if got := TuneUpload(200, 192, 0); got.MaxBufferBytes() > unknownMemBudget {
		t.Errorf("unknown memory: %+v holds %d MiB", got, got.MaxBufferBytes()>>20)
	}
}

// Small boxes stay conservative; a big one is not held back.
func TestTuneUploadSmallAndLargeBoxes(t *testing.T) {
	// t4g.nano: 2 vCPU, 0.5 GiB, a 15 Gb/s burst class: 64 MiB of buffer at most.
	if got := TuneUpload(15, 2, 512<<10); got.MaxBufferBytes() > 64*mib {
		t.Errorf("t4g.nano: %+v holds %d MiB", got, got.MaxBufferBytes()>>20)
	}
	// m7i.large: 2 vCPU, 8 GiB: the vCPU caps bind before memory.
	if got := TuneUpload(15, 2, 8<<20); got != (UploadTuning{4, 32, 16 * mib}) {
		t.Errorf("m7i.large: %+v", got)
	}
	// r8g.16xlarge: 64 vCPU, 512 GiB, 30 Gb/s: the class figures stand.
	if got := TuneUpload(30, 64, 512<<20); got != (UploadTuning{16, 256, 16 * mib}) {
		t.Errorf("r8g.16xlarge: %+v", got)
	}
}

func TestPartConcurrencySharesRequests(t *testing.T) {
	tune := UploadTuning{Workers: 8, Requests: 64}
	for active, want := range map[int]int{0: 64, 1: 64, 4: 16, 8: 8, 100: 1} {
		if got := tune.PartConcurrency(active); got != want {
			t.Errorf("%d active: %d, want %d", active, got, want)
		}
	}
}
