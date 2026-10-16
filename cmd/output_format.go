package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// The values the global -o/--output accepts.
//
// outputFormatTable is the default and has no reader of its own: every command
// renders its table when the format is not one it recognises. That fall-through
// is why an unvalidated flag was silent — `-o jsno` or `-o results.csv` printed
// the table and exited 0 (#975).
//
// outputFormats is not the source of truth on its own. TestOutputFormatsMatchTheirReaders
// parses cmd/*.go, discovers every literal compared against getOutputFormat()
// or spawnOutputFormat, and fails when this slice accepts a format nothing
// renders or a reader compares against one this slice would reject.
const (
	outputFormatTable = "table"
	outputFormatJSON  = "json"
)

var outputFormats = []string{outputFormatTable, outputFormatJSON}

// outputFormatsAnnotation lets a command that renders formats beyond
// outputFormats accept them too, as a comma-separated list in
// cmd.Annotations[outputFormatsAnnotation]. Only that command accepts them;
// everywhere else they are rejected like any other unknown value.
const outputFormatsAnnotation = "spawn:output-formats"

// supportedOutputFormats returns the --output values cmd accepts.
func supportedOutputFormats(cmd *cobra.Command) []string {
	formats := slices.Clone(outputFormats)
	for _, f := range strings.Split(cmd.Annotations[outputFormatsAnnotation], ",") {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(formats, f) {
			formats = append(formats, f)
		}
	}
	return formats
}

// validateOutputFormat rejects a global --output value cmd does not render.
//
// It only judges the root's format flag. A command that declares its own local
// --output (a file or directory, as `collect-results` and the deprecated
// queue/slurm/pipeline aliases do) shadows the root flag, and its value is not
// a format, so it is left alone.
func validateOutputFormat(cmd *cobra.Command) error {
	f := cmd.Flags().Lookup("output")
	if f == nil || f != rootCmd.PersistentFlags().Lookup("output") {
		return nil
	}
	value := f.Value.String()
	formats := supportedOutputFormats(cmd)
	if slices.Contains(formats, value) {
		return nil
	}
	msg := fmt.Sprintf("invalid --output %q: -o/--output is the output format, one of: %s",
		value, strings.Join(formats, ", "))
	switch {
	case cmd.Flags().Lookup("output-file") != nil:
		msg += fmt.Sprintf(" (to write to a file, use --output-file %s)", value)
	case cmd.Flags().Lookup("output-dir") != nil:
		msg += fmt.Sprintf(" (to choose a directory, use --output-dir %s)", value)
	}
	return errors.New(msg)
}
