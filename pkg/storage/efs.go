package storage

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// EFSProfile represents a performance profile for EFS mounting
type EFSProfile string

const (
	EFSProfileGeneral       EFSProfile = "general"
	EFSProfileMaxIO         EFSProfile = "max-io"
	EFSProfileMaxThroughput EFSProfile = "max-throughput"
	EFSProfileBurst         EFSProfile = "burst"
)

// EFSMountOptions contains NFS mount options for EFS
type EFSMountOptions struct {
	NFSVers      string // NFS version (4.1)
	RSize        int    // Read buffer size in bytes
	WSize        int    // Write buffer size in bytes
	Hard         bool   // Hard mount (vs soft)
	Timeo        int    // Timeout in deciseconds
	Retrans      int    // Number of retries
	NoResvPort   bool   // Use non-reserved source ports
	ActimeO      int    // Attribute cache timeout in seconds (0 = default)
	Async        bool   // Async writes (faster, less safe)
	NoShareCache bool   // Disable share cache across mounts
	// Extra are --efs-mount-options entries none of the fields above model
	// (nconnect=N, for one), validated by ParseCustomOptions and emitted
	// verbatim, in order, before _netdev (#1390).
	Extra []string
}

// GetEFSProfile returns the mount options for a given performance profile
func GetEFSProfile(profile EFSProfile) (EFSMountOptions, error) {
	switch profile {
	case EFSProfileGeneral:
		return EFSMountOptions{
			NFSVers:    "4.1",
			RSize:      1048576, // 1MB
			WSize:      1048576, // 1MB
			Hard:       true,
			Timeo:      600, // 60 seconds
			Retrans:    2,
			NoResvPort: true,
			ActimeO:    0, // Use default
			Async:      false,
		}, nil

	case EFSProfileMaxIO:
		return EFSMountOptions{
			NFSVers:    "4.1",
			RSize:      1048576,
			WSize:      1048576,
			Hard:       true,
			Timeo:      600,
			Retrans:    2,
			NoResvPort: true,
			ActimeO:    1, // Aggressive caching for metadata
			Async:      false,
		}, nil

	case EFSProfileMaxThroughput:
		return EFSMountOptions{
			NFSVers:    "4.1",
			RSize:      1048576,
			WSize:      1048576,
			Hard:       true,
			Timeo:      600,
			Retrans:    2,
			NoResvPort: true,
			ActimeO:    0,
			Async:      true, // Async writes for max throughput
		}, nil

	case EFSProfileBurst:
		return EFSMountOptions{
			NFSVers:    "4.1",
			RSize:      262144, // 256KB - smaller buffers
			WSize:      262144,
			Hard:       true,
			Timeo:      600,
			Retrans:    2,
			NoResvPort: true,
			ActimeO:    0,
			Async:      false,
		}, nil

	default:
		return EFSMountOptions{}, fmt.Errorf("unknown EFS profile: %s", profile)
	}
}

// ValidateProfile checks if a profile name is valid
func ValidateProfile(profile string) error {
	validProfiles := []string{"general", "max-io", "max-throughput", "burst"}

	for _, valid := range validProfiles {
		if profile == valid {
			return nil
		}
	}

	return fmt.Errorf("invalid EFS profile '%s'. Valid profiles: %s",
		profile, strings.Join(validProfiles, ", "))
}

// ToMountString converts EFSMountOptions to NFS mount option string
func (opts EFSMountOptions) ToMountString() string {
	parts := []string{
		fmt.Sprintf("nfsvers=%s", opts.NFSVers),
		fmt.Sprintf("rsize=%d", opts.RSize),
		fmt.Sprintf("wsize=%d", opts.WSize),
	}

	if opts.Hard {
		parts = append(parts, "hard")
	}

	parts = append(parts, fmt.Sprintf("timeo=%d", opts.Timeo))
	parts = append(parts, fmt.Sprintf("retrans=%d", opts.Retrans))

	if opts.NoResvPort {
		parts = append(parts, "noresvport")
	}

	if opts.ActimeO > 0 {
		parts = append(parts, fmt.Sprintf("actimeo=%d", opts.ActimeO))
	}

	if opts.Async {
		parts = append(parts, "async")
	}

	if opts.NoShareCache {
		parts = append(parts, "nosharecache")
	}

	parts = append(parts, opts.Extra...)

	// Always add _netdev for network filesystems
	parts = append(parts, "_netdev")

	return strings.Join(parts, ",")
}

