package agent

import (
	"context"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
)

// fsxPollInterval is how often spored re-checks a pending FSx's status.
const fsxPollInterval = 20 * time.Second

// fsxMountTimeout bounds how long spored waits for a pending FSx to become
// AVAILABLE before giving up. FSx Lustre creation is ~10 min; allow generous
// headroom. On timeout the instance keeps running (the job may not need the
// mount yet, or the user can investigate) — we never terminate over this.
const fsxMountTimeout = 25 * time.Minute

// maybeMountPendingFSx kicks off the async FSx mount the first time
// spawn:fsx-pending is observed (#221). It's called from the monitor loop after
// each config refresh — not once at startup — because the tag is written AFTER
// RunInstances by the (headless) launch path and is eventually consistent, so it
// may be absent when spored first boots. Idempotent: the mount goroutine is
// started at most once (fsxMountStarted), so a still-pending tag on a later tick
// doesn't spawn a second mount. Logs nothing when there's no pending FSx (the
// common case) to avoid per-tick noise.
func (a *Agent) maybeMountPendingFSx(ctx context.Context) {
	if a.fsxMountStarted {
		return
	}
	if a.cfg().FSxPending == "" {
		return // not (yet) pending; re-checked on the next refresh
	}
	a.fsxMountStarted = true
	go a.mountPendingFSx(ctx)
}

// mountPendingFSx mounts an FSx filesystem that was created asynchronously
// alongside this instance (#194). The launch path tags spawn:fsx-pending=<fs-id>
// and spawn:fsx-mount-point; this polls the FSx API until the filesystem is
// AVAILABLE, mounts it (Lustre, linux only), and flips the tag to spawn:fsx-id
// so the reaper's refcount (#192) sees this instance as a live user.
//
// It runs in its own goroutine off the lifecycle ticker's critical path — the
// poll can block for minutes and must NEVER gate TTL/idle/on-complete/pre-stop
// enforcement (#65). Best-effort: any failure leaves the instance running and is
// logged; we do not terminate over a failed mount.
func (a *Agent) mountPendingFSx(ctx context.Context) {
	// Snapshot the config: this runs in its own goroutine, concurrently with the
	// monitor loop that periodically swaps a.config (#175). cfg() returns an
	// immutable snapshot, so reads below are race-free.
	cfgSnap := a.cfg()
	fsxID := cfgSnap.FSxPending
	if fsxID == "" {
		return
	}
	mountPoint := cfgSnap.FSxMountPoint
	if mountPoint == "" {
		mountPoint = "/fsx"
	}
	region := a.identity.Region
	log.Printf("fsx: pending filesystem %s — waiting for AVAILABLE to mount at %s", fsxID, mountPoint)

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		log.Printf("fsx: load AWS config: %v — cannot mount pending %s", err, fsxID)
		return
	}
	fsxClient := fsx.NewFromConfig(cfg)

	dnsName, mountName, ok := a.waitForFSxAvailable(ctx, fsxClient, fsxID)
	if !ok {
		return // already logged
	}

	// Set up the S3 export DRA before mounting (#194): PERSISTENT_2 links S3 via a
	// separate association once AVAILABLE, and continuous export is what makes
	// results durable (the #184 lesson). Best-effort — a DRA failure is logged but
	// we still mount, so the job can run (it just won't auto-mirror to S3).
	if cfgSnap.FSxImportPath != "" || cfgSnap.FSxExportPath != "" {
		if err := createFSxS3Association(ctx, fsxClient, fsxID, cfgSnap.FSxImportPath, cfgSnap.FSxExportPath); err != nil {
			// Say what is actually broken (#622). "results may not auto-export" was
			// the old wording and it understated the import case badly: with
			// --fsx-import-path the DRA is how the data ARRIVES, so without it the
			// workload reads an EMPTY filesystem — not "results might not sync
			// later". The user who hit this paid for a 1200 GiB filesystem holding
			// nothing and spent 15 minutes finding out why.
			log.Printf("fsx: data-repository association for %s failed: %v — mounting anyway, but %s", fsxID, err, draImpact(cfgSnap.FSxImportPath, cfgSnap.FSxExportPath))
			a.notifier.Notify(context.Background(), "fsx_dra_failed", fsxID+": "+err.Error())
			// Tag it so the failure is visible to `spawn status` rather than only in
			// this log on the box, mirroring spawn:dns-status/spawn:dns-error (#435).
			a.tagFSxDRAStatus(ctx, "failed", err.Error())
		} else {
			log.Printf("fsx: S3 export association created for %s", fsxID)
			a.tagFSxDRAStatus(ctx, "associated", "")
		}
	}

	if err := sysMountLustre(ctx, dnsName, mountName, mountPoint); err != nil {
		log.Printf("fsx: mount %s at %s failed: %v", fsxID, mountPoint, err)
		a.notifier.Notify(context.Background(), "fsx_mount_failed", fsxID+": "+err.Error())
		return
	}
	log.Printf("fsx: mounted %s at %s", fsxID, mountPoint)

	// Flip spawn:fsx-pending → spawn:fsx-id so the reaper counts this instance as
	// an active user of the filesystem (#192 refcount), and remove the pending
	// marker so a spored restart doesn't re-mount.
	a.tagFSxMounted(ctx, fsxID, mountPoint)
}

