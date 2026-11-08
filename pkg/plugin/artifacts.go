package plugin

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnpinnedArtifact is returned when a spec downloads an artifact that it does
// not pin by sha256 (#838). The release signature covers plugin.yaml only; the
// sha256 a fetch step carries is what extends that coverage to the bytes it
// downloads and spored then runs as root. Without it, a signed and verified spec
// could fetch whatever its URL served.
var ErrUnpinnedArtifact = errors.New("plugin downloads an artifact it does not pin by sha256")

// NormalizeArch maps an architecture name, as a spec's arch: field or a GOARCH
// value spells it, to its canonical form: "x86_64" or "arm64". The aliases are
// GOARCH's amd64 and uname -m's aarch64. ok is false for anything else.
func NormalizeArch(arch string) (canonical string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64", "amd64":
		return "x86_64", true
	case "arm64", "aarch64":
		return "arm64", true
	}
	return "", false
}

// StepRunsOnArch reports whether step runs on an instance whose architecture is
// hostArch (a GOARCH or uname -m value). A step with no arch: runs everywhere.
// A step whose arch: is not a known architecture never runs: Validate refuses
// such a spec, and the runtime must not guess which artifact it meant.
func StepRunsOnArch(st Step, hostArch string) bool {
	if st.Arch == "" {
		return true
	}
	want, ok := NormalizeArch(st.Arch)
	if !ok {
		return false
	}
	have, ok := NormalizeArch(hostArch)
	return ok && want == have
}

// remoteStepGroups pairs every remote step list with its label.
func (s *PluginSpec) remoteStepGroups() []struct {
	label string
	steps []Step
} {
	return []struct {
		label string
		steps []Step
	}{
		{"remote.install", s.Remote.Install},
		{"remote.configure", s.Remote.Configure},
		{"remote.start", s.Remote.Start},
		{"remote.stop", s.Remote.Stop},
		{"remote.health.steps", s.Remote.Health.Steps},
	}
}

// artifactPinProblems lists every remote fetch step that does not pin what it
// downloads: no sha256, a malformed sha256, a negative size, or an unknown
// arch. Validate reports these, and CheckArtifactPins (the runtime's check)
// refuses a spec that has any.
func (s *PluginSpec) artifactPinProblems() []string {
	var probs []string
	for _, g := range s.remoteStepGroups() {
		for i, st := range g.steps {
			if st.Type != "fetch" {
				continue
			}
			switch {
			case st.SHA256 == "":
				probs = append(probs, fmt.Sprintf("%s[%d]: fetch of %s has no sha256 — every downloaded artifact must be pinned (sha256sum the exact file and add sha256: <digest>)", g.label, i, st.URL))
			case !sha256HexRe.MatchString(st.SHA256):
				probs = append(probs, fmt.Sprintf("%s[%d]: invalid sha256 %q (want 64 lowercase hex chars)", g.label, i, st.SHA256))
			}
			if st.Size < 0 {
				probs = append(probs, fmt.Sprintf("%s[%d]: invalid size %d (want the artifact's byte length, or omit it)", g.label, i, st.Size))
			}
			if st.Arch != "" {
				if _, ok := NormalizeArch(st.Arch); !ok {
					probs = append(probs, fmt.Sprintf("%s[%d]: invalid arch %q (want x86_64 or arm64)", g.label, i, st.Arch))
				}
			}
		}
	}
	return probs
}

// CheckArtifactPins returns an error wrapping ErrUnpinnedArtifact when any
// remote fetch step in spec does not pin its download (see artifactPinProblems).
// spored calls it before running a single step of an install, whatever path the
// spec arrived by, so a spec that skipped Validate is refused all the same.
func CheckArtifactPins(spec *PluginSpec) error {
	probs := spec.artifactPinProblems()
	if len(probs) == 0 {
		return nil
	}
	return fmt.Errorf("plugin %s: %w: %s", spec.Name, ErrUnpinnedArtifact, strings.Join(probs, "; "))
}
