package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestGetEnv(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		envValue     string
		defaultValue string
		want         string
		setEnv       bool
	}{
		{
			name:         "returns env var when set",
			key:          "TEST_GETENV_SET",
			envValue:     "custom-value",
			defaultValue: "default",
			want:         "custom-value",
			setEnv:       true,
		},
		{
			name:         "returns default when unset",
			key:          "TEST_GETENV_UNSET_12345",
			defaultValue: "fallback",
			want:         "fallback",
			setEnv:       false,
		},
		{
			name:         "returns default when empty",
			key:          "TEST_GETENV_EMPTY",
			envValue:     "",
			defaultValue: "default-empty",
			want:         "default-empty",
			setEnv:       true,
		},
		{
			name:         "empty default when unset",
			key:          "TEST_GETENV_EMPTY_DEFAULT",
			defaultValue: "",
			want:         "",
			setEnv:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv(tc.key, tc.envValue)
			} else {
				os.Unsetenv(tc.key)
			}

			got := getEnv(tc.key, tc.defaultValue)
			if got != tc.want {
				t.Errorf("getEnv(%q, %q) = %q, want %q", tc.key, tc.defaultValue, got, tc.want)
			}
		})
	}
}

func TestGetEnvOverridesDefault(t *testing.T) {
	const key = "SPAWN_TEST_OVERRIDE_VAR"
	t.Setenv(key, "overridden")

	got := getEnv(key, "original")
	if got != "overridden" {
		t.Errorf("getEnv returned %q, expected overridden value", got)
	}
}

func TestScheduleRecordConstants(t *testing.T) {
	// Verify the default constants are sensible non-empty strings.
	defaults := map[string]string{
		"schedulesTable":   defaultSchedulesTable,
		"historyTable":     defaultHistoryTable,
		"orchestratorFunc": defaultOrchestratorFuncName,
		"accountID":        defaultAccountID,
	}
	for name, val := range defaults {
		if val == "" {
			t.Errorf("constant %s is empty", name)
		}
	}
}

func TestDefaultSchedulesBucketTemplate(t *testing.T) {
	// The template should contain %s for region substitution.
	tmpl := defaultSchedulesBucketTmpl
	if tmpl == "" {
		t.Fatal("defaultSchedulesBucketTmpl is empty")
	}

	// Applying the template should work.
	bucket := fmt.Sprintf(tmpl, "us-east-1")
	if bucket == "" {
		t.Error("formatted bucket name is empty")
	}
	// Should contain the region.
	if !strings.Contains(bucket, "us-east-1") {
		t.Errorf("bucket %q does not contain region", bucket)
	}
}
