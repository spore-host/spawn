package plugin

import (
	"errors"
	"strings"
	"testing"
)

const testDigest = "4f9328f2bd646a7afa03d07ec20ec56641ce970839dee5af8803a276e4e8e524"

func fetchSpec(steps ...Step) *PluginSpec {
	return &PluginSpec{Name: "art", Version: "v1.0.0", Description: "d", Remote: RemoteBlock{Install: steps}}
}

// #838: a fetch step must pin its download. Validate (controller and the push
// API) and CheckArtifactPins (spored, every install path) refuse the same specs.
func TestArtifactPins(t *testing.T) {
	good := Step{Type: "fetch", URL: "https://example.com/a.deb", Dest: "/opt/a.deb", SHA256: testDigest}
	cases := []struct {
		name string
		step Step
		want string // "" = accepted; else a substring of the problem
	}{
		{"pinned", good, ""},
		{"pinned with size and arch", func() Step { s := good; s.Size = 12259980; s.Arch = "x86_64"; return s }(), ""},
		{"arch alias aarch64", func() Step { s := good; s.Arch = "aarch64"; return s }(), ""},
		{"no sha256", func() Step { s := good; s.SHA256 = ""; return s }(), "has no sha256"},
		{"uppercase sha256", func() Step { s := good; s.SHA256 = strings.ToUpper(testDigest); return s }(), "invalid sha256"},
		{"short sha256", func() Step { s := good; s.SHA256 = testDigest[:40]; return s }(), "invalid sha256"},
		{"negative size", func() Step { s := good; s.Size = -1; return s }(), "invalid size"},
		{"unknown arch", func() Step { s := good; s.Arch = "riscv64"; return s }(), "invalid arch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := fetchSpec(tc.step)
			verr := spec.Validate("")
			perr := CheckArtifactPins(spec)
			if tc.want == "" {
				if verr != nil || perr != nil {
					t.Fatalf("Validate = %v, CheckArtifactPins = %v; want both nil", verr, perr)
				}
				return
			}
			if verr == nil || !strings.Contains(verr.Error(), tc.want) {
				t.Errorf("Validate = %v, want a problem containing %q", verr, tc.want)
			}
			if !errors.Is(perr, ErrUnpinnedArtifact) || !strings.Contains(perr.Error(), tc.want) {
				t.Errorf("CheckArtifactPins = %v, want ErrUnpinnedArtifact containing %q", perr, tc.want)
			}
		})
	}
}

// The requirement covers every remote section, not only install: a health or
// start step that downloads is as much an artifact as an install step.
func TestArtifactPins_EveryRemoteSection(t *testing.T) {
	unpinned := Step{Type: "fetch", URL: "https://example.com/x", Dest: "/opt/x"}
	for _, set := range []func(*PluginSpec){
		func(s *PluginSpec) { s.Remote.Install = []Step{unpinned} },
		func(s *PluginSpec) { s.Remote.Configure = []Step{unpinned} },
		func(s *PluginSpec) { s.Remote.Start = []Step{unpinned} },
		func(s *PluginSpec) { s.Remote.Stop = []Step{unpinned} },
		func(s *PluginSpec) { s.Remote.Health.Steps = []Step{unpinned} },
	} {
		spec := &PluginSpec{Name: "art", Version: "v1.0.0", Description: "d"}
		set(spec)
		if err := CheckArtifactPins(spec); !errors.Is(err, ErrUnpinnedArtifact) {
			t.Errorf("CheckArtifactPins = %v, want ErrUnpinnedArtifact", err)
		}
	}
}

func TestStepRunsOnArch(t *testing.T) {
	cases := []struct {
		arch, host string
		want       bool
	}{
		{"", "amd64", true},
		{"", "arm64", true},
		{"x86_64", "amd64", true},
		{"amd64", "amd64", true},
		{"x86_64", "arm64", false},
		{"arm64", "arm64", true},
		{"aarch64", "arm64", true},
		{"arm64", "amd64", false},
		{"riscv64", "riscv64", false}, // an unknown arch never runs
		{"x86_64", "riscv64", false},
	}
	for _, tc := range cases {
		if got := StepRunsOnArch(Step{Type: "fetch", Arch: tc.arch}, tc.host); got != tc.want {
			t.Errorf("StepRunsOnArch(arch=%q, host=%q) = %v, want %v", tc.arch, tc.host, got, tc.want)
		}
	}
}
