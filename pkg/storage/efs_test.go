package storage

import (
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

// TestGetEFSProfile tests EFS profile retrieval
func TestGetEFSProfile(t *testing.T) {
	tests := []struct {
		name         string
		profile      EFSProfile
		wantErr      bool
		checkRSize   int
		checkWSize   int
		checkAsync   bool
		checkActimeO int
	}{
		{
			name:         "general profile",
			profile:      EFSProfileGeneral,
			wantErr:      false,
			checkRSize:   1048576,
			checkWSize:   1048576,
			checkAsync:   false,
			checkActimeO: 0,
		},
		{
			name:         "max-io profile",
			profile:      EFSProfileMaxIO,
			wantErr:      false,
			checkRSize:   1048576,
			checkWSize:   1048576,
			checkAsync:   false,
			checkActimeO: 1,
		},
		{
			name:         "max-throughput profile",
			profile:      EFSProfileMaxThroughput,
			wantErr:      false,
			checkRSize:   1048576,
			checkWSize:   1048576,
			checkAsync:   true,
			checkActimeO: 0,
		},
		{
			name:         "burst profile",
			profile:      EFSProfileBurst,
			wantErr:      false,
			checkRSize:   262144,
			checkWSize:   262144,
			checkAsync:   false,
			checkActimeO: 0,
		},
		{
			name:    "invalid profile",
			profile: EFSProfile("invalid"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := GetEFSProfile(tt.profile)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetEFSProfile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			// Verify specific options
			if opts.RSize != tt.checkRSize {
				t.Errorf("RSize = %d, want %d", opts.RSize, tt.checkRSize)
			}
			if opts.WSize != tt.checkWSize {
				t.Errorf("WSize = %d, want %d", opts.WSize, tt.checkWSize)
			}
			if opts.Async != tt.checkAsync {
				t.Errorf("Async = %v, want %v", opts.Async, tt.checkAsync)
			}
			if opts.ActimeO != tt.checkActimeO {
				t.Errorf("ActimeO = %d, want %d", opts.ActimeO, tt.checkActimeO)
			}

			// Verify common options
			if opts.NFSVers != "4.1" {
				t.Errorf("NFSVers = %s, want 4.1", opts.NFSVers)
			}
			if !opts.Hard {
				t.Error("Hard should be true")
			}
			if !opts.NoResvPort {
				t.Error("NoResvPort should be true")
			}
		})
	}
}

// TestValidateProfile tests profile validation
func TestValidateProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		wantErr bool
	}{
		{
			name:    "valid general",
			profile: "general",
			wantErr: false,
		},
		{
			name:    "valid max-io",
			profile: "max-io",
			wantErr: false,
		},
		{
			name:    "valid max-throughput",
			profile: "max-throughput",
			wantErr: false,
		},
		{
			name:    "valid burst",
			profile: "burst",
			wantErr: false,
		},
		{
			name:    "invalid profile",
			profile: "invalid",
			wantErr: true,
		},
		{
			name:    "empty profile",
			profile: "",
			wantErr: true,
		},
		{
			name:    "case sensitive",
			profile: "GENERAL",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProfile(tt.profile)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateProfile() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestToMountString tests mount string generation
func TestToMountString(t *testing.T) {
	tests := []struct {
		name            string
		opts            EFSMountOptions
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "basic options",
			opts: EFSMountOptions{
				NFSVers:    "4.1",
				RSize:      1048576,
				WSize:      1048576,
				Hard:       true,
				Timeo:      600,
				Retrans:    2,
				NoResvPort: true,
			},
			wantContains: []string{
				"nfsvers=4.1",
				"rsize=1048576",
				"wsize=1048576",
				"hard",
				"timeo=600",
				"retrans=2",
				"noresvport",
				"_netdev",
			},
			wantNotContains: []string{
				"async",
				"actimeo",
				"nosharecache",
			},
		},
		{
			name: "with async",
			opts: EFSMountOptions{
				NFSVers:    "4.1",
				RSize:      1048576,
				WSize:      1048576,
				Hard:       true,
				Timeo:      600,
				Retrans:    2,
				NoResvPort: true,
				Async:      true,
			},
			wantContains: []string{
				"async",
			},
		},
		{
			name: "with actimeo",
			opts: EFSMountOptions{
				NFSVers:    "4.1",
				RSize:      1048576,
				WSize:      1048576,
				Hard:       true,
				Timeo:      600,
				Retrans:    2,
				NoResvPort: true,
				ActimeO:    5,
			},
			wantContains: []string{
				"actimeo=5",
			},
		},
		{
			name: "with nosharecache",
			opts: EFSMountOptions{
				NFSVers:      "4.1",
				RSize:        1048576,
				WSize:        1048576,
				Hard:         true,
				Timeo:        600,
				Retrans:      2,
				NoResvPort:   true,
				NoShareCache: true,
			},
			wantContains: []string{
				"nosharecache",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.opts.ToMountString()

			// Check for required substrings
			for _, want := range tt.wantContains {
				if !testutil.Contains(got, want) {
					t.Errorf("ToMountString() = %q, should contain %q", got, want)
				}
			}

			// Check for prohibited substrings
			for _, notWant := range tt.wantNotContains {
				if testutil.Contains(got, notWant) {
					t.Errorf("ToMountString() = %q, should not contain %q", got, notWant)
				}
			}
		})
	}
}

// TestProfileMountStrings tests mount strings for all profiles
func TestProfileMountStrings(t *testing.T) {
	profiles := []EFSProfile{
		EFSProfileGeneral,
		EFSProfileMaxIO,
		EFSProfileMaxThroughput,
		EFSProfileBurst,
	}

	for _, profile := range profiles {
		t.Run(string(profile), func(t *testing.T) {
			opts, err := GetEFSProfile(profile)
			if err != nil {
				t.Fatalf("GetEFSProfile() error = %v", err)
			}

			mountStr := opts.ToMountString()

			// All mount strings should contain these
			required := []string{
				"nfsvers=4.1",
				"rsize=",
				"wsize=",
				"hard",
				"timeo=",
				"retrans=",
				"noresvport",
				"_netdev",
			}

			for _, req := range required {
				if !testutil.Contains(mountStr, req) {
					t.Errorf("mount string missing required option %q: %s", req, mountStr)
				}
			}
		})
	}
}

// TestParseCustomOptions tests parsing of custom mount options
func TestParseCustomOptions(t *testing.T) {
	tests := []struct {
		name           string
		optString      string
		wantErr        bool
		checkRSize     int
		checkWSize     int
		checkNFSVers   string
		checkTimeo     int
		checkRetrans   int
		checkActimeO   int
		checkAsync     bool
		checkSoftMount bool // Only for "soft" test
	}{
		{
			name:         "empty string uses defaults",
			optString:    "",
			wantErr:      false,
			checkRSize:   1048576,
			checkWSize:   1048576,
			checkNFSVers: "4.1",
		},
		{
			name:       "custom rsize and wsize",
			optString:  "rsize=524288,wsize=524288",
			wantErr:    false,
			checkRSize: 524288,
			checkWSize: 524288,
		},
		{
			name:         "custom timeo and retrans",
			optString:    "timeo=300,retrans=5",
			wantErr:      false,
			checkTimeo:   300,
			checkRetrans: 5,
		},
		{
			name:       "enable async",
			optString:  "async",
			wantErr:    false,
			checkAsync: true,
		},
		{
			name:           "soft mount",
			optString:      "soft",
			wantErr:        false,
			checkSoftMount: true,
		},
		{
			name:         "set actimeo",
			optString:    "actimeo=10",
			wantErr:      false,
			checkActimeO: 10,
		},
		{
			name:         "multiple options",
			optString:    "rsize=262144,wsize=262144,async,actimeo=5",
			wantErr:      false,
			checkRSize:   262144,
			checkWSize:   262144,
			checkAsync:   true,
			checkActimeO: 5,
		},
		{
			name:       "options with spaces",
			optString:  "rsize=524288, wsize=524288, async",
			wantErr:    false,
			checkRSize: 524288,
			checkWSize: 524288,
			checkAsync: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := ParseCustomOptions(tt.optString)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCustomOptions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			// Check specific values if provided
			if tt.checkRSize > 0 && opts.RSize != tt.checkRSize {
				t.Errorf("RSize = %d, want %d", opts.RSize, tt.checkRSize)
			}
			if tt.checkWSize > 0 && opts.WSize != tt.checkWSize {
				t.Errorf("WSize = %d, want %d", opts.WSize, tt.checkWSize)
			}
			if tt.checkNFSVers != "" && opts.NFSVers != tt.checkNFSVers {
				t.Errorf("NFSVers = %s, want %s", opts.NFSVers, tt.checkNFSVers)
			}
			if tt.checkTimeo > 0 && opts.Timeo != tt.checkTimeo {
				t.Errorf("Timeo = %d, want %d", opts.Timeo, tt.checkTimeo)
			}
			if tt.checkRetrans > 0 && opts.Retrans != tt.checkRetrans {
				t.Errorf("Retrans = %d, want %d", opts.Retrans, tt.checkRetrans)
			}
			if tt.checkActimeO > 0 && opts.ActimeO != tt.checkActimeO {
				t.Errorf("ActimeO = %d, want %d", opts.ActimeO, tt.checkActimeO)
			}
			if tt.checkAsync && !opts.Async {
				t.Error("Async should be true")
			}
			if tt.checkSoftMount && opts.Hard {
				t.Error("Hard should be false for soft mount")
			}
		})
	}
}

