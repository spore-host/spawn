package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Template represents a queue configuration template
type Template struct {
	Name        string
	Description string
	Config      *QueueConfig
	Variables   []TemplateVariable
}

// TemplateVariable represents a variable in a template
type TemplateVariable struct {
	Name        string
	Description string
	Default     string
	Required    bool
}

// getUserConfigDir returns the user's config directory for spawn templates
func getUserConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "spawn", "templates", "queue")
}

// LoadTemplate loads a template by name from templates/queue/<name>.json
// Searches in order: user config dir, embedded templates, file system
func LoadTemplate(name string) (*Template, error) {
	var data []byte
	var err error

	// 1. Try user config directory first (highest priority)
	userConfigPath := filepath.Join(getUserConfigDir(), name+".json")
	data, err = os.ReadFile(userConfigPath)
	if err == nil {
		return parseTemplate(name, data)
	}

	// 2. Try embedded templates
	embeddedPath := filepath.Join("templates", name+".json")
	data, err = embeddedTemplates.ReadFile(embeddedPath)
	if err == nil {
		return parseTemplate(name, data)
	}

	// 3. Fall back to file system (for development)
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("get executable path: %w", err)
	}

	exeDir := filepath.Dir(exe)
	templatePaths := []string{
		filepath.Join(exeDir, "templates", "queue", name+".json"),
		filepath.Join(exeDir, "..", "templates", "queue", name+".json"),
	}

	var lastErr error
	for _, templatePath := range templatePaths {
		data, err = os.ReadFile(templatePath)
		if err == nil {
			return parseTemplate(name, data)
		}
		lastErr = err
	}

	return nil, fmt.Errorf("template %s not found: %w", name, lastErr)
}

// parseTemplate parses template data and extracts variables
func parseTemplate(name string, data []byte) (*Template, error) {
	var config QueueConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	// Extract variables from template
	vars := extractVariables(&config)

	return &Template{
		Name:        name,
		Description: config.QueueName,
		Config:      &config,
		Variables:   vars,
	}, nil
}

// Substitute replaces variables in template with provided values
func (t *Template) Substitute(vars map[string]string) (*QueueConfig, error) {
	// Marshal to JSON for string manipulation
	data, err := json.Marshal(t.Config)
	if err != nil {
		return nil, fmt.Errorf("marshal template: %w", err)
	}

	configStr := string(data)

	// Regex: {{VAR}} or {{VAR:default}}
	re := regexp.MustCompile(`\{\{([A-Z_0-9]+)(?::([^}]+))?\}\}`)

	// Track missing required variables
	var missing []string

	configStr = re.ReplaceAllStringFunc(configStr, func(match string) string {
		parts := re.FindStringSubmatch(match)
		varName := parts[1]
		defaultVal := ""
		if len(parts) > 2 {
			defaultVal = parts[2]
		}

		// Check if variable provided
		if val, ok := vars[varName]; ok {
			return val
		}

		// Use default if available
		if defaultVal != "" {
			return defaultVal
		}

		// Mark as missing
		missing = append(missing, varName)
		return match // Leave unchanged for error reporting
	})

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required variables: %s", strings.Join(missing, ", "))
	}

	// Unmarshal back to QueueConfig
	var config QueueConfig
	if err := json.Unmarshal([]byte(configStr), &config); err != nil {
		return nil, fmt.Errorf("unmarshal substituted config: %w", err)
	}

	// Validate
	if err := ValidateQueue(&config); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	return &config, nil
}

// ListTemplates lists all available templates from all locations
// User templates override built-in templates with the same name
func ListTemplates() ([]*Template, error) {
	// Collect unique template names (user templates override built-in)
	templateNames := make(map[string]bool)

	// 1. Read embedded templates
	embeddedDir := "templates"
	entries, err := embeddedTemplates.ReadDir(embeddedDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".json")
			templateNames[name] = true
		}
	}

	// 2. Read user config directory (overrides embedded)
	userConfigDir := getUserConfigDir()
	userEntries, err := os.ReadDir(userConfigDir)
	if err == nil {
		for _, entry := range userEntries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".json")
			templateNames[name] = true
		}
	}

	// 3. Read file system directories (for development)
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		fsDirs := []string{
			filepath.Join(exeDir, "templates", "queue"),
			filepath.Join(exeDir, "..", "templates", "queue"),
		}

		for _, fsDir := range fsDirs {
			fsEntries, err := os.ReadDir(fsDir)
			if err != nil {
				continue
			}

			for _, entry := range fsEntries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				name := strings.TrimSuffix(entry.Name(), ".json")
				templateNames[name] = true
			}
		}
	}

	// Load all templates
	var templates []*Template
	for name := range templateNames {
		tmpl, err := LoadTemplate(name)
		if err != nil {
			// Skip templates that fail to load
			continue
		}
		templates = append(templates, tmpl)
	}

	// Sort by name for consistent output
	sort.Slice(templates, func(i, j int) bool {
		return templates[i].Name < templates[j].Name
	})

	return templates, nil
}

// extractVariables scans template for {{VAR}} patterns and extracts metadata
func extractVariables(config *QueueConfig) []TemplateVariable {
	re := regexp.MustCompile(`\{\{([A-Z_0-9]+)(?::([^}]+))?\}\}`)

	varMap := make(map[string]TemplateVariable)

	// Marshal to JSON and scan
	data, _ := json.Marshal(config)
	matches := re.FindAllStringSubmatch(string(data), -1)

	for _, match := range matches {
		varName := match[1]
		defaultVal := ""
		if len(match) > 2 {
			defaultVal = match[2]
		}

		if _, exists := varMap[varName]; !exists {
			varMap[varName] = TemplateVariable{
				Name:     varName,
				Default:  defaultVal,
				Required: defaultVal == "",
			}
		}
	}

	// Convert map to slice and sort
	var vars []TemplateVariable
	for _, v := range varMap {
		vars = append(vars, v)
	}

	sort.Slice(vars, func(i, j int) bool {
		// Sort required first, then by name
		if vars[i].Required != vars[j].Required {
			return vars[i].Required
		}
		return vars[i].Name < vars[j].Name
	})

	return vars
}
