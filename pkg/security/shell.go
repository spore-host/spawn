package security

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ShellEscape escapes a string for safe use as a POSIX shell argument.
// It uses Go's strconv.Quote which handles all special shell characters.
func ShellEscape(s string) string {
	return strconv.Quote(s)
}

// ValidateUsername ensures username is safe for bash and follows POSIX conventions.
// Username must start with lowercase letter and contain only lowercase letters,
// digits, hyphens, and underscores. Maximum length is 32 characters.
func ValidateUsername(username string) error {
	if username == "" {
		return errors.New("username cannot be empty")
	}

	if len(username) > 32 {
		return errors.New("username exceeds maximum length of 32 characters")
	}

	matched, err := regexp.MatchString(`^[a-z][a-z0-9_-]{0,31}$`, username)
	if err != nil {
		return err
	}

	if !matched {
		return errors.New("invalid username format: must start with lowercase letter and contain only lowercase letters, digits, hyphens, and underscores")
	}

	return nil
}

// NormalizeUsername converts an arbitrary controller username (e.g. macOS
// "SFriedman", "john.doe", "Scott") into a valid POSIX login name that
// ValidateUsername accepts, so a fresh instance's local-matching user can be
// created instead of the launch failing. Rules: lowercase; any character that
// isn't [a-z0-9_-] becomes '-'; strip leading characters until it starts with a
// letter; collapse to <=32 chars; if nothing valid remains, fall back to
// "spore". The result is deterministic for a given input.
func NormalizeUsername(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))

	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()

	// Must start with a lowercase letter: drop any leading digits/_/-.
	out = strings.TrimLeft(out, "0123456789_-")
	// Trim trailing separators for a tidy name (e.g. "José" → "jos", not "jos-").
	out = strings.TrimRight(out, "_-")

	if len(out) > 32 {
		out = strings.TrimRight(out[:32], "_-")
	}
	if out == "" {
		return "spore"
	}
	return out
}

// ValidateBase64 ensures a string contains only valid base64 characters.
// This prevents injection attacks in base64-encoded data.
func ValidateBase64(s string) error {
	if s == "" {
		return errors.New("base64 string cannot be empty")
	}

	// Base64 alphabet: A-Z, a-z, 0-9, +, /, = (padding)
	matched, err := regexp.MatchString(`^[A-Za-z0-9+/=]+$`, s)
	if err != nil {
		return err
	}

	if !matched {
		return errors.New("invalid base64 format: contains non-base64 characters")
	}

	return nil
}

// SanitizeForLog removes or masks potentially sensitive information from log messages.
// This prevents credential leakage in logs.
func SanitizeForLog(s string) string {
	// Remove anything that looks like an AWS access key
	s = regexp.MustCompile(`AKIA[0-9A-Z]{16}`).ReplaceAllString(s, "AKIA****************")

	// Remove anything that looks like a secret key (40 characters of base64-like chars)
	s = regexp.MustCompile(`[A-Za-z0-9/+=]{40}`).ReplaceAllString(s, "****************************************")

	return s
}

// ValidateCommand ensures a command string doesn't contain shell injection attempts.
// Returns an error if suspicious patterns are detected.
func ValidateCommand(cmd string) error {
	// Check for common shell injection patterns
	dangerousPatterns := []string{
		";",  // Command separator
		"|",  // Pipe
		"&",  // Background/AND
		"$(", // Command substitution
		"`",  // Command substitution (backticks)
		"${", // Variable expansion
		"\n", // Newline
		"\r", // Carriage return
		"<",  // Input redirection
		">",  // Output redirection
		"\\", // Escape character
	}

	for _, pattern := range dangerousPatterns {
		if strings.Contains(cmd, pattern) {
			return errors.New("command contains potentially dangerous characters")
		}
	}

	return nil
}
