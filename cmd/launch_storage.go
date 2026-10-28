package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/userdata"
)

// The storage-mount script: the part of a launch's user data that mounts its
// EFS filesystem, its FSx for Lustre filesystem and its --attach-volume data
// volumes before the workload starts (#166).
//
// A parameter sweep used to build its members' user data with no storage
// script at all, on both orchestration paths, so --efs-id and --fsx-id were
// tagged and never mounted and an attached volume was attached and never
// formatted or mounted (#1366). Both sweep paths and the single launch now
// render it with launchStorageScript.

// efsMountTargetLooker resolves an EFS filesystem's mount-target IP, the
// first-boot fallback for its DNS name (#704). *aws.Client satisfies it.
type efsMountTargetLooker interface {
	EFSMountTargetIP(ctx context.Context, filesystemID, region, subnetID, az string) (string, error)
}

// launchStorageScript renders cfg's storage-mount script and the mount points
// --command must see live before it starts (#668). fsxInfo is the FSx for
// Lustre filesystem to mount, already looked up or created; nil mounts none.
// Windows gets no script: it is Linux mount scripting (#55), and the Windows
// flag guard refuses the storage flags (#1022).
func launchStorageScript(ctx context.Context, l efsMountTargetLooker, cfg *aws.LaunchConfig, fsxInfo *aws.FSxInfo, warn io.Writer) (string, []string, error) {
	if (cfg.EFSID == "" && fsxInfo == nil && len(cfg.AttachVolumes) == 0) || cfg.TargetOS == "windows" {
		return "", nil, nil
	}
	storageConfig := userdata.StorageConfig{}
	// Mount points --command must see live before it starts (#668). Only EFS and
	// FSx are required: attached EBS data volumes mount with `nofail` by design,
	// so a missing one must not strand a workload that never referenced it.
	var readyMountPoints []string

	if cfg.EFSID != "" {
		mountOptions, err := getEFSMountOptions()
		if err != nil {
			return "", nil, fmt.Errorf("failed to get EFS mount options: %w", err)
		}
		storageConfig.EFSEnabled = true
		storageConfig.EFSFilesystemDNS = aws.GetEFSDNSName(cfg.EFSID, cfg.Region)
		storageConfig.EFSMountPoint = cfg.EFSMountPoint
		storageConfig.EFSMountOptions = mountOptions
		readyMountPoints = append(readyMountPoints, cfg.EFSMountPoint)

		// Resolve the mount target IP as a boot-time fallback (#704). The DNS
		// name is not reliable for a freshly created filesystem: a real run
		// failed all six retry attempts over ~60s with the mount target
		// available in this very subnet, while mounting by IP worked first try.
		// Best-effort — a launch must not fail because this lookup did, since
		// DNS works in the steady state.
		if ip, ferr := l.EFSMountTargetIP(ctx, cfg.EFSID, cfg.Region, cfg.SubnetID, cfg.AvailabilityZone); ferr != nil {
			fmt.Fprintf(warn, "⚠️  could not resolve an EFS mount-target IP for %s (%v); "+
				"the mount will rely on DNS alone, which can fail at first boot (#704)\n", cfg.EFSID, ferr)
		} else if ip != "" {
			storageConfig.EFSMountTargetIP = ip
		}
	}

	if fsxInfo != nil {
		storageConfig.FSxLustreEnabled = true
		storageConfig.FSxFilesystemDNS = fsxInfo.DNSName
		storageConfig.FSxMountName = fsxInfo.MountName
		storageConfig.FSxMountPoint = cfg.FSxMountPoint
		readyMountPoints = append(readyMountPoints, cfg.FSxMountPoint)
	}

	// Attached EBS data volumes from snapshots (#144)
	storageConfig.AttachedVolumes = attachedVolumesUserData(cfg.AttachVolumes)

	script, err := userdata.GenerateStorageUserData(storageConfig)
	if err != nil {
		return "", nil, fmt.Errorf("failed to generate storage user-data: %w", err)
	}
	return script, readyMountPoints, nil
}

// sweepStorageLookups is what rendering a sweep member's storage script asks
// AWS. *aws.Client satisfies it.
type sweepStorageLookups interface {
	efsMountTargetLooker
	GetFSxFilesystem(ctx context.Context, filesystemID, region string) (*aws.FSxInfo, error)
}

// sweepStorage renders each sweep member's storage-mount script as a single
// launch renders its own: the --fsx-id filesystem is looked up (once per
// region; a filesystem in another region fails the lookup, before anything is
// launched) unless --fsx-skip-validate, which on a single launch skips the
// mount with the lookup, and the EFS mount-target IP once per subnet and AZ.
type sweepStorage struct {
	lookups sweepStorageLookups
	warn    io.Writer
	fsx     map[string]*aws.FSxInfo
	efsIP   map[string]string
}

func newSweepStorage(l sweepStorageLookups, warn io.Writer) *sweepStorage {
	return &sweepStorage{lookups: l, warn: warn, fsx: map[string]*aws.FSxInfo{}, efsIP: map[string]string{}}
}

// EFSMountTargetIP memoizes the lookup, so a sweep warns once, not per member.
func (s *sweepStorage) EFSMountTargetIP(ctx context.Context, filesystemID, region, subnetID, az string) (string, error) {
	k := filesystemID + "|" + region + "|" + subnetID + "|" + az
	if ip, ok := s.efsIP[k]; ok {
		return ip, nil
	}
	ip, err := s.lookups.EFSMountTargetIP(ctx, filesystemID, region, subnetID, az)
	if err != nil {
		return "", err
	}
	s.efsIP[k] = ip
	return ip, nil
}

// member renders cfg's storage script, and fills in the FSx mount name and
// mount point its tags carry, as launchWithProgress does for a single launch.
func (s *sweepStorage) member(ctx context.Context, cfg *aws.LaunchConfig) (string, []string, error) {
	var info *aws.FSxInfo
	if cfg.FSxLustreID != "" && !fsxSkipValidate && cfg.TargetOS != "windows" {
		k := cfg.FSxLustreID + "|" + cfg.Region
		info = s.fsx[k]
		if info == nil {
			got, err := s.lookups.GetFSxFilesystem(ctx, cfg.FSxLustreID, cfg.Region)
			if err != nil {
				return "", nil, fmt.Errorf("--fsx-id %s in %s: %w", cfg.FSxLustreID, cfg.Region, err)
			}
			s.fsx[k] = got
			info = got
		}
		cfg.FSxMountName = info.MountName
		if cfg.FSxMountPoint == "" {
			cfg.FSxMountPoint = fsxMountPoint
		}
	}
	return launchStorageScript(ctx, s, cfg, info, s.warn)
}
