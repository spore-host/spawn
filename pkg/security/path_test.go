package security

import (
	"testing"
)

func TestValidatePathForReading(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "valid relative path",
			path:    "userdata.txt",
			wantErr: false,
		},
		{
			name:    "valid absolute path in home",
			path:    "/home/user/data.txt",
			wantErr: false,
		},
		{
			name:    "valid path in tmp",
			path:    "/tmp/data.txt",
			wantErr: false,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name:    "path traversal attack",
			path:    "../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "absolute path traversal",
			path:    "/home/user/../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "etc directory",
			path:    "/etc/passwd",
			wantErr: true,
		},
		{
			name:    "sys directory",
			path:    "/sys/kernel/config",
			wantErr: true,
		},
		{
			name:    "proc directory",
			path:    "/proc/self/mem",
			wantErr: true,
		},
		{
			name:    "root directory",
			path:    "/root/.ssh/id_rsa",
			wantErr: true,
		},
		{
			name:    "boot directory",
			path:    "/boot/vmlinuz",
			wantErr: true,
		},
		{
			name:    "dev directory",
			path:    "/dev/null",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePathForReading(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePathForReading() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateMountPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "valid mnt path",
			path:    "/mnt/data",
			wantErr: false,
		},
		{
			name:    "valid data path",
			path:    "/data/scratch",
			wantErr: false,
		},
		{
			name:    "valid scratch path",
			path:    "/scratch/temp",
			wantErr: false,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name:    "relative path",
			path:    "mnt/data",
			wantErr: true,
		},
		{
			name:    "path traversal in mnt",
			path:    "/mnt/../etc/passwd",
			wantErr: true,
		},
		{
			name:    "invalid prefix /tmp",
			path:    "/tmp/mount",
			wantErr: true,
		},
		{
			name:    "invalid prefix /home",
			path:    "/home/user/mount",
			wantErr: true,
		},
		{
			name:    "root path",
			path:    "/",
			wantErr: true,
		},
		{
			name:    "etc path",
			path:    "/etc/mnt",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMountPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMountPath() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "clean path",
			path:     "/home/user/data.txt",
			expected: "/home/user/data.txt",
		},
		{
			name:     "path with traversal",
			path:     "/home/user/../../etc/passwd",
			expected: "/etc/passwd",
		},
		{
			name:     "path with double dots",
			path:     "/home/../user/../data.txt",
			expected: "/data.txt",
		},
		{
			name:     "relative path",
			path:     "../../etc/passwd",
			expected: "/etc/passwd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizePath(tt.path)
			if result != tt.expected {
				t.Errorf("SanitizePath() = %v, want %v", result, tt.expected)
			}
		})
	}
}
