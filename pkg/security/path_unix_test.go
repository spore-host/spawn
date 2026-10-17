//go:build !windows

package security

import "testing"

// POSIX host: the system directories of Linux (and macOS, where /etc and /var
// live under /private). Windows-only shapes are ordinary file names here.
func TestValidatePathForReading_POSIXHost(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"absolute path in home", "/home/user/data.txt", false},
		{"path in tmp", "/tmp/data.txt", false},
		{"a colon is a legal POSIX file name character", "/tmp/file.txt:stream", false},
		{"so is NUL as a name", "/tmp/NUL", false},
		{"a backslash is not a separator here", `/tmp/a\..\b`, false},

		{"absolute path traversal", "/home/user/../../etc/passwd", true},
		{"etc directory", "/etc/passwd", true},
		{"etc itself", "/etc", true},
		{"etc in upper case (macOS is case-insensitive)", "/ETC/passwd", true},
		{"macOS private etc", "/private/etc/master.passwd", true},
		{"macOS root home", "/var/root/.ssh/id_rsa", true},
		{"repeated separators", "//etc//shadow", true},
		{"sys directory", "/sys/kernel/config", true},
		{"proc directory", "/proc/self/mem", true},
		{"root directory", "/root/.ssh/id_rsa", true},
		{"boot directory", "/boot/vmlinuz", true},
		{"dev directory", "/dev/null", true},
		{"var/lib", "/var/lib/cloud/instance/user-data.txt", true},
		{"run", "/run/secrets/x", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePathForReading(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePathForReading(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}
