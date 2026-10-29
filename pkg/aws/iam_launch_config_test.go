package aws

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/taskiam"
)

// s3GetObjectAllows reports whether doc has an Allow statement granting
// s3:GetObject on arn, matching Resource globs as IAM does ('*' any run,
// '?' one character).
func s3GetObjectAllows(t *testing.T, doc, arn string) bool {
	t.Helper()
	var pol struct {
		Statement []struct {
			Effect   string
			Action   interface{}
			Resource interface{}
		}
	}
	if err := json.Unmarshal([]byte(doc), &pol); err != nil {
		t.Fatalf("policy is not JSON: %v", err)
	}
	for _, st := range pol.Statement {
		if st.Effect != "Allow" {
			continue
		}
		get := false
		for _, a := range stringList(st.Action) {
			if a == "s3:GetObject" || a == "s3:*" || a == "*" {
				get = true
			}
		}
		if !get {
			continue
		}
		for _, r := range stringList(st.Resource) {
			re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(r)) + "$"
			if regexp.MustCompile(re).MatchString(arn) {
				return true
			}
		}
	}
	return false
}

// #1359: every spored role can read a launch config object in its own
// account's staging buckets, and nothing else in them: not another account's
// object, and not a task's staged bootstrap (#1274), whose key is its secret.
// Both copies are checked, bound to an account as PutRolePolicy gets them:
// the shared spored-instance-role's policy and the baseline every
// spawn-instance-* role carries. Resource-level, because both also grant
// s3:GetObject elsewhere, so an action-level check passes without this one.
func TestSporedRolesReadTheirLaunchConfigObjectAndNothingElse(t *testing.T) {
	const acct = "111122223333"
	sha := strings.Repeat("a", 64)
	bucket := launchconfig.Bucket(acct, "eu-west-1")
	config := "arn:aws:s3:::" + bucket + "/" + launchconfig.Key(sha)
	bootstrap := "arn:aws:s3:::" + bucket + "/" + taskiam.StagedFilesKey("t1", sha)
	foreign := "arn:aws:s3:::" + launchconfig.Bucket("999999999999", "eu-west-1") + "/" + launchconfig.Key(sha)
	other := "arn:aws:s3:::" + bucket + "/launch-config/notes.txt"

	for name, tmpl := range map[string]string{
		"spored-instance-role": sporedDCVRolePolicy,
		"spored baseline":      (&Client{}).sporedBaselinePolicyDoc(),
	} {
		doc, err := bindPolicyAccount(tmpl, acct)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !s3GetObjectAllows(t, doc, config) {
			t.Errorf("%s cannot read its launch config object %s", name, config)
		}
		for _, arn := range []string{bootstrap, foreign, other} {
			if s3GetObjectAllows(t, doc, arn) {
				t.Errorf("%s can read %s", name, arn)
			}
		}
	}
}
