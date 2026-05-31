package plugin

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Valid type sets, kept in sync with the executors and spec:
//   - remote step types: remote_executor.go runStep
//   - local step types:  local_executor.go RunProvision
//   - condition types:   local_executor.go checkCondition
//   - config param types: spec.go ConfigParam
var (
	validRemoteStepTypes = map[string]bool{"run": true, "fetch": true, "extract": true}
	validLocalStepTypes  = map[string]bool{"run": true, "push": true}
	validConditionTypes  = map[string]bool{"command": true, "platform": true}
	validConfigTypes     = map[string]bool{"": true, "string": true, "int": true, "bool": true}
)

// semverRe matches an optional-"v" semantic version (major.minor.patch with
// optional pre-release/build), e.g. v1.2.0, 1.0.0, 1.2.3-rc1.
var semverRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)*$`)

// configRefRe finds template references to config parameters in either
// supported form: {{ config.key }} / {{ config "key" }} and the Go-style
// {{ .Config.key }}. Capture group 1 or 2 holds the key.
var configRefRe = regexp.MustCompile(`{{-?\s*(?:config(?:\s+"|\.)([A-Za-z0-9_]+)|\.Config\.([A-Za-z0-9_]+))`)

// ValidationError aggregates all problems found in a spec so authors see every
// issue at once rather than one-per-run.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems:\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Validate performs static, offline checks on a parsed spec beyond what
// ParseSpec enforces: semver, known step/condition/config types, declared
// config references, and required-without-default sanity. It does not execute
// anything. dirName, when non-empty, is checked to equal spec.Name (the
// registry convention that a plugin's directory matches its name).
func (s *PluginSpec) Validate(dirName string) error {
	var probs []string
	add := func(format string, args ...interface{}) {
		probs = append(probs, fmt.Sprintf(format, args...))
	}

	// Name is already checked by ParseSpec, but Validate may run on a
	// hand-built spec, so re-check defensively.
	if s.Name == "" {
		add("missing required field: name")
	} else if !specNameRe.MatchString(s.Name) {
		add("invalid name %q (must match [a-zA-Z0-9_-]{1,64})", s.Name)
	}

	if dirName != "" && s.Name != "" && dirName != s.Name {
		add("directory %q does not match plugin name %q", dirName, s.Name)
	}

	if s.Version == "" {
		add("missing required field: version")
	} else if !semverRe.MatchString(s.Version) {
		add("invalid version %q (want semver, e.g. v1.2.0)", s.Version)
	}

	if strings.TrimSpace(s.Description) == "" {
		add("missing required field: description")
	}

	// Config params: known types, and a required param with a default is a
	// contradiction (the default makes it effectively optional).
	for key, p := range s.Config {
		if !validConfigTypes[p.Type] {
			add("config %q: invalid type %q (want string, int, or bool)", key, p.Type)
		}
		if p.Required && p.Default != nil {
			add("config %q: required and has a default — pick one", key)
		}
	}

	// Conditions.
	for i, c := range s.Conditions.Local {
		if !validConditionTypes[c.Type] {
			add("conditions.local[%d]: invalid type %q (want command or platform)", i, c.Type)
		}
	}
	for i, c := range s.Conditions.Remote {
		if !validConditionTypes[c.Type] {
			add("conditions.remote[%d]: invalid type %q (want command or platform)", i, c.Type)
		}
	}

	// Steps: remote vs local accept different type sets.
	checkSteps := func(label string, steps []Step, valid map[string]bool, want string) {
		for i, st := range steps {
			if !valid[st.Type] {
				add("%s[%d]: invalid step type %q (want %s)", label, i, st.Type, want)
			}
		}
	}
	checkSteps("local.provision", s.Local.Provision, validLocalStepTypes, "run or push")
	checkSteps("local.deprovision", s.Local.Deprovision, validLocalStepTypes, "run or push")
	checkSteps("remote.install", s.Remote.Install, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.configure", s.Remote.Configure, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.start", s.Remote.Start, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.stop", s.Remote.Stop, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.health.steps", s.Remote.Health.Steps, validRemoteStepTypes, "run, fetch, or extract")

	// Every {{ config.X }} reference must point at a declared config param.
	for _, ref := range s.configReferences() {
		if _, ok := s.Config[ref]; !ok {
			add("template references undeclared config %q", ref)
		}
	}

	if len(probs) > 0 {
		sort.Strings(probs)
		return &ValidationError{Problems: probs}
	}
	return nil
}

// configReferences returns the sorted, de-duplicated set of config keys
// referenced via templates across every step and condition in the spec.
func (s *PluginSpec) configReferences() []string {
	seen := map[string]bool{}
	scan := func(text string) {
		for _, m := range configRefRe.FindAllStringSubmatch(text, -1) {
			key := m[1]
			if key == "" {
				key = m[2]
			}
			if key != "" {
				seen[key] = true
			}
		}
	}
	scanStep := func(st Step) {
		scan(st.Run)
		scan(st.URL)
		scan(st.Dest)
		scan(st.Src)
		scan(st.Value)
		for _, v := range st.Env {
			scan(v)
		}
	}
	for _, group := range [][]Step{
		s.Local.Provision, s.Local.Deprovision,
		s.Remote.Install, s.Remote.Configure, s.Remote.Start, s.Remote.Stop,
		s.Remote.Health.Steps,
	} {
		for _, st := range group {
			scanStep(st)
		}
	}
	for _, c := range append(append([]Condition{}, s.Conditions.Local...), s.Conditions.Remote...) {
		scan(c.Run)
	}

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidateSpecFile parses and fully validates a plugin.yaml at path, inferring
// the expected plugin name from the containing directory (the registry layout
// plugins/<name>/plugin.yaml).
func ValidateSpecFile(path string) error {
	spec, err := ParseSpecFile(path)
	if err != nil {
		return err
	}
	dirName := filepath.Base(filepath.Dir(path))
	return spec.Validate(dirName)
}
