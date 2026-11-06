package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	paramparser "github.com/spore-host/spawn/pkg/params"
)

// inlineParamsSource names inline --params in messages that would otherwise
// name the param file, and in a checkpoint that has no file to name.
const inlineParamsSource = "--params (inline JSON)"

// sweepParamSource is the source a sweep's parameters were read from, as
// messages name it: the param file's path, or inline --params.
func sweepParamSource(file string) string {
	if file != "" {
		return file
	}
	return inlineParamsSource
}

// validateSweepParamFlags refuses --params with --param-file (#1540): each is
// a whole parameter document, and neither is merged into the other.
func validateSweepParamFlags(file, inline string) error {
	if file != "" && inline != "" {
		return fmt.Errorf("--params and --param-file cannot be used together: pass the parameters " +
			"inline with --params, or in a file with --param-file")
	}
	return nil
}

// loadSweepParams reads the sweep's parameters from --param-file or inline
// --params, whichever is set.
func loadSweepParams(file, inline string) (*ParamFileFormat, error) {
	if err := validateSweepParamFlags(file, inline); err != nil {
		return nil, err
	}
	switch {
	case file != "":
		pf, err := parseParamFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to parse parameter file: %w", err)
		}
		return pf, nil
	case inline != "":
		return parseInlineParams(inline)
	}
	return nil, fmt.Errorf("either --param-file or --params must be specified for parameter sweep")
}

// parseInlineParams parses the value of --params (#1540): a JSON document in
// the schema of a JSON param file (defaults, grid, params), read by the same
// parser. A value that is not JSON and looks like a path is the common
// copy-paste mistake of passing a file to --params; it is refused with the
// flag to use instead, and the file is never read.
func parseInlineParams(value string) (*ParamFileFormat, error) {
	if !json.Valid([]byte(value)) && looksLikeParamPath(value) {
		path := strings.TrimSpace(value)
		return nil, fmt.Errorf("--params takes inline JSON; to use the file %s, pass --param-file %s", path, path)
	}
	result, err := paramparser.ParseParamJSON([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("--params: %w", err)
	}
	return fromParserFormat(result), nil
}

// looksLikeParamPath reports whether a --params value names a file rather
// than holding JSON: an existing file, or a param-file extension whether or
// not the file exists. Only os.Stat is called; the file is not opened.
func looksLikeParamPath(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}
	switch strings.ToLower(filepath.Ext(v)) {
	case ".yaml", ".yml", ".json", ".csv":
		return true
	}
	fi, err := os.Stat(v)
	return err == nil && !fi.IsDir()
}

// inlineParamsFor is the inline --params document to checkpoint: set only
// when there is no param file to reload.
func inlineParamsFor(file, inline string) string {
	if file != "" {
		return ""
	}
	return inline
}
