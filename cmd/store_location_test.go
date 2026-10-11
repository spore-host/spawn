package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A reader must learn a store's location, not reconstruct it (#804).
//
// Two subsystems shared one defect: the writer resolved a bucket at runtime and
// the reader rebuilt the name from a convention, so the two agreed only by
// coincidence.
//
//   - Scheduled params: `resolveSchedulesBucket` prefers the shared
//     `spawn-schedules-<region>` and falls back to an account-scoped
//     `spawn-schedules-<account>-<region>` when the shared name is taken. It
//     returns which one it used; the caller discarded that, the record carried
//     only a key, and the handler rebuilt the name from the region. In any
//     account on the fallback path, every scheduled execution read a bucket it
//     had never written to.
//   - Queue results: `spawn queue results` derived `spawn-results-<region>`
//     directly under a comment saying it "should match what was specified in the
//     queue config", which it never loaded.
//
// Same principle as #790's Lambda-name constants: reconstructing an identifier
// from a convention is how the scheduler handler ended up asking for a function
// that never existed.

// The schedule record must carry the bucket, and the CLI must populate it.
func TestScheduleRecordPersistsTheResolvedBucket(t *testing.T) {
	root := repoRootFromCmd(t)

	types := readRepoSource(t, filepath.Join(root, "pkg", "scheduler", "types.go"))
	// The complete tag, not a prefix of it: `dynamodbav:"s3_params_bucket` also
	// matches `s3_params_bucket_unused`, so a rename passed this gate on the
	// first attempt. Rule 3 applies to the gate's own matcher.
	if !strings.Contains(types, `dynamodbav:"s3_params_bucket,omitempty"`) {
		t.Error("ScheduleRecord has no s3_params_bucket field, so the bucket the params were " +
			"actually written to is not persisted and the handler can only guess (#804)")
	}

	sched := readRepoSource(t, filepath.Join(root, "cmd", "schedule.go"))
	// The upload returns (bucket, key, size, sha, err). Discarding the first
	// value is the defect, so the gate is anchored on the assignment.
	if regexp.MustCompile(`_,\s*s3Key,\s*size,\s*sha256,\s*err\s*:?=\s*stagingClient\.UploadScheduleParams`).
		MatchString(sched) {
		t.Error("cmd/schedule.go still discards the bucket UploadScheduleParams resolved — that is " +
			"the #804 defect exactly")
	}
	if !strings.Contains(sched, "S3ParamsBucket:") {
		t.Error("cmd/schedule.go does not store S3ParamsBucket on the record")
	}
}

// The scheduler-handler must prefer the recorded bucket over the template.
func TestSchedulerHandlerPrefersTheRecordedBucket(t *testing.T) {
	root := repoRootFromCmd(t)
	src := readRepoSource(t, filepath.Join(root, "lambda", "scheduler-handler", "main.go"))

	if !strings.Contains(src, `dynamodbav:"s3_params_bucket,omitempty"`) {
		t.Error("the handler's ScheduleRecord does not unmarshal s3_params_bucket, so it cannot " +
			"learn where the params were written")
	}
	if !strings.Contains(src, "downloadParams(ctx, schedule.S3ParamsBucket") {
		t.Error("downloadParams is not passed the recorded bucket")
	}

	// No ordering assertion here. The first version compared the source positions
	// of `recordedBucket` and `schedulesBucketTmpl` inside downloadParams, and a
	// revert that computed the template first and then overrode it PASSED —
	// correctly, because computing-then-overriding is equally valid. The gate was
	// checking style, not mechanism (CLAUDE.md rule 6, a fourth time in this
	// audit). The decision is now a pure function, `paramsBucket`, with a
	// behavioural test beside it in the scheduler-handler module.
}

