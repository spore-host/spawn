package plugin_test

import (
	"strings"
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

// A config key the spec does not declare is refused, naming the keys it does
// accept (#1116). It used to be passed through, so a typo or a stale key was
// silently ignored by an install that never read it.
func TestResolvedConfig_RejectsUndeclaredKeys(t *testing.T) {
	spec := &plugin.PluginSpec{
		Name: "p",
		Config: map[string]plugin.ConfigParam{
			"dir":  {Default: "/data"},
			"name": {Required: true},
		},
	}
	got, err := spec.ResolvedConfig(map[string]string{"name": "ep", "name_typo": "typo", "auth_key": "x"})
	if err == nil {
		t.Fatalf("ResolvedConfig = %v, nil; want an error for the undeclared keys", got)
	}
	for _, want := range []string{"auth_key, name_typo", "it accepts: dir, name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	none := &plugin.PluginSpec{Name: "q"}
	if _, err := none.ResolvedConfig(map[string]string{"x": "1"}); err == nil || !strings.Contains(err.Error(), "declares no config parameters") {
		t.Errorf("spec with no config: err = %v, want it to say it declares none", err)
	}
	if got, err := none.ResolvedConfig(nil); err != nil || len(got) != 0 {
		t.Errorf("spec with no config, no input: = %v, %v; want empty, nil", got, err)
	}
}
