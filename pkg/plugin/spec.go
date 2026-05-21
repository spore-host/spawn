// Package plugin provides types and utilities for the spore-host plugin system.
package plugin

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// specNameRe matches valid plugin names: alphanumeric, dash, underscore; 1–64 chars.
var specNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// PluginSpec represents a parsed plugin.yaml specification.
type PluginSpec struct {
	Name        string                 `yaml:"name"`
	Version     string                 `yaml:"version"`
	Description string                 `yaml:"description"`
	Config      map[string]ConfigParam `yaml:"config"`
	Conditions  ConditionsBlock        `yaml:"conditions"`
	Local       LocalBlock             `yaml:"local"`
	Remote      RemoteBlock            `yaml:"remote"`
	Outputs     map[string]OutputSpec  `yaml:"outputs"`
}

// ConfigParam defines a plugin configuration parameter.
type ConfigParam struct {
	Default  interface{} `yaml:"default"`
	Required bool        `yaml:"required"`
	Type     string      `yaml:"type"` // string, int, bool
}

// ConditionsBlock holds pre-flight condition checks.
type ConditionsBlock struct {
	Local  []Condition `yaml:"local"`
	Remote []Condition `yaml:"remote"`
}

// Condition is a single pre-flight check.
type Condition struct {
	Type    string `yaml:"type"` // command, platform
	Run     string `yaml:"run"`
	OS      string `yaml:"os"`
	Message string `yaml:"message"`
}

// LocalBlock holds steps that run on the controller machine.
type LocalBlock struct {
	Provision   []Step `yaml:"provision"`
	Deprovision []Step `yaml:"deprovision"`
}

// RemoteBlock holds steps that run on the remote instance.
type RemoteBlock struct {
	Install   []Step      `yaml:"install"`
	Configure []Step      `yaml:"configure"`
	Start     []Step      `yaml:"start"`
	Stop      []Step      `yaml:"stop"`
	Health    HealthBlock `yaml:"health"`
}

// Step is a single executable action within a lifecycle phase.
type Step struct {
	Type       string            `yaml:"type"` // run, fetch, extract, push
	Run        string            `yaml:"run"`
	URL        string            `yaml:"url"`
	Dest       string            `yaml:"dest"`
	Src        string            `yaml:"src"`
	Key        string            `yaml:"key"`
	Value      string            `yaml:"value"`
	Background bool              `yaml:"background"`
	Capture    map[string]string `yaml:"capture"` // varname -> jmespath into stdout JSON
	Env        map[string]string `yaml:"env"`
}

// HealthBlock configures the remote health check loop.
type HealthBlock struct {
	Interval string `yaml:"interval"`
	Steps    []Step `yaml:"steps"`
}

// OutputSpec describes a plugin output value.
type OutputSpec struct {
	Source string `yaml:"source"` // local_capture, pushed
}

// Declaration represents a plugin reference with user-supplied config,
// used in launch configs and user-data to declare plugins at launch time.
type Declaration struct {
	Ref    string            `json:"ref" yaml:"ref"`
	Config map[string]string `json:"config,omitempty" yaml:"config,omitempty"`
}

// ParseSpec parses a plugin spec from YAML bytes.
func ParseSpec(data []byte) (*PluginSpec, error) {
	var spec PluginSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse plugin spec: %w", err)
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("plugin spec missing required field: name")
	}
	if !specNameRe.MatchString(spec.Name) {
		return nil, fmt.Errorf("plugin spec: invalid name %q (must match [a-zA-Z0-9_-]{1,64})", spec.Name)
	}
	return &spec, nil
}

// ParseSpecFile parses a plugin spec from a YAML file.
func ParseSpecFile(path string) (*PluginSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plugin spec %s: %w", path, err)
	}
	return ParseSpec(data)
}

// ResolvedConfig merges user-supplied config values with spec defaults,
// returning an error if any required parameter is missing.
func (s *PluginSpec) ResolvedConfig(userConfig map[string]string) (map[string]string, error) {
	result := make(map[string]string)

	for k, param := range s.Config {
		if v, ok := userConfig[k]; ok {
			result[k] = v
		} else if param.Default != nil {
			result[k] = fmt.Sprintf("%v", param.Default)
		} else if param.Required {
			return nil, fmt.Errorf("plugin %s: required config parameter %q not set", s.Name, k)
		}
	}

	// Pass through any extra user-supplied keys not declared in spec.
	for k, v := range userConfig {
		if _, ok := result[k]; !ok {
			result[k] = v
		}
	}

	return result, nil
}
