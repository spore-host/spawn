package config

import "testing"

// setTestHome points the user's home directory at dir for the rest of the test.
//
// HOME alone is not enough: os.UserHomeDir reads USERPROFILE on Windows, so a
// test that only set HOME read the real profile there (and failed, or worse,
// passed against a developer's own ~/.spawn) until the Windows CI job ran it
// (#971).
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}
