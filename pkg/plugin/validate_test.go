package plugin_test

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// validSpec is a minimal spec that should pass Validate with dirName "good".
const validSpec = `
name: good
version: v1.0.0
description: A valid plugin
config:
  auth_key:
    type: string
    required: true
remote:
  install:
    - type: run
      run: echo {{ config.auth_key }}
  health:
    interval: 10s
    steps:
      - type: run
        run: true
`

func TestValidate_Valid(t *testing.T) {
	spec, err := plugin.ParseSpec([]byte(validSpec))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if err := spec.Validate("good"); err != nil {
		t.Errorf("Validate: unexpected error: %v", err)
	}
}

func TestValidate_DirNameMismatch(t *testing.T) {
	spec, _ := plugin.ParseSpec([]byte(validSpec))
	err := spec.Validate("wrong-dir")
	if err == nil || !strings.Contains(err.Error(), "does not match plugin name") {
		t.Errorf("expected dir-mismatch error, got %v", err)
	}
}

func TestValidate_Problems(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantSub string
	}{
		{
			name:    "bad semver",
			spec:    "name: p\nversion: 1.x\ndescription: d\n",
			wantSub: "invalid version",
		},
		{
			name:    "missing description",
			spec:    "name: p\nversion: v1.0.0\n",
			wantSub: "missing required field: description",
		},
		{
			name:    "unknown remote step type",
			spec:    "name: p\nversion: v1.0.0\ndescription: d\nremote:\n  install:\n    - type: frobnicate\n      run: x\n",
			wantSub: "invalid step type",
		},
		{
			name:    "unknown condition type",
			spec:    "name: p\nversion: v1.0.0\ndescription: d\nconditions:\n  remote:\n    - type: phase-of-moon\n",
			wantSub: "invalid type",
		},
		{
			name:    "bad config type",
			spec:    "name: p\nversion: v1.0.0\ndescription: d\nconfig:\n  k:\n    type: float\n",
			wantSub: "invalid type",
		},
		{
			name:    "required with default",
			spec:    "name: p\nversion: v1.0.0\ndescription: d\nconfig:\n  k:\n    required: true\n    default: x\n",
			wantSub: "required and has a default",
		},
		{
			name:    "undeclared config ref",
			spec:    "name: p\nversion: v1.0.0\ndescription: d\nremote:\n  install:\n    - type: run\n      run: echo {{ config.nope }}\n",
			wantSub: "undeclared config",
		},
		{
			name:    "push only valid locally not remotely",
			spec:    "name: p\nversion: v1.0.0\ndescription: d\nremote:\n  install:\n    - type: push\n      key: k\n",
			wantSub: "invalid step type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := plugin.ParseSpec([]byte(tc.spec))
			if err != nil {
				// Some specs (e.g. missing name) fail at parse — that's fine.
				if strings.Contains(err.Error(), tc.wantSub) {
					return
				}
				t.Fatalf("ParseSpec: %v", err)
			}
			err = spec.Validate("p")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidate_GoStyleRefIsInvalid(t *testing.T) {
	// The Go-style {{ .Config.key }} form is NOT canonical syntax: the render
	// engine can't evaluate it (it would silently produce "<no value>"), so
	// Validate must reject it as an invalid template reference — the canonical
	// form is lowercase {{ config.key }}.
	spec, _ := plugin.ParseSpec([]byte(`
name: p
version: v1.0.0
description: d
remote:
  start:
    - type: run
      run: serve --name={{ .Config.undeclared }}
`))
	err := spec.Validate("p")
	if err == nil || !strings.Contains(err.Error(), "invalid template reference") {
		t.Errorf("expected invalid-template-reference error for .Config form, got %v", err)
	}
}

func TestValidate_CanonicalRefsAccepted(t *testing.T) {
	// The canonical {{ namespace.key }} forms validate cleanly.
	spec, _ := plugin.ParseSpec([]byte(`
name: p
version: v1.0.0
description: d
config:
  token:
    type: string
    required: true
remote:
  start:
    - type: run
      run: serve --name={{ instance.name }} --token={{ config.token }} --ip={{ instance.ip }}
`))
	if err := spec.Validate("p"); err != nil {
		t.Errorf("canonical references should validate cleanly, got: %v", err)
	}
}

func TestValidate_UnknownNamespaceRejected(t *testing.T) {
	spec, _ := plugin.ParseSpec([]byte(`
name: p
version: v1.0.0
description: d
remote:
  start:
    - type: run
      run: serve {{ bogus.x }}
`))
	err := spec.Validate("p")
	if err == nil || !strings.Contains(err.Error(), "invalid template reference") {
		t.Errorf("expected invalid-template-reference error for unknown namespace, got %v", err)
	}
}
