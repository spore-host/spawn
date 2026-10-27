package security

import (
	"errors"
	"regexp"
	"strings"
)

// ShellQuote renders s as a single-quoted POSIX shell word, suppressing every
// expansion the shell would otherwise perform.
//
// Single quotes are the only POSIX construct inside which nothing is special, so
// this is what "shell-safe" actually requires. An embedded single quote is
// emitted as '\” — closing the quoted run, escaping the quote, reopening — which
// is the standard idiom and keeps the result a single word.
//
// This quotes one WORD. A command line (multiple words, with the user's own
// quoting and redirection) cannot be made safe this way and should be written to
// a file and executed with a shell instead; see the --mpi-command handling in
// pkg/userdata/mpi.go (spawn#660).
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
//
// No production caller, kept on purpose (#868): no log line found carries a
// credential. Note the second pattern masks ANY 40-character [A-Za-z0-9/+=] run,
// git commit SHAs included, so it is lossy on ordinary output; apply it to the
// specific field that may hold a key, not to whole messages.
func SanitizeForLog(s string) string {
	// Remove anything that looks like an AWS access key
	s = regexp.MustCompile(`AKIA[0-9A-Z]{16}`).ReplaceAllString(s, "AKIA****************")

	// Remove anything that looks like a secret key (40 characters of base64-like chars)
	s = regexp.MustCompile(`[A-Za-z0-9/+=]{40}`).ReplaceAllString(s, "****************************************")

	return s
}
