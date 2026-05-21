package params

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseJSON(t *testing.T) {
	// Create temp JSON file
	tmpDir := t.TempDir()
	jsonFile := filepath.Join(tmpDir, "params.json")

	jsonContent := `{
  "defaults": {
    "region": "us-east-1",
    "ttl": "4h"
  },
  "params": [
    {"instance_type": "t3.micro", "alpha": 0.1},
    {"instance_type": "t3.small", "alpha": 0.2}
  ]
}`

	if err := os.WriteFile(jsonFile, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Parse
	result, err := ParseParamFile(jsonFile)
	if err != nil {
		t.Fatalf("ParseParamFile failed: %v", err)
	}

	// Verify defaults
	if result.Defaults["region"] != "us-east-1" {
		t.Errorf("Expected region 'us-east-1', got %v", result.Defaults["region"])
	}

	// Verify params
	if len(result.Params) != 2 {
		t.Fatalf("Expected 2 param sets, got %d", len(result.Params))
	}

	if result.Params[0]["instance_type"] != "t3.micro" {
		t.Errorf("Expected instance_type 't3.micro', got %v", result.Params[0]["instance_type"])
	}

	if result.Params[0]["alpha"] != 0.1 {
		t.Errorf("Expected alpha 0.1, got %v", result.Params[0]["alpha"])
	}
}

func TestParseYAML(t *testing.T) {
	// Create temp YAML file
	tmpDir := t.TempDir()
	yamlFile := filepath.Join(tmpDir, "params.yaml")

	yamlContent := `defaults:
  region: us-east-1
  ttl: 4h

params:
  - instance_type: t3.micro
    alpha: 0.1
  - instance_type: t3.small
    alpha: 0.2
`

	if err := os.WriteFile(yamlFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Parse
	result, err := ParseParamFile(yamlFile)
	if err != nil {
		t.Fatalf("ParseParamFile failed: %v", err)
	}

	// Verify defaults
	if result.Defaults["region"] != "us-east-1" {
		t.Errorf("Expected region 'us-east-1', got %v", result.Defaults["region"])
	}

	// Verify params
	if len(result.Params) != 2 {
		t.Fatalf("Expected 2 param sets, got %d", len(result.Params))
	}

	if result.Params[0]["instance_type"] != "t3.micro" {
		t.Errorf("Expected instance_type 't3.micro', got %v", result.Params[0]["instance_type"])
	}
}

func TestParseCSV(t *testing.T) {
	// Create temp CSV file
	tmpDir := t.TempDir()
	csvFile := filepath.Join(tmpDir, "params.csv")

	csvContent := `instance_type,alpha,beta,ttl
t3.micro,0.1,0.5,4h
t3.small,0.2,0.6,6h
t3.medium,0.3,0.7,8h
`

	if err := os.WriteFile(csvFile, []byte(csvContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Parse
	result, err := ParseParamFile(csvFile)
	if err != nil {
		t.Fatalf("ParseParamFile failed: %v", err)
	}

	// CSV doesn't have defaults section
	if len(result.Defaults) != 0 {
		t.Errorf("Expected empty defaults, got %d entries", len(result.Defaults))
	}

	// Verify params
	if len(result.Params) != 3 {
		t.Fatalf("Expected 3 param sets, got %d", len(result.Params))
	}

	// Check first row
	if result.Params[0]["instance_type"] != "t3.micro" {
		t.Errorf("Expected instance_type 't3.micro', got %v", result.Params[0]["instance_type"])
	}

	if result.Params[0]["alpha"] != 0.1 {
		t.Errorf("Expected alpha 0.1, got %v", result.Params[0]["alpha"])
	}

	if result.Params[0]["ttl"] != "4h" {
		t.Errorf("Expected ttl '4h', got %v", result.Params[0]["ttl"])
	}

	// Check second row
	if result.Params[1]["instance_type"] != "t3.small" {
		t.Errorf("Expected instance_type 't3.small', got %v", result.Params[1]["instance_type"])
	}

	if result.Params[1]["beta"] != 0.6 {
		t.Errorf("Expected beta 0.6, got %v", result.Params[1]["beta"])
	}
}

func TestParseCSV_BooleanValues(t *testing.T) {
	tmpDir := t.TempDir()
	csvFile := filepath.Join(tmpDir, "params.csv")

	csvContent := `instance_type,spot,hibernate
t3.micro,true,false
t3.small,false,true
`

	if err := os.WriteFile(csvFile, []byte(csvContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	result, err := ParseParamFile(csvFile)
	if err != nil {
		t.Fatalf("ParseParamFile failed: %v", err)
	}

	// Check boolean parsing
	if result.Params[0]["spot"] != true {
		t.Errorf("Expected spot=true, got %v", result.Params[0]["spot"])
	}

	if result.Params[0]["hibernate"] != false {
		t.Errorf("Expected hibernate=false, got %v", result.Params[0]["hibernate"])
	}

	if result.Params[1]["spot"] != false {
		t.Errorf("Expected spot=false, got %v", result.Params[1]["spot"])
	}

	if result.Params[1]["hibernate"] != true {
		t.Errorf("Expected hibernate=true, got %v", result.Params[1]["hibernate"])
	}
}

func TestParseCSV_EmptyValues(t *testing.T) {
	tmpDir := t.TempDir()
	csvFile := filepath.Join(tmpDir, "params.csv")

	csvContent := `instance_type,alpha,beta
t3.micro,0.1,
t3.small,,0.6
`

	if err := os.WriteFile(csvFile, []byte(csvContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	result, err := ParseParamFile(csvFile)
	if err != nil {
		t.Fatalf("ParseParamFile failed: %v", err)
	}

	// First row should not have beta
	if _, exists := result.Params[0]["beta"]; exists {
		t.Errorf("Expected beta to be omitted for empty value")
	}

	// Second row should not have alpha
	if _, exists := result.Params[1]["alpha"]; exists {
		t.Errorf("Expected alpha to be omitted for empty value")
	}
}

func TestParseValue(t *testing.T) {
	tests := []struct {
		input    string
		expected interface{}
		typeStr  string
	}{
		{"true", true, "bool"},
		{"false", false, "bool"},
		{"True", true, "bool"},
		{"FALSE", false, "bool"},
		{"42", int64(42), "int64"},
		{"3.14", 3.14, "float64"},
		{"hello", "hello", "string"},
		{"t3.micro", "t3.micro", "string"},
		{"4h", "4h", "string"},
	}

	for _, tt := range tests {
		result := parseValue(tt.input)
		if result != tt.expected {
			t.Errorf("parseValue(%q) = %v (type %T), expected %v (type %T)",
				tt.input, result, result, tt.expected, tt.expected)
		}
	}
}

func TestParseJSON_NoParams(t *testing.T) {
	tmpDir := t.TempDir()
	jsonFile := filepath.Join(tmpDir, "empty.json")

	jsonContent := `{"defaults": {"region": "us-east-1"}}`

	if err := os.WriteFile(jsonFile, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	_, err := ParseParamFile(jsonFile)
	if err == nil {
		t.Error("Expected error for file with no params, got nil")
	}
}

func TestParseCSV_HeaderMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	csvFile := filepath.Join(tmpDir, "mismatch.csv")

	csvContent := `instance_type,alpha
t3.micro,0.1,extra
`

	if err := os.WriteFile(csvFile, []byte(csvContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	_, err := ParseParamFile(csvFile)
	if err == nil {
		t.Error("Expected error for row with mismatched columns, got nil")
	}
}
