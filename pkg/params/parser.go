package params

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParamFileFormat represents the parameter file structure
type ParamFileFormat struct {
	Defaults map[string]interface{} `json:"defaults" yaml:"defaults"`
	// Grid, when present, is expanded into the cartesian product of its named
	// value lists and appended to Params — so `grid: {lr: [...], bs: [...]}`
	// yields one param set per combination without the user pre-generating them.
	// Grid and an explicit Params list may be combined; the expansion is appended
	// after any explicit sets. Keys are iterated in sorted order so the generated
	// combinations (and thus sweep indexes) are deterministic across runs.
	Grid   map[string][]interface{} `json:"grid" yaml:"grid"`
	Params []map[string]interface{} `json:"params" yaml:"params"`
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

	if err := format.finalize(); err != nil {
		return nil, err
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

	if err := format.finalize(); err != nil {
		return nil, err
	}
	return &format, nil
}

// finalize expands any Grid into Params (cartesian product), ensures Defaults is
// non-nil, and enforces that the file yields at least one parameter set. Shared
// by the JSON and YAML parsers so both handle `grid:` identically. CSV has no
// grid concept and validates its own row count.
func (f *ParamFileFormat) finalize() error {
	if f.Defaults == nil {
		f.Defaults = make(map[string]interface{})
	}
	if len(f.Grid) > 0 {
		f.Params = append(f.Params, expandGrid(f.Grid)...)
	}
	if len(f.Params) == 0 {
		return fmt.Errorf("parameter file must contain at least one parameter set (a non-empty 'params' array or 'grid')")
	}
	return nil
}

// expandGrid returns the cartesian product of the named value lists as one
// parameter set per combination. Keys are processed in sorted order and each
// key's values in file order, so the resulting sequence — and therefore the
// sweep index assigned to each combination — is deterministic. A key with an
// empty value list contributes nothing and collapses the product to zero sets.
func expandGrid(grid map[string][]interface{}) []map[string]interface{} {
	keys := make([]string, 0, len(grid))
	for k := range grid {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	combos := []map[string]interface{}{{}}
	for _, k := range keys {
		values := grid[k]
		next := make([]map[string]interface{}, 0, len(combos)*len(values))
		for _, base := range combos {
			for _, v := range values {
				merged := make(map[string]interface{}, len(base)+1)
				for bk, bv := range base {
					merged[bk] = bv
				}
				merged[k] = v
				next = append(next, merged)
			}
		}
		combos = next
	}
	// A single empty combo means the grid had no keys — yield nothing.
	if len(combos) == 1 && len(combos[0]) == 0 {
		return nil
	}
	return combos
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
