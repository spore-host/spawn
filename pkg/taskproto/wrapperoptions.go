package taskproto

import "errors"

// WrapperOptions carries the per-launch inputs the generated scripts need, for
// GenerateWrapper, GeneratePooledJobScript and GenerateFlushScript.
//
// This replaces four positional parameters (spawn#764, split from #679). The
// reason is not tidiness — it is that the positional form broke a consumer
// twice, and the second break was silent:
//
//	v0.111.0  GenerateWrapper(spec, resultsBucket, region string) string
//	v0.111.1  ... gpu bool) string                     ← PATCH. broke spore-host-mcp
//	v0.112.0  ... gpu bool, runID string) string       ← MINOR, compliant
//
// Both additions are "the caller must decide this per launch", so neither could
// be given a default. And both have a zero value that compiles and looks
// deliberate: the quickest way to make spore-host-mcp build again was
// `GenerateWrapper(spec, bucket, region, false, "")`, which passed every test
// while emitting an empty run_id and dropping `--gpus all`. That is spawn#608
// and spawn#606 reintroduced by a padding edit.
//
// A struct alone would make that WORSE, not better. With positional parameters a
// new argument at least forces a compile error at every call site, so somebody
// has to look; with a struct, an omitted field is silence. So the struct comes
// with Validate, and the generators return an error rather than a script when a
// required field is missing — trading a compile error for a loud runtime one at
// the point of misuse, instead of an unattributable S3 record discovered weeks
// later.
//
// The field that is NOT validated is Region, and deliberately: it is used only
// for the ECR login of a private-registry image, so a host-command task
// legitimately has none. Validating it would make the check cry wolf, which is
// how checks get worked around.
type WrapperOptions struct {
	// ResultsPrefix is the resolved S3 prefix the completion record and staged
	// outputs land under. Pass the output of EffectiveResultsPrefix rather than
	// deriving it, so a spec's results_prefix override reaches the wrapper, the
	// flush hook and the IAM policy from one place (spawn#646).
	ResultsPrefix string

	// RunID identifies THIS attempt (spawn#608). It is stamped into the completion
	// record as run_id so a launcher can tell its own run's record from one a
	// previous run of the same task_id left at the same S3 key.
	//
	// Caller-supplied, and NOT defaulted here even though an empty value is
	// always wrong. The caller needs the same id for the things it does around
	// the launch — clearing the previous attempt's record, and verifying run_id
	// when polling for completion — so an id minted privately in here would be
	// invisible to exactly the code that has to match it, which would defeat
	// #608's verification rather than protect it.
	RunID string

	// Region is used only for the ECR login of a private-registry container
	// image. Optional; see the note on Region above.
	Region string

	// GPU reports that the resolved instance is GPU-capable, so the container
	// runs with `--gpus all` and the host needs the NVIDIA Container Toolkit
	// (spawn#601/#606). Decide it from the SIZED instance type, not just
	// spec.Resources.GPUs, so a task sized onto a GPU box gets the GPU even when
	// the spec only asked via `families`.
	//
	// Read by GenerateWrapper and GeneratePooledJobScript. GenerateFlushScript
	// ignores it: the flush hook does not run the container.
	GPU bool
}

// ErrMissingRunID and ErrMissingResultsPrefix are returned by Validate. They are
// sentinels so a caller can distinguish "I built the options wrong" from a
// spec-validation failure, without matching on message text.
var (
	ErrMissingRunID         = errors.New("taskproto: WrapperOptions.RunID is empty; pass a freshly minted id per launch (spawn#608)")
	ErrMissingResultsPrefix = errors.New("taskproto: WrapperOptions.ResultsPrefix is empty; pass EffectiveResultsPrefix's result (spawn#646)")
)

// Validate reports whether the options can produce a correct script.
//
// Exported so a caller can check at config time rather than at generate time,
// and so the requirement is testable on its own.
func (o WrapperOptions) Validate() error {
	if o.ResultsPrefix == "" {
		return ErrMissingResultsPrefix
	}
	if o.RunID == "" {
		return ErrMissingRunID
	}
	return nil
}
