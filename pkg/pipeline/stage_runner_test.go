package pipeline

import (
	"reflect"
	"testing"
)

func TestSplitS3URL(t *testing.T) {
	tests := []struct {
		name     string
		s3URL    string
		expected []string
	}{
		{
			name:     "standard s3 url with key",
			s3URL:    "s3://my-bucket/path/to/object.json",
			expected: []string{"my-bucket", "path/to/object.json"},
		},
		{
			name:     "s3 url without prefix",
			s3URL:    "my-bucket/path/to/object.json",
			expected: []string{"my-bucket", "path/to/object.json"},
		},
		{
			name:     "bucket only with s3 prefix",
			s3URL:    "s3://my-bucket",
			expected: []string{"my-bucket"},
		},
		{
			name:     "bucket only without prefix",
			s3URL:    "my-bucket",
			expected: []string{"my-bucket"},
		},
		{
			name:     "nested path",
			s3URL:    "s3://pipeline-data/pipelines/test/config.json",
			expected: []string{"pipeline-data", "pipelines/test/config.json"},
		},
		{
			name:     "bucket with trailing slash",
			s3URL:    "s3://my-bucket/",
			expected: []string{"my-bucket", ""},
		},
		{
			name:     "deep nested path",
			s3URL:    "s3://bucket/a/b/c/d/e/file.txt",
			expected: []string{"bucket", "a/b/c/d/e/file.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := splitS3URL(tt.s3URL)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("splitS3URL(%q) = %v, want %v", tt.s3URL, result, tt.expected)
			}
		})
	}
}

func TestSplitS3URL_EmptyString(t *testing.T) {
	result := splitS3URL("")
	expected := []string{""}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("splitS3URL(\"\") = %v, want %v", result, expected)
	}
}

func TestSplitS3URL_MultiplePaths(t *testing.T) {
	// Verify that only the first slash is used as bucket/key separator
	result := splitS3URL("s3://my-bucket/dir1/dir2/file.txt")
	expected := []string{"my-bucket", "dir1/dir2/file.txt"}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("splitS3URL() = %v, want %v", result, expected)
	}

	// Verify bucket extraction
	if len(result) > 0 && result[0] != "my-bucket" {
		t.Errorf("Expected bucket 'my-bucket', got %q", result[0])
	}

	// Verify key extraction
	if len(result) > 1 && result[1] != "dir1/dir2/file.txt" {
		t.Errorf("Expected key 'dir1/dir2/file.txt', got %q", result[1])
	}
}
