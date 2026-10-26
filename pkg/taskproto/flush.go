package taskproto

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// flushScriptPath is where the bootstrap writes the terminal-flush hook and
// where spored looks for it.
//
// Deliberately under /etc/spawn (root-owned, created by cloud-init) and NOT
// /tmp: spored runs as root, and executing a script out of a world-writable
// directory would let any local user choose what root runs. The existing /tmp
// channel (/tmp/SPAWN_COMPLETE) is only ever READ as data, which is a different
// risk class from exec.
//
// A variable only so tests can point it at a temp file; it is not settable from
// the environment, for the reason above.
var flushScriptPath = "/etc/spawn/task-flush.sh"

// FlushScriptPath returns the absolute path of the terminal-flush hook.
func FlushScriptPath() string { return flushScriptPath }

// SetFlushScriptPathForTest overrides the hook path for the rest of this test
// binary's run and returns a function restoring the previous value.
func SetFlushScriptPathForTest(path string) (restore func()) {
	prev := flushScriptPath
	flushScriptPath = path
	return func() { flushScriptPath = prev }
}

// FlushHookSeconds caps the hook when something other than spored runs it (the
// CLI's `spawn terminate`, the TTL reaper), matching spored's own 45 s bound.
const FlushHookSeconds = 45

// FlushCommand is the shell that runs the hook with reason why over SSM
// (AWS-RunShellScript, as root). It applies the same trust check spored's
// flushTaskRecord does before exec'ing the hook (a regular file, not a symlink,
// owned by the caller, not group- or world-writable) and caps it with
// timeout(1). A missing hook (a non-task launch, or a spored older than
// 0.116.0) is a clean no-op. Nothing from the instance's tags is interpolated:
// the path and why are this package's constants.
func FlushCommand(why ExitReason) string {
	h := FlushScriptPath()
	return fmt.Sprintf(`H=%s
if [ ! -e "$H" ]; then echo "spawn: no task flush hook on this instance"; exit 0; fi
if [ -L "$H" ] || [ ! -f "$H" ] || [ ! -O "$H" ] || [ -n "$(find "$H" -perm /022)" ]; then echo "spawn: refusing to run untrusted $H" >&2; exit 2; fi
exec timeout %d "$H" %s`, h, FlushHookSeconds, string(why))
}

// flushCommandLogPath is the command log the hook uploads: where the bootstrap
// tees the wrapper's output. A variable only so tests can give the hook a log
// to upload; nothing outside this package's tests writes it.
var flushCommandLogPath = "/var/log/spawn-command.log"

// FlushExitUploadFailed is the terminal-flush hook's exit status when it ran
// but at least one of its S3 uploads still failed after its retries (#1186).
// spored reports it as such; any other non-zero status means the hook itself
// broke.
const FlushExitUploadFailed = 3

// flushCp is how every hook upload starts. The connect and read timeouts are
// there because the aws CLI's defaults are 60 s each, so a single hung attempt
// could spend the hook's whole 45 s budget (pkg/agent taskFlushTimeout) and
// leave no time for the retries or for the record after the log.
const flushCp = "aws \"$AWS_REGION_ARG\" --cli-connect-timeout 5 --cli-read-timeout 15 s3 cp --no-progress"

// Retry bounds for spawn_up. Three attempts with 1 s and 2 s backoff, and no new
// attempt once the hook has run flushRetryBudget seconds, so retrying the log
// cannot starve the completion record that follows it inside the 45 s cap.
// The seconds spent staging outputs (spawn#1297) are added to that budget
// ($RB), so the record keeps the retries it had before outputs were staged.
const (
	flushUploadAttempts = 3
	flushRetryBudget    = 25
)

