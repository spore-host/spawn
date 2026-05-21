package plugin_test

import (
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestParseSpec(t *testing.T) {
	yaml := `
name: test-plugin
version: "1.0.0"
description: A test plugin
config:
  data_dir:
    default: /data
  endpoint_name:
    required: true
remote:
  health:
    interval: 1m
    steps:
      - type: run
        run: echo ok
`
	spec, err := plugin.ParseSpec([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}

	if spec.Name != "test-plugin" {
		t.Errorf("Name = %q, want %q", spec.Name, "test-plugin")
	}
	if spec.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", spec.Version, "1.0.0")
	}
	if spec.Remote.Health.Interval != "1m" {
		t.Errorf("Health.Interval = %q, want %q", spec.Remote.Health.Interval, "1m")
	}
}

func TestParseSpec_MissingName(t *testing.T) {
	_, err := plugin.ParseSpec([]byte("version: 1.0.0\n"))
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestResolvedConfig(t *testing.T) {
	spec := &plugin.PluginSpec{
		Name: "p",
		Config: map[string]plugin.ConfigParam{
			"dir":  {Default: "/data"},
			"name": {Required: true},
		},
	}

	tests := []struct {
		name    string
		input   map[string]string
		wantErr bool
		wantDir string
	}{
		{
			name:    "required present",
			input:   map[string]string{"name": "myep"},
			wantDir: "/data",
		},
		{
			name:    "override default",
			input:   map[string]string{"name": "myep", "dir": "/custom"},
			wantDir: "/custom",
		},
		{
			name:    "missing required",
			input:   map[string]string{},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := spec.ResolvedConfig(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got["dir"] != tc.wantDir {
				t.Errorf("dir = %q, want %q", got["dir"], tc.wantDir)
			}
		})
	}
}
