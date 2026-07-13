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

// envVarNameRe matches a valid POSIX environment variable name (used to validate
// local.env_passthrough entries).
var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// semverRe matches an optional-"v" semantic version (major.minor.patch with
// optional pre-release/build), e.g. v1.2.0, 1.0.0, 1.2.3-rc1.
var semverRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)*$`)

// configRefRe finds canonical template references to config parameters:
// {{ config.key }} (the one supported form). Capture group 1 holds the key.
// The Go-style {{ .Config.key }} is deliberately NOT matched — it is not valid
// template syntax (see templateRefRe / the render engine) and is reported as an
// invalid reference rather than treated as a config reference.
var configRefRe = regexp.MustCompile(`{{-?\s*config\.([A-Za-z0-9_]+)`)

// templateRefRe matches any template action ({{ ... }}) so Validate can check
// each one is a canonical {{ namespace.key }} reference (or a control action
// like if/range/end). canonicalRefRe matches the accepted reference form.
var (
	templateRefRe  = regexp.MustCompile(`{{-?\s*(.*?)\s*-?}}`)
	canonicalRefRe = regexp.MustCompile(`^(instance|config|outputs|pushed)\.[A-Za-z0-9_]+$`)
	// controlKeywords are text/template actions that aren't references; specs
	// rarely use them, but don't flag them as invalid references if present.
	controlKeywords = map[string]bool{"if": true, "else": true, "end": true, "range": true, "with": true, "template": true, "block": true, "define": true}
)

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

	// local.env_passthrough names must be valid environment variable identifiers.
	for _, name := range s.Local.EnvPassthrough {
		if !envVarNameRe.MatchString(name) {
			add("local.env_passthrough: invalid environment variable name %q", name)
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
	checkSteps("local.reconcile", s.Local.Reconcile, validLocalStepTypes, "run or push")
	checkSteps("remote.install", s.Remote.Install, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.configure", s.Remote.Configure, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.start", s.Remote.Start, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.stop", s.Remote.Stop, validRemoteStepTypes, "run, fetch, or extract")
	checkSteps("remote.health.steps", s.Remote.Health.Steps, validRemoteStepTypes, "run, fetch, or extract")

	// Every template action must be a canonical {{ namespace.key }} reference
	// (namespace ∈ instance/config/outputs/pushed) or a control keyword. This
	// rejects the Go-style {{ .Config.x }} and other constructs that the render
	// engine can't evaluate — which otherwise slip through to a silent
	// "<no value>" at launch (spore-plugins template-syntax drift).
	for _, expr := range s.invalidTemplateRefs() {
		add("invalid template reference {{ %s }} — use {{ instance.<key> }}, {{ config.<key> }}, {{ outputs.<key> }}, or {{ pushed.<key> }}", expr)
	}

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

// templateTexts returns every string in the spec that may contain template
// actions: all step fields (across every lifecycle phase) and condition run
// commands. Shared by configReferences and invalidTemplateRefs so both scan the
// same surface.
func (s *PluginSpec) templateTexts() []string {
	var texts []string
	addStep := func(st Step) {
		texts = append(texts, st.Run, st.URL, st.Dest, st.Src, st.Value)
		for _, v := range st.Env {
			texts = append(texts, v)
		}
	}
	for _, group := range [][]Step{
		s.Local.Provision, s.Local.Deprovision, s.Local.Reconcile,
		s.Remote.Install, s.Remote.Configure, s.Remote.Start, s.Remote.Stop,
		s.Remote.Health.Steps,
	} {
		for _, st := range group {
			addStep(st)
		}
	}
	for _, c := range append(append([]Condition{}, s.Conditions.Local...), s.Conditions.Remote...) {
		texts = append(texts, c.Run)
	}
	return texts
}

// configReferences returns the sorted, de-duplicated set of config keys
// referenced via canonical {{ config.key }} templates across the spec.
func (s *PluginSpec) configReferences() []string {
	seen := map[string]bool{}
	for _, text := range s.templateTexts() {
		for _, m := range configRefRe.FindAllStringSubmatch(text, -1) {
			if m[1] != "" {
				seen[m[1]] = true
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// invalidTemplateRefs returns the sorted, de-duplicated set of template actions
// in the spec that are NOT a canonical {{ namespace.key }} reference and not a
// control keyword — e.g. the Go-style {{ .Config.x }}, an unknown namespace, or
// a malformed reference. These render to a silent "<no value>" (or an execute
// error) at launch, so Validate flags them offline.
func (s *PluginSpec) invalidTemplateRefs() []string {
	seen := map[string]bool{}
	for _, text := range s.templateTexts() {
		for _, m := range templateRefRe.FindAllStringSubmatch(text, -1) {
			expr := strings.TrimSpace(m[1])
			if expr == "" {
				continue
			}
			// Skip control actions (if/range/end/…) — keyword is the first token.
			if controlKeywords[strings.Fields(expr)[0]] {
				continue
			}
			if !canonicalRefRe.MatchString(expr) {
				seen[expr] = true
			}
		}
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