// TestGetProfileDescription tests profile descriptions
func TestGetProfileDescription(t *testing.T) {
	tests := []struct {
		name    string
		profile EFSProfile
		want    string
	}{
		{
			name:    "general description",
			profile: EFSProfileGeneral,
			want:    "balanced",
		},
		{
			name:    "max-io description",
			profile: EFSProfileMaxIO,
			want:    "small files",
		},
		{
			name:    "max-throughput description",
			profile: EFSProfileMaxThroughput,
			want:    "sequential writes",
		},
		{
			name:    "burst description",
			profile: EFSProfileBurst,
			want:    "burst",
		},
		{
			name:    "invalid profile",
			profile: EFSProfile("invalid"),
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetProfileDescription(tt.profile)
			if tt.want != "" && !testutil.Contains(got, tt.want) {
				t.Errorf("GetProfileDescription() = %q, should contain %q", got, tt.want)
			}
			if tt.want == "" && got != "" {
				t.Errorf("GetProfileDescription() = %q, want empty", got)
			}
		})
	}
}

// TestMountOptionsRoundTrip tests parsing and generating mount strings
func TestMountOptionsRoundTrip(t *testing.T) {
	profiles := []EFSProfile{
		EFSProfileGeneral,
		EFSProfileMaxIO,
		EFSProfileMaxThroughput,
		EFSProfileBurst,
	}

	for _, profile := range profiles {
		t.Run(string(profile), func(t *testing.T) {
			// Get profile options
			opts1, err := GetEFSProfile(profile)
			if err != nil {
				t.Fatalf("GetEFSProfile() error = %v", err)
			}

			// Convert to mount string
			mountStr := opts1.ToMountString()

			// Parse it back
			opts2, err := ParseCustomOptions(mountStr)
			if err != nil {
				t.Fatalf("ParseCustomOptions() error = %v", err)
			}

			// Compare key fields (not all fields will round-trip perfectly)
			if opts2.RSize != opts1.RSize {
				t.Errorf("RSize mismatch: got %d, want %d", opts2.RSize, opts1.RSize)
			}
			if opts2.WSize != opts1.WSize {
				t.Errorf("WSize mismatch: got %d, want %d", opts2.WSize, opts1.WSize)
			}
			if opts2.Timeo != opts1.Timeo {
				t.Errorf("Timeo mismatch: got %d, want %d", opts2.Timeo, opts1.Timeo)
			}
			if opts2.Retrans != opts1.Retrans {
				t.Errorf("Retrans mismatch: got %d, want %d", opts2.Retrans, opts1.Retrans)
			}
			if opts2.Async != opts1.Async {
				t.Errorf("Async mismatch: got %v, want %v", opts2.Async, opts1.Async)
			}
		})
	}
}
