package security

import (
	"errors"
	"path/filepath"
	"strings"
)

// ValidatePathForReading validates a file path for safe reading operations.
// It prevents path traversal attacks and access to system directories.
func ValidatePathForReading(path string) error {
	if path == "" {
		return errors.New("path cannot be empty")
	}

	// Clean the path to resolve . and ..
	cleaned := filepath.Clean(path)

	// Check for path traversal attempts
	if strings.Contains(cleaned, "..") {
		return errors.New("path traversal not allowed")
	}

	// Get absolute path for system directory checks
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		// If we can't get absolute path, be conservative and reject
		return errors.New("cannot validate path")
	}

	// List of forbidden system directories
	forbiddenPrefixes := []string{
		"/etc/",
		"/sys/",
		"/proc/",
		"/root/",
		"/boot/",
		"/dev/",
		"/var/lib/",
		"/run/",
	}

	for _, prefix := range forbiddenPrefixes {
		if strings.HasPrefix(abs, prefix) {
			return errors.New("access to system paths not allowed")
		}
	}

	return nil
}

// ValidateMountPath validates a filesystem mount path.
// Mount paths must be absolute and under /mnt or /data.
func ValidateMountPath(path string) error {
	if path == "" {
		return errors.New("mount path cannot be empty")
	}

	// Clean the path
	cleaned := filepath.Clean(path)

	// Must be absolute
	if !filepath.IsAbs(cleaned) {
		return errors.New("mount path must be absolute")
	}

	// Check for path traversal
	if strings.Contains(cleaned, "..") {
		return errors.New("path traversal not allowed in mount path")
	}

	// Allowed mount prefixes
	allowedPrefixes := []string{
		"/mnt/",
		"/data/",
		"/scratch/",
	}

	allowed := false
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(cleaned, prefix) {
			allowed = true
			break
		}
	}

	if !allowed {
		return errors.New("mount path must be under /mnt, /data, or /scratch")
	}

	return nil
}

// SanitizePath sanitizes a path for safe logging and display.
// It removes any path traversal sequences.
func SanitizePath(path string) string {
	// Clean the path
	cleaned := filepath.Clean(path)

	// Remove any remaining .. sequences and clean up double slashes
	cleaned = strings.ReplaceAll(cleaned, "..", "")
	cleaned = filepath.Clean(cleaned)

	return cleaned
}