// createFSxS3Association sets up the continuous-export S3 data-repository
// association on an AVAILABLE PERSISTENT_2 filesystem (NEW/CHANGED/DELETED
// auto-import+export), mirroring pkg/aws.associateFSxS3 — done spored-side for
// the async/ephemeral path so the launch never blocks (#194).
func createFSxS3Association(ctx context.Context, fsxClient *fsx.Client, fsxID, importPath, exportPath string) error {
	repoPath := importPath
	if exportPath != "" {
		repoPath = exportPath
	}
	_, err := fsxClient.CreateDataRepositoryAssociation(ctx, &fsx.CreateDataRepositoryAssociationInput{
		FileSystemId:                aws.String(fsxID),
		FileSystemPath:              aws.String("/"),
		DataRepositoryPath:          aws.String(repoPath),
		BatchImportMetaDataOnCreate: aws.Bool(true),
		S3: &fsxtypes.S3DataRepositoryConfiguration{
			AutoImportPolicy: &fsxtypes.AutoImportPolicy{
				Events: []fsxtypes.EventType{fsxtypes.EventTypeNew, fsxtypes.EventTypeChanged, fsxtypes.EventTypeDeleted},
			},
			AutoExportPolicy: &fsxtypes.AutoExportPolicy{
				Events: []fsxtypes.EventType{fsxtypes.EventTypeNew, fsxtypes.EventTypeChanged, fsxtypes.EventTypeDeleted},
			},
		},
	})
	return err
}

// waitForFSxAvailable polls until the filesystem is AVAILABLE and returns its
// DNS name and Lustre mount name, or (.,.,false) on timeout/terminal failure.
func (a *Agent) waitForFSxAvailable(ctx context.Context, fsxClient *fsx.Client, fsxID string) (dnsName, mountName string, ok bool) {
	deadline := time.Now().Add(fsxMountTimeout)
	for {
		out, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
			FileSystemIds: []string{fsxID},
		})
		if err == nil && len(out.FileSystems) == 1 {
			fsItem := out.FileSystems[0]
			switch fsItem.Lifecycle {
			case "AVAILABLE":
				if fsItem.DNSName != nil && fsItem.LustreConfiguration != nil && fsItem.LustreConfiguration.MountName != nil {
					return aws.ToString(fsItem.DNSName), aws.ToString(fsItem.LustreConfiguration.MountName), true
				}
				log.Printf("fsx: %s AVAILABLE but missing DNS/mount-name — cannot mount", fsxID)
				return "", "", false
			case "FAILED", "DELETING", "MISCONFIGURED":
				log.Printf("fsx: %s entered terminal state %s — not mounting", fsxID, fsItem.Lifecycle)
				return "", "", false
			}
		}
		if time.Now().After(deadline) {
			log.Printf("fsx: %s did not become AVAILABLE within %s — instance keeps running, not mounted", fsxID, fsxMountTimeout)
			return "", "", false
		}
		interval := fsxPollInterval
		if rem := time.Until(deadline); rem < interval {
			interval = rem
		}
		select {
		case <-ctx.Done():
			return "", "", false
		case <-time.After(interval):
		}
	}
}

