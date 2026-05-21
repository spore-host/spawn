package storage

import (
	"fmt"
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

	// Always add _netdev for network filesystems
	parts = append(parts, "_netdev")

	return strings.Join(parts, ",")
}

// ParseCustomOptions parses a custom mount options string
func ParseCustomOptions(optString string) (EFSMountOptions, error) {
	// Start with general profile defaults
	opts, _ := GetEFSProfile(EFSProfileGeneral)

	if optString == "" {
		return opts, nil
	}

	parts := strings.Split(optString, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Handle key=value options
		if strings.Contains(part, "=") {
			kv := strings.SplitN(part, "=", 2)
			key := strings.TrimSpace(kv[0])
			value := strings.TrimSpace(kv[1])

			switch key {
			case "nfsvers":
				opts.NFSVers = value
			case "rsize":
				var size int
				_, _ = fmt.Sscanf(value, "%d", &size)
				opts.RSize = size
			case "wsize":
				var size int
				_, _ = fmt.Sscanf(value, "%d", &size)
				opts.WSize = size
			case "timeo":
				var t int
				_, _ = fmt.Sscanf(value, "%d", &t)
				opts.Timeo = t
			case "retrans":
				var r int
				_, _ = fmt.Sscanf(value, "%d", &r)
				opts.Retrans = r
			case "actimeo":
				var a int
				_, _ = fmt.Sscanf(value, "%d", &a)
				opts.ActimeO = a
			default:
				// Ignore unknown options (pass through)
			}
		} else {
			// Handle boolean flags
			switch part {
			case "hard":
				opts.Hard = true
			case "soft":
				opts.Hard = false
			case "noresvport":
				opts.NoResvPort = true
			case "async":
				opts.Async = true
			case "nosharecache":
				opts.NoShareCache = true
			}
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
