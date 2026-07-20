package plugin_test

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestValidatePermissionConsistency(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr string // "" = expect pass
	}{
		{
			name: "consistent: root=true with a root run step",
			spec: `name: p
version: v1.0.0
description: d
permissions:
  instance:
    root: true
    network: true
remote:
  install:
    - type: fetch
      url: https://example.com/x
      dest: /tmp/x
    - type: run
      run: install /tmp/x /usr/local/bin/x
`,
			wantErr: "",
		},
		{
			name: "root=false but a run step runs as root",
			spec: `name: p
version: v1.0.0
description: d
permissions:
  instance:
    root: false
remote:
  install:
    - type: run
      run: systemctl enable x
`,
			wantErr: "runs as root but permissions.instance.root=false",
		},
		{
			name: "root=false but a fetch step (always root)",
			spec: `name: p
version: v1.0.0
description: d
permissions:
  instance:
    root: false
    network: true
remote:
  install:
    - type: fetch
      url: https://example.com/x
      dest: /tmp/x
`,
			wantErr: "runs as root",
		},
		{
			name: "root=false consistent: only as_user run steps",
			spec: `name: p
version: v1.0.0
description: d
permissions:
  instance:
    root: false
remote:
  install:
    - type: run
      run: whoami
      as_user: true
`,
			wantErr: "",
		},
		{
			name: "instance.network=false but a fetch downloads",
			spec: `name: p
version: v1.0.0
description: d
permissions:
  instance:
    root: true
    network: false
remote:
  install:
    - type: fetch
      url: https://example.com/x
      dest: /tmp/x
`,
			wantErr: "permissions.instance.network=false",
		},
		{
			name: "controller.network=false but a local fetch",
			spec: `name: p
version: v1.0.0
description: d
permissions:
  controller:
    network: false
  instance:
    root: true
local:
  provision:
    - type: fetch
      url: https://example.com/x
      dest: /tmp/x
`,
			wantErr: "permissions.controller.network=false",
		},
		{
			name: "missing permissions block fails strict",
			spec: `name: p
version: v1.0.0
description: d
remote:
  install:
    - type: run
      run: "true"
`,
			wantErr: "permissions: block is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := plugin.ParseSpec([]byte(tc.spec))
			if err != nil {
				t.Fatalf("ParseSpec: %v", err)
			}
			err = spec.ValidatePermissionConsistency()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected consistency to pass, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
