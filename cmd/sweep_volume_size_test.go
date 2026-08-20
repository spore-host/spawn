package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// Unit coverage for the #544 fix. The end-to-end guarantee — that this value
// reaches the launched instance's actual root EBS volume size — lives in
// test/e2e/tier0_sweep_volume_size_test.go; what is cheap to pin here is the
// precedence rule and the value parsing, following the same split
// cmd/sweep_spend_controls_test.go established for #525.

// TestApplyCLIVolumeSizeToSweep_Precedence: an explicit --volume-size outranks
// the param file's defaults:, and an unset flag never overwrites the file.
func TestApplyCLIVolumeSizeToSweep_Precedence(t *testing.T) {
	// launchVolumeSize is a package-level launch flag; restore it so test order
	// cannot leak a volume size into an unrelated test.
	defer func(v0 int32) { launchVolumeSize = v0 }(launchVolumeSize)

	tests := []struct {
		name                    string
		fileDefaults            map[string]interface{}
		flagSize                int32
		wantSize                interface{}
		wantAppliedContainsNone bool
	}{
		{
			name:         "flag wins over the file's defaults",
			fileDefaults: map[string]interface{}{"volume_size": 40},
			flagSize:     100,
			wantSize:     int32(100),
		},
		{
			name:                    "unset flag leaves the file alone",
			fileDefaults:            map[string]interface{}{"volume_size": 40},
			wantSize:                40,
			wantAppliedContainsNone: true,
		},
		{
			name:                    "--volume-size 0 means unset, not 'set to zero'",
			fileDefaults:            map[string]interface{}{"volume_size": 40},
			flagSize:                0,
			wantSize:                40, // untouched: 0 is the flag's own default
			wantAppliedContainsNone: true,
		},
		{
			name:     "a nil defaults map is created, not panicked on",
			flagSize: 60,
			wantSize: int32(60),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			launchVolumeSize = tc.flagSize
			pf := &ParamFileFormat{Defaults: tc.fileDefaults}

			applied := applyCLIVolumeSizeToSweep(pf)

			if tc.wantSize != nil {
				if got := pf.Defaults["volume_size"]; got != tc.wantSize {
					t.Errorf("defaults[volume_size] = %v (%T), want %v (%T)", got, got, tc.wantSize, tc.wantSize)
				}
			}
			if tc.wantAppliedContainsNone && len(applied) != 0 {
				t.Errorf("reported %v as applied from the command line, but no flag was set", applied)
			}
			if !tc.wantAppliedContainsNone && len(applied) == 0 {
				t.Error("a flag was set but nothing was reported as applied — the launch header " +
					"would not mention it")
			}
		})
	}
}

// TestApplyCLIVolumeSizeToSweep_RowStillWins: the injection targets defaults:,
// so a row's own volume_size: must still override it after the merge — the pair
// to the precedence test above, and the case that would have caught injecting
// into the wrong map (which would clobber every per-row override with a global
// one, exactly the mistake #525's row-still-wins test caught for spend controls).
func TestApplyCLIVolumeSizeToSweep_RowStillWins(t *testing.T) {
	defer func(v0 int32) { launchVolumeSize = v0 }(launchVolumeSize)
	launchVolumeSize = 100

	pf := &ParamFileFormat{
		Params: []map[string]interface{}{
			{"instance_type": "c5.large"},
			{"instance_type": "c5.xlarge", "volume_size": 250},
		},
	}
	applyCLIVolumeSizeToSweep(pf)

	for i, want := range []int32{100, 250} {
		cfg, err := buildLaunchConfigFromParams(pf.Defaults, pf.Params[i], "sw", "sw", i, 2)
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if cfg.RootVolumeSizeGiB != want {
			t.Errorf("row %d: RootVolumeSizeGiB = %v, want %v", i, cfg.RootVolumeSizeGiB, want)
		}
	}
}

// TestBuildLaunchConfigFromParams_VolumeSize: volume_size: is a real config key,
// not a PARAM_* passthrough. Pre-fix it had no parser case, so it landed in
// Parameters and resized nothing (#544).
func TestBuildLaunchConfigFromParams_VolumeSize(t *testing.T) {
	cfg, err := buildLaunchConfigFromParams(
		map[string]interface{}{"volume_size": 100}, nil, "sw", "sw", 0, 1)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams: %v", err)
	}
	if cfg.RootVolumeSizeGiB != 100 {
		t.Errorf("RootVolumeSizeGiB = %v, want 100", cfg.RootVolumeSizeGiB)
	}
	if v, ok := cfg.Parameters["volume_size"]; ok {
		t.Errorf("volume_size also leaked to Parameters[%q] = %q — it would ride as a "+
			"PARAM_volume_size env var that resizes nothing", "volume_size", v)
	}
}

// TestBuildLaunchConfigFromParams_BadVolumeSizeErrors: an unusable volume size
// fails the sweep instead of quietly becoming 0 — which on this path means
// "no override", i.e. spawn's own hardcoded 20 GiB default, the opposite of a
// value the operator explicitly wrote.
func TestBuildLaunchConfigFromParams_BadVolumeSizeErrors(t *testing.T) {
	for _, bad := range []interface{}{"a hundred gigs", "", true, []interface{}{1}, -3, 40.5} {
		_, err := buildLaunchConfigFromParams(
			map[string]interface{}{"volume_size": bad}, nil, "sw", "sw", 0, 1)
		if err == nil {
			t.Errorf("volume_size=%#v: expected an error, got none — an unparsed size silently "+
				"becomes the 20 GiB default", bad)
			continue
		}
		if !strings.Contains(err.Error(), "volume_size") {
			t.Errorf("volume_size=%#v: error %q does not name the offending key", bad, err)
		}
	}
}

// TestParseVolumeSizeGiB covers the shapes the three param-file formats (plus
// the CLI-flag route through applyCLIVolumeSizeToSweep) produce for the same
// number: YAML gives int for `100`, JSON may hand back json.Number, CSV values
// arrive as strings, and the CLI flag arrives as int32 (launchVolumeSize's
// native type) rather than through any file parser.
func TestParseVolumeSizeGiB(t *testing.T) {
	ok := map[string]interface{}{
		"yaml int":    100,
		"int32 (CLI)": int32(100),
		"int64":       int64(100),
		"json.Number": json.Number("100"),
		"csv string":  "100",
		"padded":      "  100  ",
		"zero":        0,
	}
	for name, in := range ok {
		got, err := parseVolumeSizeGiB(in)
		if err != nil {
			t.Errorf("%s (%#v): unexpected error %v", name, in, err)
			continue
		}
		if got != 100 && got != 0 {
			t.Errorf("%s (%#v): got %v", name, in, got)
		}
	}
	bad := map[string]interface{}{
		"prose":       "a hundred gigs",
		"empty":       "",
		"bool":        true,
		"nil":         nil,
		"negative":    -1,
		"fractional":  40.5,
		"not-a-whole": "40.5",
	}
	for name, in := range bad {
		if got, err := parseVolumeSizeGiB(in); err == nil {
			t.Errorf("%s (%#v): expected an error, got %v", name, in, got)
		}
	}
}