// mountOptionRE is one NFS mount option as --efs-mount-options may carry it:
// a name, optionally =value, drawn from a set with no shell, fstab or
// option-list metacharacter (no whitespace, quote, $, `, ;, |, &, <, >, #, or
// comma inside a value). The string is single-quoted into the mount command and
// the fstab line (#680), so this is not what keeps the shell safe; it is what
// keeps the fstab line one line of six fields and the option list a list.
var mountOptionRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(=[A-Za-z0-9_.:/+-]+)?$`)

// efsUtilsOnlyOptions are amazon-efs-utils (`mount -t efs`) options. The
// storage script mounts with `mount -t nfs4` (pkg/userdata/storage.go), whose
// kernel client rejects them, so the mount would fail at boot; refuse them at
// launch instead. TLS in particular cannot work there (see storage.go, #718).
var efsUtilsOnlyOptions = map[string]bool{
	"tls": true, "notls": true, "iam": true, "accesspoint": true, "awsprofile": true,
	"awscredsuri": true, "mounttargetip": true, "az": true, "netns": true,
	"ocsp": true, "noocsp": true, "stunnelport": true, "verify": true, "cafile": true,
	"crossaccount": true,
}

// ParseCustomOptions parses a --efs-mount-options string over the general
// profile's defaults. The options it models (nfsvers, rsize, wsize, timeo,
// retrans, actimeo, hard, soft, noresvport, async, nosharecache) set their
// field; any other well-formed option, nconnect for one, is passed through to
// the mount verbatim (#1390: they used to be dropped without a word). A
// malformed option, a non-numeric value for a numeric one, or an efs-utils-only
// option is refused.
func ParseCustomOptions(optString string) (EFSMountOptions, error) {
	// Start with general profile defaults
	opts, _ := GetEFSProfile(EFSProfileGeneral)

	parts := strings.Split(optString, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !mountOptionRE.MatchString(part) {
			return EFSMountOptions{}, fmt.Errorf("--efs-mount-options: %q is not a mount option (want name or name=value, "+
				"using letters, digits and _ . - (and : / + in a value))", part)
		}
		key, value, hasValue := strings.Cut(part, "=")
		if efsUtilsOnlyOptions[key] {
			return EFSMountOptions{}, fmt.Errorf("--efs-mount-options: %q is an amazon-efs-utils option; "+
				"spawn mounts EFS with `mount -t nfs4`, which does not accept it", key)
		}

		num := func(dst *int) error {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return fmt.Errorf("--efs-mount-options: %s=%q is not a non-negative integer", key, value)
			}
			*dst = n
			return nil
		}
		var err error
		switch {
		case hasValue && key == "nfsvers":
			opts.NFSVers = value
		case hasValue && key == "rsize":
			err = num(&opts.RSize)
		case hasValue && key == "wsize":
			err = num(&opts.WSize)
		case hasValue && key == "timeo":
			err = num(&opts.Timeo)
		case hasValue && key == "retrans":
			err = num(&opts.Retrans)
		case hasValue && key == "actimeo":
			err = num(&opts.ActimeO)
		case part == "hard":
			opts.Hard = true
		case part == "soft":
			opts.Hard = false
		case part == "noresvport":
			opts.NoResvPort = true
		case part == "async":
			opts.Async = true
		case part == "nosharecache":
			opts.NoShareCache = true
		case part == "_netdev":
			// ToMountString always ends with it.
		default:
			opts.Extra = append(opts.Extra, part)
		}
		if err != nil {
			return EFSMountOptions{}, err
		}
	}

	return opts, nil
}

// GetProfileDescription returns a description of the profile
func GetProfileDescription(profile EFSProfile) string {
	switch profile {
	case EFSProfileGeneral:
		return "Best for most workloads - balanced throughput and latency"
	case EFSProfileMaxIO:
		return "Best for many small files - aggressive metadata caching (actimeo=1)"
	case EFSProfileMaxThroughput:
		return "Best for large sequential writes - async writes enabled"
	case EFSProfileBurst:
		return "Best for burst workloads - smaller buffers (256KB)"
	default:
		return ""
	}
}
