package cmd

import "testing"

func TestIsBurstableInstanceType(t *testing.T) {
	burst := []string{"t2.micro", "t3.large", "t3a.medium", "t4g.nano"}
	notBurst := []string{"m7i.xlarge", "m6i.large", "c7i.2xlarge", "r7i.large", "trn1.2xlarge", "g5.xlarge"}
	for _, it := range burst {
		if !isBurstableInstanceType(it) {
			t.Errorf("%s should be burstable", it)
		}
	}
	for _, it := range notBurst {
		if isBurstableInstanceType(it) {
			t.Errorf("%s should NOT be burstable", it)
		}
	}
}

func TestGuardWindowsInstanceType(t *testing.T) {
	// Non-windows: never an error, even for burstable.
	if err := guardWindowsInstanceType("linux", "t3.large"); err != nil {
		t.Errorf("linux burstable must be allowed: %v", err)
	}
	// Windows + burstable: error mentioning the type + the default.
	err := guardWindowsInstanceType("windows", "t3.large")
	if err == nil {
		t.Fatal("windows + t3.large must be rejected")
	}
	// Windows + non-burstable: ok.
	if err := guardWindowsInstanceType("windows", "m7i.xlarge"); err != nil {
		t.Errorf("windows + m7i.xlarge must be allowed: %v", err)
	}
}

func TestRDPClientCommand(t *testing.T) {
	cases := []struct {
		goos, host, wantBin string
	}{
		{"windows", "1.2.3.4", "mstsc"},
		{"darwin", "1.2.3.4", "open"},
		{"linux", "localhost:13389", "xfreerdp"},
	}
	for _, c := range cases {
		bin, args := rdpClientCommand(c.goos, c.host)
		if bin != c.wantBin {
			t.Errorf("goos=%s: bin=%s want %s", c.goos, bin, c.wantBin)
		}
		if len(args) == 0 {
			t.Errorf("goos=%s: expected args", c.goos)
		}
		// The host must appear in the args somewhere.
		found := false
		for _, a := range args {
			if len(a) >= len(c.host) && (a == c.host || containsStr(a, c.host)) {
				found = true
			}
		}
		if !found {
			t.Errorf("goos=%s: host %q not in args %v", c.goos, c.host, args)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestDefaultWindowsInstanceTypeNonBurstable(t *testing.T) {
	if isBurstableInstanceType(defaultWindowsInstanceType) {
		t.Errorf("default Windows type %s must not be burstable", defaultWindowsInstanceType)
	}
}

// TestWarmFlagDefaultsOn verifies --no-warm defaults to false (warm is on by
// default) and --warm-instance-type defaults to a non-burstable type.
func TestWarmFlagDefaultsOn(t *testing.T) {
	nw := imageImportCmd.Flags().Lookup("no-warm")
	if nw == nil {
		t.Fatal("--no-warm flag not registered")
	}
	if nw.DefValue != "false" {
		t.Errorf("--no-warm default = %q, want false (warm on by default)", nw.DefValue)
	}
	wt := imageImportCmd.Flags().Lookup("warm-instance-type")
	if wt == nil {
		t.Fatal("--warm-instance-type flag not registered")
	}
	if isBurstableInstanceType(wt.DefValue) {
		t.Errorf("--warm-instance-type default %q must not be burstable", wt.DefValue)
	}
	// The default warm seed type must pass the Windows guard.
	if err := guardWindowsInstanceType("windows", wt.DefValue); err != nil {
		t.Errorf("default warm-instance-type %q rejected by Windows guard: %v", wt.DefValue, err)
	}
}