// `spawn queue results` must be able to be told where the results are.
func TestQueueResultsCanBeToldWhereTheResultsAre(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"queue", "results"})
	if err != nil {
		t.Fatalf("find queue results: %v", err)
	}
	for _, name := range []string{"config", "bucket"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("queue results has no --%s, so it can only guess the bucket from the region — "+
				"which is wrong for every queue not launched with the wizard default (#804)", name)
		}
	}
}

// An explicit bucket wins; a config is read; otherwise the guess is ANNOUNCED.
//
// The announcement is the load-bearing part: the failure mode of guessing wrong
// is an empty listing, which looks exactly like "the queue produced nothing".
func TestQueueResultsLocationPrecedence(t *testing.T) {
	reset := func() { queueResultsBucket, queueResultsConfig = "", "" }
	t.Cleanup(reset)

	t.Run("explicit bucket wins", func(t *testing.T) {
		reset()
		queueResultsBucket = "my-explicit-bucket"
		b, p, err := resolveQueueResultsLocation("q-1", "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		if b != "my-explicit-bucket" {
			t.Errorf("bucket = %q, want the explicit one", b)
		}
		if p != "queues/q-1/" {
			t.Errorf("prefix = %q", p)
		}
	})

	t.Run("config supplies bucket and prefix", func(t *testing.T) {
		reset()
		dir := t.TempDir()
		cfg := filepath.Join(dir, "queue.json")
		body, _ := json.Marshal(map[string]string{
			"result_s3_bucket": "from-config",
			"result_s3_prefix": "custom/place",
		})
		if werr := os.WriteFile(cfg, body, 0o600); werr != nil {
			t.Fatal(werr)
		}
		queueResultsConfig = cfg

		b, p, err := resolveQueueResultsLocation("q-1", "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		if b != "from-config" {
			t.Errorf("bucket = %q, want from-config", b)
		}
		if p != "custom/place/" {
			t.Errorf("prefix = %q, want custom/place/", p)
		}
	})

	t.Run("a config with no bucket is an error, not a silent guess", func(t *testing.T) {
		reset()
		dir := t.TempDir()
		cfg := filepath.Join(dir, "queue.json")
		if werr := os.WriteFile(cfg, []byte(`{"name":"x"}`), 0o600); werr != nil {
			t.Fatal(werr)
		}
		queueResultsConfig = cfg
		if _, _, err := resolveQueueResultsLocation("q-1", "us-east-1"); err == nil {
			t.Error("a config declaring no result_s3_bucket resolved successfully; it cannot say " +
				"where the results went, so falling back to a guess would hide that")
		}
	})

	t.Run("a missing config is an error", func(t *testing.T) {
		reset()
		queueResultsConfig = filepath.Join(t.TempDir(), "nope.json")
		if _, _, err := resolveQueueResultsLocation("q-1", "us-east-1"); err == nil {
			t.Error("a nonexistent --config resolved successfully")
		}
	})

	t.Run("the guess still works, for compatibility", func(t *testing.T) {
		reset()
		b, p, err := resolveQueueResultsLocation("q-1", "eu-west-1")
		if err != nil {
			t.Fatal(err)
		}
		if b != "spawn-results-eu-west-1" {
			t.Errorf("bucket = %q, want the historical region-derived guess", b)
		}
		if p != "queues/q-1/" {
			t.Errorf("prefix = %q", p)
		}
	})
}

// The guess must be announced in the source, since the gate above cannot read
// stderr from a pure function.
func TestQueueResultsGuessIsAnnounced(t *testing.T) {
	src := readRepoSource(t, filepath.Join(repoRootFromCmd(t), "cmd", "queue.go"))
	fn := strings.Index(src, "func resolveQueueResultsLocation(")
	if fn < 0 {
		t.Fatal("resolveQueueResultsLocation not found")
	}
	body := src[fn:]
	if !strings.Contains(body, "guessing") {
		t.Error("the region-derived fallback is not announced. Guessing wrong produces an EMPTY " +
			"listing, which is indistinguishable from the queue having produced nothing — so the " +
			"guess has to say it is guessing")
	}
}

func readRepoSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
