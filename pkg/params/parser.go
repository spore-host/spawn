package params

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParamFileFormat represents the parameter file structure
type ParamFileFormat struct {
	Defaults map[string]interface{}   `json:"defaults" yaml:"defaults"`
	Params   []map[string]interface{} `json:"params" yaml:"params"`
}

// ParseParamFile reads and parses a parameter file (JSON, YAML, or CSV)
func ParseParamFile(path string) (*ParamFileFormat, error) {
	// Detect format based on file extension
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".json":
		return parseJSON(path)
	case ".yaml", ".yml":
		return parseYAML(path)
	case ".csv":
		return parseCSV(path)
	default:
		// Try JSON as default
		return parseJSON(path)
	}
}

// parseJSON parses a JSON parameter file
func parseJSON(path string) (*ParamFileFormat, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read parameter file: %w", err)
	}

	var format ParamFileFormat
	if err := json.Unmarshal(data, &format); err != nil {
		return nil, fmt.Errorf("failed to parse JSON parameter file: %w", err)
	}

	if len(format.Params) == 0 {
		return nil, fmt.Errorf("parameter file must contain at least one parameter set in 'params' array")
	}

	// Ensure Defaults is not nil
	if format.Defaults == nil {
		format.Defaults = make(map[string]interface{})
	}

	return &format, nil
}

// parseYAML parses a YAML parameter file
func parseYAML(path string) (*ParamFileFormat, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read parameter file: %w", err)
	}

	var format ParamFileFormat
	if err := yaml.Unmarshal(data, &format); err != nil {
		return nil, fmt.Errorf("failed to parse YAML parameter file: %w", err)
	}

	if len(format.Params) == 0 {
		return nil, fmt.Errorf("parameter file must contain at least one parameter set in 'params' array")
	}

	// Ensure Defaults is not nil
	if format.Defaults == nil {
		format.Defaults = make(map[string]interface{})
	}

	return &format, nil
}

// parseCSV parses a CSV parameter file
// First row is header with column names
// Subsequent rows are parameter values
func parseCSV(path string) (*ParamFileFormat, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open CSV file: %w", err)
	}
	defer func() { _ = file.Close() }()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	// Read all records
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to read CSV file: %w", err)
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("CSV file is empty")
	}

	if len(records) < 2 {
		return nil, fmt.Errorf("CSV file must have at least a header row and one data row")
	}

	// First row is header
	headers := records[0]
	if len(headers) == 0 {
		return nil, fmt.Errorf("CSV header row is empty")
	}

	// Parse data rows
	params := make([]map[string]interface{}, 0, len(records)-1)
	for i, record := range records[1:] {
		if len(record) != len(headers) {
			return nil, fmt.Errorf("row %d has %d columns but header has %d", i+2, len(record), len(headers))
		}

		paramSet := make(map[string]interface{})
		for j, value := range record {
			if value == "" {
				continue // Skip empty values
			}

			// Try to parse as appropriate type
			parsedValue := parseValue(value)
			paramSet[headers[j]] = parsedValue
		}

		if len(paramSet) > 0 {
			params = append(params, paramSet)
		}
	}

	if len(params) == 0 {
		return nil, fmt.Errorf("no valid parameter sets found in CSV file")
	}

	return &ParamFileFormat{
		Defaults: make(map[string]interface{}),
		Params:   params,
	}, nil
}

// parseValue attempts to parse a string value into an appropriate type
func parseValue(value string) interface{} {
	value = strings.TrimSpace(value)

	// Try boolean
	if strings.ToLower(value) == "true" {
		return true
	}
	if strings.ToLower(value) == "false" {
		return false
	}

	// Try integer
	if intVal, err := strconv.ParseInt(value, 10, 64); err == nil {
		return intVal
	}

	// Try float
	if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
		return floatVal
	}

	// Default to string
	return value
}
