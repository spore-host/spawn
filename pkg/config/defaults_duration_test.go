package config

import "testing"

// #990: `spawn defaults set idle-timeout` used to store anything, and spored
// ignores an idle-timeout tag it cannot parse — no idle timeout at all.
func TestApplyDefaultKeyValidatesIdleTimeout(t *testing.T) {
	var d LaunchDefaults
	for _, ok := range []string{"30m", "1d", "2d12h", ""} {
		if err := applyDefaultKey(&d, "idle-timeout", ok); err != nil {
			t.Errorf("idle-timeout %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"1w", "soon", "0", "-1h"} {
		if err := applyDefaultKey(&d, "idle-timeout", bad); err == nil {
			t.Errorf("idle-timeout %q accepted", bad)
		}
	}
}
