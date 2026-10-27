package provenance

// How fast an output upload may go on this box (#1324).
//
// The task wrapper hands every output to `spored provenance upload`, so on a
// release build this, not `aws s3 cp`, is stage-out. It used to upload a
// directory's files one at a time, each with 5 concurrent 5 MiB parts: below
// the AWS CLI's own defaults, and far below the NIC-sized CRT client #1256 gave
// the wrapper's copies (measured 5-15x faster than CLI defaults on stage-in).
//
// UploadTuning sizes the upload from the same NIC-class table the wrapper uses
// (taskproto.NICTargetGbps), the vCPU count and MemTotal.
//
//   - Workers: files uploaded at once.
//   - Requests: part requests in flight across all of them. A file gets
//     Requests divided by the files actually in flight, so one large output
//     gets them all and many small ones share them.
//   - PartSize: also the multipart threshold (s3transfer.WithPartSize).
//
// Memory. The transfer manager buffers up to (concurrency+1) parts per
// multipart upload, so the worst case in flight is PartSize x (Requests +
// Workers). That is held to an eighth of MemTotal: the task's command has
// exited by stage-out, but its page cache, the agent and any sidecar have
// not. When the class's figures do not fit, workers and requests are halved
// together, then the part size drops to 8 MiB and then 5 MiB. The floor is
// the old fixed setting (1 file, 5 parts of 5 MiB), so a small box never does
// worse than before and never holds more than it did.

const (
	mib = int64(1) << 20
	// minPartSize is S3's minimum part size, and the old fixed setting.
	minPartSize = 5 * mib
	// floorRequests is the old fixed part concurrency.
	floorRequests = 5
	// unknownMemBudget is the buffer budget when MemTotal can't be read: what
	// an eighth of a 1 GiB box allows.
	unknownMemBudget = 128 * mib
)

// UploadTuning is how an output upload is run. See the file comment.
type UploadTuning struct {
	Workers  int
	Requests int
	PartSize int64
}

// MaxBufferBytes is the most part-buffer memory the tuning can hold at once.
func (t UploadTuning) MaxBufferBytes() int64 {
	return t.PartSize * int64(t.Requests+t.Workers)
}

// PartConcurrency is the part concurrency for one file while active files are
// in flight: an even share of Requests, at least 1.
func (t UploadTuning) PartConcurrency(active int) int {
	if active < 1 {
		active = 1
	}
	return max(1, t.Requests/active)
}

// uploadClass is one row of the size-class table: the most a box of at most
// MaxGbps may use before the vCPU and memory bounds.
type uploadClass struct {
	MaxGbps           int
	Workers, Requests int
	PartSize          int64
}

// uploadClasses is ordered by MaxGbps; the first row a box's NIC fits is used.
// Row 0 is a box whose type gave no figure (unknown size or no IMDS):
// modestly above the old setting. Requests grow with the NIC because one S3
// connection carries a few hundred Mb/s at most; a NIC is filled by many.
var uploadClasses = []uploadClass{
	{MaxGbps: 0, Workers: 4, Requests: 32, PartSize: 8 * mib},
	{MaxGbps: 15, Workers: 8, Requests: 64, PartSize: 16 * mib},
	{MaxGbps: 25, Workers: 16, Requests: 128, PartSize: 16 * mib},
	{MaxGbps: 50, Workers: 16, Requests: 256, PartSize: 16 * mib},
	{MaxGbps: 1 << 30, Workers: 32, Requests: 512, PartSize: 16 * mib},
}

// Per-vCPU caps. A part costs a CRC32 over its bytes and a file costs a
// goroutine reading it; neither is worth outrunning the CPU for.
const (
	workersPerCPU  = 2
	requestsPerCPU = 16
)

// TuneUpload picks the tuning for a box with a NIC of nicGbps (0 when
// unknown), vcpus CPUs and memKiB of MemTotal (0 when unknown).
func TuneUpload(nicGbps, vcpus int, memKiB int64) UploadTuning {
	c := uploadClasses[len(uploadClasses)-1]
	for _, r := range uploadClasses {
		if nicGbps <= r.MaxGbps {
			c = r
			break
		}
	}
	t := UploadTuning{Workers: c.Workers, Requests: c.Requests, PartSize: c.PartSize}
	if vcpus > 0 {
		t.Workers = min(t.Workers, workersPerCPU*vcpus)
		t.Requests = min(t.Requests, requestsPerCPU*vcpus)
	}
	t.Requests = max(t.Requests, t.Workers, floorRequests)
	budget := unknownMemBudget
	if memKiB > 0 {
		budget = memKiB * 1024 / 8
	}
	for t.MaxBufferBytes() > budget {
		switch {
		case t.Workers > 1 || t.Requests > floorRequests:
			t.Workers = max(1, t.Workers/2)
			t.Requests = max(floorRequests, t.Requests/2)
		case t.PartSize > 8*mib:
			t.PartSize = 8 * mib
		case t.PartSize > minPartSize:
			t.PartSize = minPartSize
		default:
			return t // the old fixed setting; never go below it
		}
	}
	return t
}
