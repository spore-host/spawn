package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// Unit coverage for the #525 fix. The end-to-end guarantee — that these values
// reach the EC2 tags spored enforces — lives in
// test/e2e/tier0_sweep_spend_controls_test.go; what is cheap to pin here is the
// precedence rule and the refusal to read a cost limit it cannot parse.

// TestApplyCLISpendControlsToSweep_Precedence: an explicit flag outranks the
// param file's defaults:, and a flag left unset never overwrites the file.
func TestApplyCLISpendControlsToSweep_Precedence(t *testing.T) {
	// These are package-level launch flags; restore them so test order cannot
	// leak a spend control into an unrelated test.
	defer func(t0, i0 string, c0 float64) { ttl, idleTimeout, costLimit = t0, i0, c0 }(ttl, idleTimeout, costLimit)

	tests := []struct {
		name                    string
		fileDefaults            map[string]interface{}
		flagTTL, flagIdle       string
		flagCost                float64
		wantTTL, wantIdle       interface{}
		wantCost                interface{}
		wantAppliedContainsNone bool
	}{
		{
			name:         "flags win over the file's defaults",
			fileDefaults: map[string]interface{}{"ttl": "8h", "idle_timeout": "2h"},
			flagTTL:      "30m", flagIdle: "10m", flagCost: 3.5,
			wantTTL: "30m", wantIdle: "10m", wantCost: 3.5,
		},
		{
			name:                    "unset flags leave the file alone",
			fileDefaults:            map[string]interface{}{"ttl": "8h", "cost_limit": 9.0},
			wantTTL:                 "8h",
			wantCost:                9.0,
			wantAppliedContainsNone: true,
		},
		{
			name:                    "--cost-limit 0 means disabled, not 'set to zero'",
			fileDefaults:            map[string]interface{}{"cost_limit": 9.0},
			flagCost:                0,
			wantCost:                9.0, // untouched: 0 is the flag's own default
			wantAppliedContainsNone: true,
		},
		{
			name:         "a nil defaults map is created, not panicked on",
			fileDefaults: nil,
			flagTTL:      "1h",
			wantTTL:      "1h",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ttl, idleTimeout, costLimit = tc.flagTTL, tc.flagIdle, tc.flagCost
			pf := &ParamFileFormat{Defaults: tc.fileDefaults}

			applied := applyCLISpendControlsToSweep(pf)

			for key, want := range map[string]interface{}{
				"ttl": tc.wantTTL, "idle_timeout": tc.wantIdle, "cost_limit": tc.wantCost,
			} {
				if want == nil {
					continue
				}
				if got := pf.Defaults[key]; got != want {
					t.Errorf("defaults[%q] = %v (%T), want %v (%T)", key, got, got, want, want)
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

// TestApplyCLISpendControlsToSweep_RowStillWins: the injection targets defaults:,
// so a row's own value must still override it after the merge. This is the pair
// to the precedence test above — injecting into the wrong map would have
// clobbered every per-row override with a global one.
func TestApplyCLISpendControlsToSweep_RowStillWins(t *testing.T) {
	defer func(t0 string, c0 float64) { ttl, costLimit = t0, c0 }(ttl, costLimit)
	ttl, costLimit = "4h", 10

	pf := &ParamFileFormat{
		Params: []map[string]interface{}{
			{"instance_type": "c5.large"},
			{"instance_type": "g6e.xlarge", "ttl": "30m", "cost_limit": 2},
		},
	}
	applyCLISpendControlsToSweep(pf)

	for i, want := range []struct {
		ttl  string
		cost float64
	}{{"4h", 10}, {"30m", 2}} {
		cfg, err := buildLaunchConfigFromParams(pf.Defaults, pf.Params[i], "sw", "sw", i, 2)
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if cfg.TTL != want.ttl {
			t.Errorf("row %d: TTL = %q, want %q", i, cfg.TTL, want.ttl)
		}
		if cfg.CostLimit != want.cost {
			t.Errorf("row %d: CostLimit = %v, want %v", i, cfg.CostLimit, want.cost)
		}
	}
}

// TestSweepRowsWithoutBound pins what the --no-detach guard now checks: the
// merged, per-row bound rather than the CLI variable. The last case is the one
// the old check could not see — a file that bounds some rows and not others.
func TestSweepRowsWithoutBound(t *testing.T) {
	tests := []struct {
		name     string
		defaults map[string]interface{}
		params   []map[string]interface{}
		want     []string
	}{
		{
			name:     "bounded by defaults",
			defaults: map[string]interface{}{"ttl": "1h"},
			params:   []map[string]interface{}{{"instance_type": "c5.large"}},
		},
		{
			name:     "an idle timeout counts as a bound",
			defaults: map[string]interface{}{"idle_timeout": "30m"},
			params:   []map[string]interface{}{{"instance_type": "c5.large"}},
		},
		{
			name:   "bounded per row",
			params: []map[string]interface{}{{"instance_type": "c5.large", "ttl": "1h"}},
		},
		{
			name:   "nothing anywhere",
			params: []map[string]interface{}{{"instance_type": "c5.large"}},
			want:   []string{"row 0 (c5.large)"},
		},
		{
			name:     "an empty string is not a bound",
			defaults: map[string]interface{}{"ttl": ""},
			params:   []map[string]interface{}{{"instance_type": "c5.large"}},
			want:     []string{"row 0 (c5.large)"},
		},
		{
			// The parser's type assertion drops a non-string ttl, so treating it
			// as absent here is what makes the guard agree with what the launch
			// will actually do rather than with what the file appears to say.
			name:     "a non-string ttl is not a bound (the parser drops it too)",
			defaults: map[string]interface{}{"ttl": 3600},
			params:   []map[string]interface{}{{"instance_type": "c5.large"}},
			want:     []string{"row 0 (c5.large)"},
		},
		{
			name: "only SOME rows bounded — invisible to a CLI-variable check",
			params: []map[string]interface{}{
				{"instance_type": "c5.large", "ttl": "1h"},
				{"instance_type": "c5.xlarge"},
				{"instance_type": "c5.2xlarge", "idle_timeout": "20m"},
				{},
			},
			want: []string{"row 1 (c5.xlarge)", "row 3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sweepRowsWithoutBound(&ParamFileFormat{Defaults: tc.defaults, Params: tc.params})
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("sweepRowsWithoutBound() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildLaunchConfigFromParams_CostLimit: `cost_limit:` is a real config key,
// not a PARAM_* passthrough. Pre-fix it had no parser case, so it landed in
// Parameters and capped nothing.
func TestBuildLaunchConfigFromParams_CostLimit(t *testing.T) {
	cfg, err := buildLaunchConfigFromParams(
		map[string]interface{}{"cost_limit": 8}, nil, "sw", "sw", 0, 1)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams: %v", err)
	}
	if cfg.CostLimit != 8 {
		t.Errorf("CostLimit = %v, want 8", cfg.CostLimit)
	}
	if v, ok := cfg.Parameters["cost_limit"]; ok {
		t.Errorf("cost_limit also leaked to Parameters[%q] = %q — it would ride as a "+
			"PARAM_cost_limit env var that caps nothing", "cost_limit", v)
	}
}

// TestBuildLaunchConfigFromParams_BadCostLimitErrors: an unusable cost limit
// fails the sweep instead of quietly becoming 0 (= no cap).
func TestBuildLaunchConfigFromParams_BadCostLimitErrors(t *testing.T) {
	for _, bad := range []interface{}{"eight", "", true, []interface{}{1}, -3} {
		_, err := buildLaunchConfigFromParams(
			map[string]interface{}{"cost_limit": bad}, nil, "sw", "sw", 0, 1)
		if err == nil {
			t.Errorf("cost_limit=%#v: expected an error, got none — a 0 cost limit is no cap", bad)
			continue
		}
		if !strings.Contains(err.Error(), "cost_limit") {
			t.Errorf("cost_limit=%#v: error %q does not name the offending key", bad, err)
		}
	}
}

// TestParseCostLimit covers the shapes the three param-file formats produce for
// the same number: YAML gives int for `8` and float64 for `8.50`, JSON may hand
// back json.Number, and CSV values arrive as strings.
func TestParseCostLimit(t *testing.T) {
	ok := map[string]interface{}{
		"yaml int":      8,
		"yaml float":    8.5,
		"int64":         int64(8),
		"float32":       float32(8),
		"json.Number":   json.Number("8.5"),
		"csv string":    "8.5",
		"padded string": "  8.5  ",
		"zero disables": 0,
	}
	for name, in := range ok {
		got, err := parseCostLimit(in)
		if err != nil {
			t.Errorf("%s (%#v): unexpected error %v", name, in, err)
			continue
		}
		if got != 8 && got != 8.5 && got != 0 {
			t.Errorf("%s (%#v): got %v", name, in, got)
		}
	}
	for name, in := range map[string]interface{}{
		"prose":    "eight dollars",
		"currency": "$8",
		"empty":    "",
		"bool":     true,
		"nil":      nil,
		"negative": -1,
	} {
		if got, err := parseCostLimit(in); err == nil {
			t.Errorf("%s (%#v): expected an error, got %v", name, in, got)
		}
	}
}