// writeFlushUploader emits spawn_up, which runs one upload command (its
// arguments) and retries it (#1186). Before this, each upload was tried once
// and a failure left only a line in spored's journal, which goes with the
// instance. On the last failure it prints the command and the aws CLI's own
// error text, which spored captures, and sets UPLOAD_FAILED so the hook exits
// FlushExitUploadFailed. An upload reading stdin ("-") has its input buffered
// first, because a retry would otherwise read an already-drained pipe and
// upload an empty object.
func writeFlushUploader(p func(string, ...interface{})) {
	p("UPLOAD_FAILED=0 RB=%d\n", flushRetryBudget)
	p("spawn_up() {\n")
	p("  local n=1 out in='' pipe=0 a\n")
	p("  for a; do [ \"$a\" = - ] && pipe=1; done\n")
	p("  [ \"$pipe\" = 1 ] && in=\"$(cat)\"\n")
	p("  while :; do\n")
	p("    if [ \"$pipe\" = 1 ]; then out=\"$(printf '%%s' \"$in\" | \"$@\" 2>&1)\"; else out=\"$(\"$@\" 2>&1)\"; fi && { [ -z \"$out\" ] || echo \"$out\"; return 0; }\n")
	p("    if [ \"$n\" -ge %d ] || [ \"$SECONDS\" -ge \"$RB\" ]; then\n", flushUploadAttempts)
	p("      echo \"spawn: upload failed after $n attempt(s): ${*: -1}: $out\" >&2; UPLOAD_FAILED=1; return 1\n")
	p("    fi\n")
	p("    sleep \"$n\"; n=$((n+1))\n")
	p("  done\n")
	p("}\n\n")
}

