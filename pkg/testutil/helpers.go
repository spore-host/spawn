package testutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

// CreateTempDir creates a temporary directory for testing and registers cleanup
func CreateTempDir(t *testing.T, pattern string) string {
	t.Helper()

	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	return dir
}

// CreateTempFile creates a temporary file with content and registers cleanup
func CreateTempFile(t *testing.T, dir, pattern, content string) string {
	t.Helper()

	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	return f.Name()
}

// AssertFileExists checks if a file exists at the given path
func AssertFileExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("expected file to exist: %s", path)
	}
}

// AssertFileNotExists checks if a file does not exist at the given path
func AssertFileNotExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected file to not exist: %s", path)
	}
}

// AssertFileContains checks if a file contains the expected content
func AssertFileContains(t *testing.T, path, expected string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file %s: %v", path, err)
	}

	if !Contains(string(content), expected) {
		t.Errorf("file %s does not contain expected content %q\nGot: %s", path, expected, string(content))
	}
}

// AssertDirExists checks if a directory exists at the given path
func AssertDirExists(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		t.Errorf("expected directory to exist: %s", path)
		return
	}
	if err != nil {
		t.Fatalf("failed to stat directory %s: %v", path, err)
	}
	if !info.IsDir() {
		t.Errorf("expected %s to be a directory, but it's a file", path)
	}
}

// MockAWSConfig returns a mock AWS config for testing
func MockAWSConfig(ctx context.Context) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
	)
}

// ChdirTemp changes to a temporary directory for testing and registers cleanup
func ChdirTemp(t *testing.T) string {
	t.Helper()

	tmpDir := CreateTempDir(t, "spawn-test-*")
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current directory: %v", err)
	}

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to change to temp directory: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})

	return tmpDir
}

// WriteFile writes content to a file (creating directories if needed)
func WriteFile(t *testing.T, path, content string) {
	t.Helper()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create directory %s: %v", dir, err)
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write file %s: %v", path, err)
	}
}

// ReadFile reads a file and returns its content
func ReadFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file %s: %v", path, err)
	}

	return string(content)
}

// AssertNoError fails the test if err is not nil
func AssertNoError(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// AssertError fails the test if err is nil
func AssertError(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("expected error but got nil")
	}
}

// AssertEqual fails the test if got != want
func AssertEqual(t *testing.T, got, want interface{}) {
	t.Helper()

	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// AssertStringContains fails the test if s does not contain substr
func AssertStringContains(t *testing.T, s, substr string) {
	t.Helper()

	if !Contains(s, substr) {
		t.Errorf("string %q does not contain %q", s, substr)
	}
}

// AssertStringNotContains fails the test if s contains substr
func AssertStringNotContains(t *testing.T, s, substr string) {
	t.Helper()

	if Contains(s, substr) {
		t.Errorf("string %q should not contain %q", s, substr)
	}
}

// Contains checks if a string contains a substring
func Contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// SplitString splits a string by a separator
func SplitString(s, sep string) []string {
	if s == "" {
		return []string{}
	}
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			parts = append(parts, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// TrimSpace trims whitespace from a string
func TrimSpace(s string) string {
	start := 0
	end := len(s)

	// Trim leading spaces
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n') {
		start++
	}

	// Trim trailing spaces
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n') {
		end--
	}

	return s[start:end]
}

// ReplaceString replaces all occurrences of old with new in s
func ReplaceString(s, old, new string) string {
	if old == "" {
		return s
	}
	result := ""
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			result += new
			i += len(old)
		} else {
			result += string(s[i])
			i++
		}
	}
	return result
}

// IntToString converts an integer to a string
func IntToString(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}
