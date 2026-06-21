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
			log.Printf("fsx: data-repository association for %s failed: %v — mounting anyway (results may not auto-export to S3)", fsxID, err)
			a.notifier.Notify(context.Background(), "fsx_dra_failed", fsxID+": "+err.Error())
		} else {
			log.Printf("fsx: S3 export association created for %s", fsxID)
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
