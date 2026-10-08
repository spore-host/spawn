package cmd

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestDetachedSweepSeedsEveryKeyTheLambdaReads is the gate on #749.
//
// The orchestrator Lambda never sees a LaunchConfig: it reads param keys and
// builds RunInstances from them. Nothing links the keys it READS to the keys the
// CLI WRITES — different modules, no shared type, no compile-time relationship —
// so the two sides disagreed silently for the whole life of the feature. `ami`
// was missing, EC2 rejected every row with MissingParameter, the Lambda logged
// success, and the CLI exited 0.
//
// Greps the Lambda source rather than trusting a hand-maintained list, because a
// hand-maintained list is exactly what failed.
func TestDetachedSweepSeedsEveryKeyTheLambdaReads(t *testing.T) {
	src, err := os.ReadFile("../lambda/sweep-orchestrator/main.go")
	if err != nil {
		t.Skipf("cannot read the orchestrator source: %v", err)
	}
	re := regexp.MustCompile(`get(?:String|Bool|Int)Param\(config, "([a-z_]+)"`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("parsed zero param keys from the orchestrator — the regex no longer matches, " +
			"which would make this gate silently vacuous")
	}

	declared := map[string]bool{}
	for _, k := range detachedParamKeys {
		declared[k] = true
	}

	var missing, extra []string
	for k := range found {
		if !declared[k] {
			missing = append(missing, k)
		}
	}
	for k := range declared {
		if !found[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("the orchestrator reads %v, which detachedParamKeys does not declare.\n"+
			"A key the Lambda reads and the CLI never writes becomes an empty string there — "+
			"which for `ami` meant RunInstances was rejected for every row while the sweep "+
			"reported success (#749).", missing)
	}
	if len(extra) > 0 {
		// Not fatal in itself, but it means the list has drifted from reality and
		// the next person cannot trust it.
		t.Errorf("detachedParamKeys declares %v, which the orchestrator does not read — "+
			"stale entries make the list untrustworthy", extra)
	}
}

// Every key the Lambda reads must actually be POPULATED by the seeder for an
// ordinary sweep. Declaring the key set is not the same as filling it in.
func TestSeedDetachedLaunchKeys_FillsTheLaunchCriticalKeys(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t4g.small"},
		Params:   []map[string]interface{}{{"trial": "a"}, {"trial": "b"}},
	}
	base := &aws.LaunchConfig{
		InstanceType:       "t4g.small",
		AMI:                "ami-explicit", // explicit --ami: no AWS call needed
		KeyName:            "my-key",
		IamInstanceProfile: "spawnd-role",
		Spot:               true,
	}

	if err := seedDetachedLaunchKeys(nil, nil, pf, base, "us-east-1"); err != nil {
		t.Fatalf("seedDetachedLaunchKeys: %v", err)
	}

	for key, want := range map[string]interface{}{
		"ami":           "ami-explicit",
		"key_name":      "my-key",
		"iam_role":      "spawnd-role",
		"region":        "us-east-1",
		"instance_type": "t4g.small",
		"spot":          true,
	} {
		if got := pf.Defaults[key]; got != want {
			t.Errorf("Defaults[%q] = %v, want %v", key, got, want)
		}
	}
}

// A user's own value must never be overwritten — the established sweep
// precedence is a row's params: > the CLI flag > the file's defaults:.
func TestSeedDetachedLaunchKeys_RespectsPrecedence(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{
			"instance_type": "t4g.small",
			"ami":           "ami-from-file",
			"key_name":      "key-from-file",
		},
		Params: []map[string]interface{}{
			{"trial": "a"},
			{"trial": "b", "ami": "ami-row-specific"},
		},
	}
	base := &aws.LaunchConfig{InstanceType: "c7g.large", AMI: "ami-from-flag", KeyName: "key-from-flag"}

	if err := seedDetachedLaunchKeys(nil, nil, pf, base, "us-east-1"); err != nil {
		t.Fatalf("seedDetachedLaunchKeys: %v", err)
	}
	if got := pf.Defaults["ami"]; got != "ami-from-file" {
		t.Errorf("Defaults[ami] = %v; the file's own default must beat the CLI flag", got)
	}
	if got := pf.Defaults["key_name"]; got != "key-from-file" {
		t.Errorf("Defaults[key_name] = %v; the file's own default must win", got)
	}
	if got := pf.Params[1]["ami"]; got != "ami-row-specific" {
		t.Errorf("Params[1][ami] = %v; a row's own key must win", got)
	}
}

// An unset flag must leave the key ABSENT rather than writing an empty string.
// RunInstances rejects an empty KeyName outright ("Invalid value ” for
// keyPairNames"), and the Lambda has its own iam_role fallback that an empty
// string would defeat.
func TestSeedDetachedLaunchKeys_DoesNotWriteEmptyValues(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t4g.small"},
		Params:   []map[string]interface{}{{"trial": "a"}},
	}
	base := &aws.LaunchConfig{InstanceType: "t4g.small", AMI: "ami-x"} // no key, no profile, no spot

	if err := seedDetachedLaunchKeys(nil, nil, pf, base, "us-east-1"); err != nil {
		t.Fatalf("seedDetachedLaunchKeys: %v", err)
	}
	for _, key := range []string{"key_name", "iam_role", "spot"} {
		if v, present := pf.Defaults[key]; present {
			t.Errorf("Defaults[%q] = %#v was written for an unset flag; absence is not the "+
				"same as empty, and an empty KeyName is rejected by RunInstances", key, v)
		}
	}
}

// With no instance type anywhere, no architecture can be inferred, so no AMI can
// be resolved. That must be an error rather than an unresolved row — an
// unresolved row is #749.
func TestSeedDetachedLaunchKeys_NoInstanceTypeIsAnError(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{},
		Params:   []map[string]interface{}{{"trial": "a"}},
	}
	err := seedDetachedLaunchKeys(nil, nil, pf, &aws.LaunchConfig{}, "us-east-1")
	if err == nil {
		t.Fatal("no error for a row with no instance type; it would upload with no ami")
	}
	if !strings.Contains(err.Error(), "instance_type") {
		t.Errorf("error does not name the missing key: %v", err)
	}
}
