package cmd

import (
	"os"

	"golang.org/x/term"
)

// colorEnabled reports whether spawn may write ANSI colour/style escapes to
// stdout. It is the single place that decision is made: every command that
// colours output must ask it, rather than consulting --no-color on its own,
// because a check that only knows the flag is how `spawn list | cat` came to
// write raw escapes into pipes and files (#898).
func colorEnabled() bool {
	return colorAllowed(flagNoColor || flagAccessibility, os.Getenv, stdoutIsTerminal)
}

// colorAllowed is colorEnabled with its inputs injected so it is testable.
// Colour is off when any of these holds:
//   - --no-color or --accessibility was given;
//   - NO_COLOR is set to a non-empty value (https://no-color.org);
//   - TERM=dumb;
//   - stdout is not a terminal (a pipe, a file, a CI log).
func colorAllowed(disabledByFlag bool, getenv func(string) string, isTerminal func() bool) bool {
	if disabledByFlag {
		return false
	}
	if getenv("NO_COLOR") != "" {
		return false
	}
	if getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal()
}

// stdoutIsTerminal is a seam so tests never depend on how `go test` wires stdout.
var stdoutIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