// GenerateFlushScript builds the terminal-flush hook for one task (spawn#632).
//
// The problem it solves: a task killed by its own TTL left NOTHING. Both the
// command.log upload (wrapper.go, "best-effort copy of the command log") and the
// completion.json heredoc sit AFTER the user command, in sequence, so they only
// ever run if the command returns. When a lifecycle limit fires mid-command the
// log dies with the instance's disk — and that log is exactly what you need,
// because spawn_phase() has already written timestamped stage-in/pull/command
// markers by then. A reporter lost a 45-minute run and had no way to tell how far
// it got, so re-sizing the TTL was another blind guess. This is distinct from the
// two already-fixed neighbours #566 (a command that FAILS writes no record) and
// #561 (a lost output recorded as success): both of those assume the command
// returns. Here it neither succeeded nor failed — it was still running.
//
// Why a spored-invoked hook rather than a trap in the wrapper. A trap on
// SIGTERM/EXIT only fires during OS shutdown, which races an S3 upload against
// network teardown — the worst possible moment to need the network. spored's
// runPreStop already runs SYNCHRONOUSLY, with a timeout, BEFORE
// provider.Terminate/Stop/Hibernate, i.e. while the instance is still fully
// healthy. Hanging the flush there also means it covers every exit path spored
// mediates — TTL, cost limit, idle-stop, hibernate and spot interruption — not
// just the TTL case that was reported.
//
// Everything the hook needs is baked in at generate time. It must not depend on
// the wrapper's live shell state, because it runs as a different process (and as
// root, not the instance user) while the wrapper is still blocked on the user's
// command.
//
// The reason token arrives as $1 and is a stable machine token from
// pkg/agent (ttl_expired, cost_limit_exceeded, …), never the human log string:
// rewording a log message must not silently change a recorded retry class.
// Takes the same WrapperOptions as GenerateWrapper so the three generators stay
// in step and the next per-launch input is a non-breaking addition for all of
// them (spawn#764). opts.GPU is ignored here: this hook does not run the
// container.
func GenerateFlushScript(spec *TaskSpec, opts WrapperOptions) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	resultsPrefix, region, runID := opts.ResultsPrefix, opts.Region, opts.RunID

	var b strings.Builder
	p := func(format string, a ...interface{}) { fmt.Fprintf(&b, format, a...) }

	taskID := spec.TaskID
	// Resolved by the caller (EffectiveResultsPrefix), same as the wrapper.

	p("#!/bin/bash\n")
	// Read by spored to size its timeout (FlushHookOutputSeconds), so it is
	// the second line, well inside the head spored reads.
	p("%s%d\n", flushOutputsHeader, flushOutputsBudget(spec))
	p("# spawn task terminal-flush hook — task_id=%s (generated by 'spawn task run', spawn#632)\n", taskID)
	p("# Run by spored on every lifecycle exit path, BEFORE the instance goes away.\n")
	p("# $1 is the exit-reason token (see pkg/agent exitReason).\n")
	// Not `set -e`: every step here is best-effort salvage. Aborting on the first
	// failure would be the same mistake #566 fixed in the wrapper — a partial
	// record beats none, and the whole point of this script is that it runs when
	// something has already gone wrong.
	p("set +e -u -o pipefail\n")
	// spored is a systemd service, so this inherits a minimal PATH rather than the
	// login PATH the wrapper enjoys — the system directories are APPENDED to make
	// sure `aws` is reachable. Appended, not substituted: replacing PATH outright
	// would drop any aws CLI installed outside these six directories, which is not
	// hypothetical (Ubuntu's snap package puts it in /snap/bin). Losing the AWS CLI
	// is losing the whole point of this hook.
	p("export PATH=\"${PATH:-}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\"\n\n")

	p("TASK_ID=%s\n", shQuote(taskID))
	p("RESULTS_PREFIX=%s\n", shQuote(resultsPrefix))
	p("AWS_REGION_ARG=%s\n", shQuote("--region="+region))
	p("REASON=\"${1:-unknown}\"\n\n")

	// One upload, retried (#1186). See writeFlushUploader.
	writeFlushUploader(p)

	// The interlock. Without this the hook would overwrite the completion record
	// of every task that finished normally: the success path is command returns ->
	// wrapper writes completion.json -> wrapper signals SPAWN_COMPLETE -> spored
	// acts on on_complete -> terminate() -> runPreStop() -> this script. Relabelling
	// a completed task "failed/ttl_expired" would be a far worse bug than the one
	// being fixed, so the guard comes first and never writes a record of its own.
	//
	// What it protects is the wrapper's record, not the wrapper's UPLOAD of it
	// (#1186). A local record whose S3 copy never landed — the upload failed, or
	// the limit fired between the wrapper's local write and its upload — used to
	// end here with exit 0, leaving the prefix empty. Now the hook uploads that
	// same local record: the wrapper's own content, so nothing is relabelled.
	//
	// head-object needs s3:GetObject, which a task role normally does not hold on
	// its results bucket (write-only), so in practice it answers 403 and the
	// local record is re-uploaded. That is safe: the bytes are the wrapper's own.
	// The one thing that is NOT safe is uploading a local copy a full /tmp left
	// empty or cut short (#1179) — the wrapper then uploaded from memory, and
	// this would overwrite that good object with a truncated one. So the local
	// copy is uploaded only when it is complete, i.e. ends with the record's
	// closing brace.
	rec := shQuote(CompletionRecordPath())
	logPath := shQuote(flushCommandLogPath)
	p("if [ -f %s ]; then\n", rec)
	if bucket, key, ok := SplitS3URI(resultsPrefix); ok {
		p("  if aws \"$AWS_REGION_ARG\" s3api head-object --bucket %s --key %s >/dev/null 2>&1; then\n",
			shQuote(bucket), shQuote(strings.TrimPrefix(key+"/completion.json", "/")))
		p("    echo \"spawn: task $TASK_ID already wrote its completion record; nothing to flush\"; exit 0\n")
		p("  fi\n")
	}
	p("  if [ \"$(tail -c 1 %s 2>/dev/null)\" != '}' ]; then\n", rec)
	p("    echo \"spawn: task $TASK_ID's local completion record is incomplete; not uploading it\" >&2; exit 0\n")
	p("  fi\n")
	p("  echo \"spawn: task $TASK_ID wrote its completion record, but it is not in S3; uploading it (reason: $REASON)\"\n")
	p("  [ -f %s ] && spawn_up %s %s \"$RESULTS_PREFIX/command.log\"\n", logPath, flushCp, logPath)
	p("  spawn_up %s %s \"$RESULTS_PREFIX/completion.json\"\n", flushCp, rec)
	p("  [ -f %s ] && spawn_up %s %s \"$RESULTS_PREFIX/.exitcode\"\n", shQuote(localPath(exitCodeFileName)), flushCp, shQuote(localPath(exitCodeFileName)))
	p("  [ \"$UPLOAD_FAILED\" = 0 ] || exit %d\n", FlushExitUploadFailed)
	p("  exit 0\n")
	p("fi\n\n")

	p("echo \"spawn: flushing a terminal record for task $TASK_ID (reason: $REASON)\"\n\n")

	// Reason -> retry class. Only the classes whose semantics actually match are
	// mapped; anything unrecognized gets an empty class ("unclassified terminal
	// state") rather than a guess, because Retryable() is read by adapters to
	// decide whether to spend money running the task again.
	p("case \"$REASON\" in\n")
	p("  %s) RETRY_CLASS=%s ;;\n", string(ExitTTLExpired), shQuote(string(RetryTTLExpired)))
	p("  %s) RETRY_CLASS=%s ;;\n", string(ExitSpotInterruption), shQuote(string(RetrySpotInterruption)))
	p("  *) RETRY_CLASS='' ;;\n")
	p("esac\n\n")

	// started_at comes from the file the wrapper persisted; an empty value is
	// honest about not knowing rather than inventing "now".
	p("STARTED_AT=\"$(cat %s 2>/dev/null)\"\n", shQuote(StartedAtPath()))
	p("ENDED_AT=\"$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)\"\n\n")
	// instance_type and purchase (spawn#1260): the record members the wrapper
	// built from IMDS, read back from its file rather than asked of IMDS again,
	// because this hook runs against a deadline. No file, no fields. The file is
	// the task user's to write, but so is the wrapper's own completion.json, so
	// reading it grants nothing new.
	p("INSTANCE_JSON=\"$(cat %s 2>/dev/null)\"\n\n", shQuote(instanceFactsPath()))

	// The log is the artifact that actually answers "how far did it get" — upload
	// it FIRST, before the record, so a hook that runs out of time still salvages
	// the more useful half.
	p("LOGS_JSON='[]'\n")
	p("if [ -f %s ]; then\n", logPath)
	p("  if spawn_up %s %s \"$RESULTS_PREFIX/command.log\"; then\n", flushCp, logPath)
	p("    LOGS_JSON=\"[\\\"$RESULTS_PREFIX/command.log\\\"]\"\n")
	p("  fi\n")
	p("else\n")
	p("  echo 'spawn: no %s to upload' >&2\n", flushCommandLogPath)
	p("fi\n\n")

	// The declared outputs (spawn#1297): after the log, which matters more,
	// and before the record, so the record can say what landed.
	writeFlushOutputs(p, spec)

	// state is "failed", NOT a new "terminated" value. CompletionRecord.State is
	// documented `completed | failed` and six workflow adapters parse this exact
	// object; a third value would send every one of them down a default branch.
	// "failed" is also true — the task did not produce its outputs. The WHY, which
	// is what the reporter actually needed, rides in retry_class and
	// terminal_reason, both of which are additive and ignorable.
	//
	// exit_code is -1 to mean "the process never returned a status", which is the
	// honest distinction from a real 0 or a real non-zero exit.
	//
	// Built in memory and written by writeRecordFile, exactly as the wrapper's
	// record is, so a full /tmp cannot turn it into an empty object (#1179).
	p("printf -v REC %s %s %s \"$STARTED_AT\" \"$ENDED_AT\" \"$INSTANCE_JSON\" \"$OUT_JSON\" \"$LOGS_JSON\" \"$RETRY_CLASS\" \"$REASON\"\n",
		shQuote(`{\n  "task_id": %s,\n  "run_id": %s,\n  "exit_code": -1,\n  "state": "`+string(StateFailed)+`",\n  "started_at": "%s",\n  "ended_at": "%s",\n  %s\n  %s\n  "logs": %s,\n  "retry_class": "%s",\n  "terminal_reason": "%s"\n}`),
		shQuote(jsonStr(taskID)), shQuote(jsonStr(runID)))
	writeRecordFile(p, "spawn_up "+flushCp, "UPLOAD_FAILED=1", "$REC", localPath(terminalFileName), "\"$RESULTS_PREFIX/completion.json\"")
	writeRecordFile(p, "spawn_up "+flushCp, "UPLOAD_FAILED=1", "-1", localPath(exitCodeFileName), "\"$RESULTS_PREFIX/.exitcode\"")

	// A failed upload is an exit status, not just a line on stderr (#1186):
	// spored turns a non-zero exit into a console message, a tag and a
	// notification, which is the only way a flush that did not reach S3 is ever
	// seen once the instance is gone.
	p("if [ \"$UPLOAD_FAILED\" != 0 ]; then echo \"spawn: terminal record for task $TASK_ID did NOT fully reach S3\" >&2; exit %d; fi\n", FlushExitUploadFailed)
	p("echo \"spawn: terminal record written for task $TASK_ID\"\n")
	p("exit 0\n")

	return b.String(), nil
}

