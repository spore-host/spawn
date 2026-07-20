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

// sha256HexRe matches a bare SHA-256 digest: exactly 64 lowercase hex chars.
var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

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

	// permissions (optional): if declared, its env names must be valid, its ports
	// in range, and — since local.env_passthrough is what spawn actually injects —
	// controller.env must not understate it (every passed-through var must be
	// declared). This keeps the declaration honest rather than decorative.
	if s.Permissions != nil {
		declaredEnv := map[string]bool{}
		for _, name := range s.Permissions.Controller.Env {
			if !envVarNameRe.MatchString(name) {
				add("permissions.controller.env: invalid environment variable name %q", name)
			}
			declaredEnv[name] = true
		}
		for _, name := range s.Local.EnvPassthrough {
			if envVarNameRe.MatchString(name) && !declaredEnv[name] {
				add("permissions.controller.env must include %q (it is in local.env_passthrough)", name)
			}
		}
		for _, p := range s.Permissions.Instance.Ports {
			if p < 1 || p > 65535 {
				add("permissions.instance.ports: %d out of range (1-65535)", p)
			}
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
			// sha256 is a fetch-only integrity field; when set, it must be a
			// bare 64-char lowercase hex digest, and it's meaningless on any
			// other step type.
			if st.SHA256 != "" {
				if st.Type != "fetch" {
					add("%s[%d]: sha256 is only valid on a fetch step, not %q", label, i, st.Type)
				} else if !sha256HexRe.MatchString(st.SHA256) {
					add("%s[%d]: invalid sha256 %q (want 64 lowercase hex chars)", label, i, st.SHA256)
				}
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

// remoteRunsAsRoot reports whether a remote step executes as root on the
// instance. spored runs every remote step as root EXCEPT a "run" step that opts
// into as_user (fetch/extract always run as root). Mirrors remote_executor.go.
func remoteRunsAsRoot(st Step) bool {
	if st.Type == "run" {
		return !st.AsUser
	}
	return true // fetch / extract
}

// ValidatePermissionConsistency performs STRICTER, publish-time checks that the
// author-declared permissions: block is consistent with the plugin's actual
// steps — so the declaration surfaced by `spawn plugin inspect` can be trusted,
// not just decorative. It is separate from Validate (which is always-on and
// intentionally lenient) because these checks only make sense at publish time
// for the official registry; a plugin without a permissions: block is skipped.
//
// Checks:
//   - instance.root=false must have NO remote step that runs as root (every
//     remote run step must be as_user, and there must be no fetch/extract step,
//     which always run as root);
//   - instance.network=false must have no fetch step (a transitive download);
//   - controller.network=false must have no local step with a fetch/URL.
func (s *PluginSpec) ValidatePermissionConsistency() error {
	if s.Permissions == nil {
		return &ValidationError{Problems: []string{"permissions: block is required for publish-time consistency checks"}}
	}
	var probs []string
	add := func(format string, args ...interface{}) {
		probs = append(probs, fmt.Sprintf(format, args...))
	}

	remoteGroups := []struct {
		label string
		steps []Step
	}{
		{"remote.install", s.Remote.Install},
		{"remote.configure", s.Remote.Configure},
		{"remote.start", s.Remote.Start},
		{"remote.stop", s.Remote.Stop},
		{"remote.health.steps", s.Remote.Health.Steps},
	}

	if !s.Permissions.Instance.Root {
		for _, g := range remoteGroups {
			for i, st := range g.steps {
				if remoteRunsAsRoot(st) {
					add("%s[%d]: runs as root but permissions.instance.root=false (a %q step needs as_user, and fetch/extract always run as root)", g.label, i, st.Type)
				}
			}
		}
	}

	if !s.Permissions.Instance.Network {
		for _, g := range remoteGroups {
			for i, st := range g.steps {
				if st.Type == "fetch" {
					add("%s[%d]: is a fetch (network download) but permissions.instance.network=false", g.label, i)
				}
			}
		}
	}

	if !s.Permissions.Controller.Network {
		for _, g := range []struct {
			label string
			steps []Step
		}{
			{"local.provision", s.Local.Provision},
			{"local.deprovision", s.Local.Deprovision},
			{"local.reconcile", s.Local.Reconcile},
		} {
			for i, st := range g.steps {
				if st.Type == "fetch" || st.URL != "" {
					add("%s[%d]: performs a network fetch but permissions.controller.network=false", g.label, i)
				}
			}
		}
	}

	if len(probs) > 0 {
		sort.Strings(probs)
		return &ValidationError{Problems: probs}
	}
	return nil
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

// ValidateSpecFileStrict runs ValidateSpecFile plus the publish-time permission/
// step consistency checks (ValidatePermissionConsistency). Used by the registry's
// lint CI (`spawn plugin validate --strict`). A plugin with no permissions: block
// passes the base validation but fails strict, so official plugins must declare
// their capability surface.
func ValidateSpecFileStrict(path string) error {
	spec, err := ParseSpecFile(path)
	if err != nil {
		return err
	}
	dirName := filepath.Base(filepath.Dir(path))
	if verr := spec.Validate(dirName); verr != nil {
		return verr
	}
	return spec.ValidatePermissionConsistency()
}