// tagFSxMounted records the now-mounted filesystem as spawn:fsx-id (the reaper
// refcount lease, #192) and clears spawn:fsx-pending. Best-effort.
// draImpact describes, in the user's terms, what a failed data-repository
// association actually costs for the paths they asked for.
//
// An import path is how data gets ONTO the filesystem, so losing the association
// means the mount is empty; an export path is how results get OFF it, so losing it
// means results stay on a filesystem that may be reclaimed. Both is both.
func draImpact(importPath, exportPath string) string {
	switch {
	case importPath != "" && exportPath != "":
		return "the filesystem will be EMPTY (nothing imported from " + importPath + ") and results will NOT be exported to " + exportPath
	case importPath != "":
		return "the filesystem will be EMPTY — nothing will be imported from " + importPath
	default:
		return "results will NOT be exported to " + exportPath
	}
}

// tagFSxDRAStatus records the outcome of the S3 data-repository association on the
// instance, so `spawn status` can report it instead of it living only in this log
// on the box (#622). It mirrors the DNS pattern in agent.go's tagDNSStatus,
// including deleting a stale error tag on a later success — an instance carrying
// both spawn:fsx-dra-status=associated and a leftover spawn:fsx-dra-error would
// contradict itself.
func (a *Agent) tagFSxDRAStatus(ctx context.Context, status, detail string) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.identity.Region))
	if err != nil {
		log.Printf("fsx: tag dra status: load config: %v", err)
		return
	}
	client := ec2.NewFromConfig(cfg)

	tags := []ec2types.Tag{{Key: aws.String("spawn:fsx-dra-status"), Value: aws.String(status)}}
	if detail != "" {
		tags = append(tags, ec2types.Tag{
			Key: aws.String("spawn:fsx-dra-error"),
			// EC2 tag values cap at 256 characters, and an AWS error string can
			// exceed that — a rejected CreateTags would lose the whole signal, which
			// is the silence this fix is about.
			Value: aws.String(truncateTagValue(detail, 255)),
		})
	}
	if _, err := client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.identity.InstanceID},
		Tags:      tags,
	}); err != nil {
		log.Printf("fsx: write spawn:fsx-dra-status tag: %v", err)
		return
	}

	if detail == "" {
		if _, err := client.DeleteTags(ctx, &ec2.DeleteTagsInput{
			Resources: []string{a.identity.InstanceID},
			Tags:      []ec2types.Tag{{Key: aws.String("spawn:fsx-dra-error")}},
		}); err != nil {
			log.Printf("fsx: clear stale spawn:fsx-dra-error tag: %v", err)
		}
	}
}

// truncateTagValue keeps a tag value within EC2's limit, marking that it was cut
// so a reader doesn't mistake a truncated error for the whole error.
func truncateTagValue(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func (a *Agent) tagFSxMounted(ctx context.Context, fsxID, mountPoint string) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.identity.Region))
	if err != nil {
		log.Printf("fsx: tag mounted: load config: %v", err)
		return
	}
	client := ec2.NewFromConfig(cfg)
	if _, err := client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.identity.InstanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:fsx-id"), Value: aws.String(fsxID)},
			{Key: aws.String("spawn:fsx-mount-point"), Value: aws.String(mountPoint)},
		},
	}); err != nil {
		log.Printf("fsx: write spawn:fsx-id tag: %v", err)
	}
	if _, err := client.DeleteTags(ctx, &ec2.DeleteTagsInput{
		Resources: []string{a.identity.InstanceID},
		Tags:      []ec2types.Tag{{Key: aws.String("spawn:fsx-pending")}},
	}); err != nil {
		log.Printf("fsx: clear spawn:fsx-pending tag: %v", err)
	}
}