// flushOutputsHeader starts the hook line declaring its output budget in
// seconds (spawn#1297). spored reads it to size the hook's timeout and to
// decide how many seconds to grant, so a hook with no outputs, or from before
// #1297, keeps the 45 s bound.
const flushOutputsHeader = "# spawn-flush-outputs-seconds: "

// FlushHookOutputSeconds returns the output budget a terminal-flush hook
// declares, given the hook's first bytes: 0 when it declares none, and never
// more than MaxFlushOutputsSeconds.
func FlushHookOutputSeconds(head []byte) int {
	sc := bufio.NewScanner(bytes.NewReader(head))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), flushOutputsHeader); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return 0
			}
			return min(n, MaxFlushOutputsSeconds)
		}
	}
	return 0
}

// flushOutputsBudget is the output budget the hook declares: the spec's, or 0
// when it declares no outputs.
func flushOutputsBudget(spec *TaskSpec) int {
	if len(spec.Outputs) == 0 {
		return 0
	}
	return spec.Lifecycle.FlushOutputsBudget()
}

// writeFlushOutputs emits the hook's stage-out of the declared outputs
// (spawn#1297) and sets OUT_JSON to the record members reporting it.
//
// Before this a lifecycle kill uploaded command.log and the record but no
// output, so a capped run's partial evidence (a tool's own log, its counters)
// went with the disk. Each output is copied as the wrapper copies it (aws s3
// cp, --recursive for a directory source), but untagged: provenance needs
// spored's state, and untagged means kept.
//
// Bounded twice. The seconds come from spored as $2, which it grants from the
// budget declared in the header line and shortens on a Spot interruption; the
// hook never takes more than the spec allows. With no $2 (run over SSM by
// `spawn terminate` or the reaper, whose waits leave no room, or by an older
// spored with a 45 s timeout) the budget is 0 and nothing is attempted. Each
// copy then runs under timeout(1) with what is left, so a large or hung upload
// is cut off at the deadline and the record still follows. Retried like
// spawn_up, but not through it: spawn_up runs its command in a command
// substitution, which loses timeout's 124, and a failed output must not fail
// the flush (exit 3), which is about the record.
func writeFlushOutputs(p func(string, ...interface{}), spec *TaskSpec) {
	p("OUT_JSON=''\n")
	budget := flushOutputsBudget(spec)
	if len(spec.Outputs) == 0 {
		return
	}
	if budget == 0 {
		p("OUT_JSON='\"outputs_staged\": \"%s\",'\n\n", OutputsDisabled)
		return
	}
	p("OB=0; case \"${2:-}\" in ''|*[!0-9]*) ;; *) OB=$2 ;; esac; [ \"$OB\" -le %d ] || OB=%d\n", budget, budget)
	p("T0=$SECONDS OE=$((SECONDS+OB)) N=0 OK=0 NS=''\n")
	p("spawn_out() {\n")
	p("  local j=$1 s=$2 d=$3 r=%s n=0 l; shift 3; N=$((N+1))\n", OutputMissing)
	p("  if [ -e \"$s\" ]; then while :; do\n")
	p("    l=$((OE-SECONDS)) r=%s; [ \"$l\" -gt 0 ] || break\n", OutputTimedOut)
	p("    timeout -k 2 \"$l\" %s \"$@\" \"$s\" \"$d\" && { OK=$((OK+1)); return 0; }\n", flushCp)
	p("    case $? in 124|137) break ;; esac\n")
	p("    r=%s n=$((n+1)); [ \"$n\" -lt %d ] && [ $((SECONDS+n)) -lt \"$OE\" ] || break; sleep \"$n\"\n", OutputFailed, flushUploadAttempts)
	p("  done; fi\n")
	p("  echo \"spawn: output $s not staged ($r)\" >&2; NS=\"$NS${NS:+, }{\\\"source\\\": $j, \\\"reason\\\": \\\"$r\\\"}\"\n")
	p("}\n")
	for _, m := range spec.Outputs {
		p("spawn_out %s %s %s%s\n", shQuote(jsonStr(m.Source)), shQuote(m.Source), shQuote(m.Destination), recursiveFlag(m.Source))
	}
	p("if [ \"$OK\" = \"$N\" ]; then OS=%s; elif [ \"$OK\" = 0 ]; then OS=%s; else OS=%s; fi\n", OutputsComplete, OutputsSkipped, OutputsPartial)
	p("OUT_JSON=\"\\\"outputs_staged\\\": \\\"$OS\\\",${NS:+ \\\"outputs_not_staged\\\": [$NS],}\"\n")
	p("RB=$((RB+SECONDS-T0))\n\n")
}
