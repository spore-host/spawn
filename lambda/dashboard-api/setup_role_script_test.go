package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The cleanup half of scripts/setup-dashboard-lambda-role.sh (#1376), run
// end to end under a fake `aws` on the PATH that records every call and keeps
// just enough IAM state to behave like the real thing: delete-policy fails
// with DeleteConflict while a non-default version remains, as it did in
// production on 2026-10-28.

const roleScript = "scripts/setup-dashboard-lambda-role.sh"

// fakeAWS answers the calls the script makes. State lives in files under
// $FAKE_DIR: versions (the legacy policy's non-default versions still
// present), and the env vars set per case.
const fakeAWS = `#!/bin/bash
echo "$*" >> "$FAKE_DIR/calls"
svc=$1 op=$2; shift 2
query="" pname="" version=""
while [ $# -gt 0 ]; do
  case "$1" in
    --query) query=$2; shift ;;
    --policy-name) pname=$2; shift ;;
    --version-id) version=$2; shift ;;
  esac
  shift
done
case "$svc $op" in
  "sts get-caller-identity") echo 966362334030 ;;
  "iam create-role") echo arn:aws:iam::966362334030:role/SpawnDashboardLambdaRole ;;
  "lambda get-function-configuration") echo "${FAKE_SLACK_ARN:-None}" ;;
  "apigatewayv2 get-apis") echo abc123 ;;
  "iam list-attached-role-policies") echo "$FAKE_ATTACHED" ;;
  "iam get-policy")
    [ "$FAKE_ATTACHMENTS" = absent ] && { echo "NoSuchEntity" >&2; exit 254; }
    echo "$FAKE_ATTACHMENTS" ;;
  "iam list-policy-versions")
    # Only a query that filters on IsDefaultVersion gets the non-default set;
    # anything else also gets the default, v9, which must never be deleted.
    case "$query" in
      *IsDefaultVersion*) tr '\n' '\t' < "$FAKE_DIR/versions"; echo ;;
      *) { cat "$FAKE_DIR/versions"; echo v9; } | tr '\n' '\t'; echo ;;
    esac ;;
  "iam delete-policy-version")
    grep -qx "$version" "$FAKE_DIR/versions" || { echo "NoSuchEntity: $version" >&2; exit 254; }
    grep -vx "$version" "$FAKE_DIR/versions" > "$FAKE_DIR/v.tmp"; mv "$FAKE_DIR/v.tmp" "$FAKE_DIR/versions" ;;
  "iam delete-policy")
    [ -s "$FAKE_DIR/versions" ] && { echo "DeleteConflict: This policy has more than one version" >&2; exit 254; }
    : ;;
  "iam list-role-policies") echo "$FAKE_INLINE" ;;
  "iam get-role-policy")
    case "$query" in
      length*) echo 0 ;;
      *Action*)
        case "$pname" in
          DynamoDBStreamsAccess) printf 'dynamodb:DescribeStream\tdynamodb:GetRecords\ndynamodb:GetShardIterator\tdynamodb:ListStreams\n' ;;
          SlackSecretsAccess) echo "${FAKE_SLACK_ACTIONS:-secretsmanager:GetSecretValue}" ;;
          SporeSlackWorkspaceAccess) printf 'dynamodb:GetItem\tdynamodb:PutItem\tdynamodb:Scan\tdynamodb:UpdateItem\n' ;;
          *) echo "*" ;;
        esac ;;
      *) echo "{\"Statement\": \"document of $pname\"}" ;;
    esac ;;
esac
exit 0
`

type roleRun struct {
	calls []string
	out   string
}

// runRoleScript runs the committed script with the fake first on the PATH.
func runRoleScript(t *testing.T, env map[string]string, versions []string) roleRun {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fakeAWS), 0o755); err != nil { //nolint:gosec // an executable test fake
		t.Fatal(err)
	}
	v := ""
	if len(versions) > 0 {
		v = strings.Join(versions, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "versions"), []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", roleScript, "test-profile")
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_DIR="+dir,
		"SLACK_SECRETS_ARN=", "WS_API_ID=", "SPAWN_LAMBDA_ACCOUNT=")
	for k, val := range env {
		cmd.Env = append(cmd.Env, k+"="+val)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return roleRun{calls: strings.Split(strings.TrimSpace(string(raw)), "\n"), out: string(out)}
}

// invocations returns the indexes of calls to `aws <svc> <op>`.
func (r roleRun) invocations(svcOp string) []int {
	var idx []int
	for i, c := range r.calls {
		if strings.HasPrefix(c, svcOp+" ") {
			idx = append(idx, i)
		}
	}
	return idx
}

const legacyARN = "arn:aws:iam::966362334030:policy/SpawnDashboardLambdaPolicy"

func TestRoleScriptDeletesEveryNonDefaultVersionBeforeThePolicy(t *testing.T) {
	r := runRoleScript(t, map[string]string{
		"FAKE_ATTACHED":      legacyARN,
		"FAKE_ATTACHMENTS":   "0",
		"FAKE_INLINE":        "SpawnDashboardLambdaPolicy",
		"FAKE_SLACK_ARN":     "",
		"FAKE_SLACK_ACTIONS": "",
	}, []string{"v5", "v6", "v7", "v8"})

	del := r.invocations("iam delete-policy")
	if len(del) != 1 {
		t.Fatalf("delete-policy ran %d times, want once; calls:\n%s", len(del), strings.Join(r.calls, "\n"))
	}
	if !strings.Contains(r.calls[del[0]], "--policy-arn "+legacyARN) {
		t.Errorf("delete-policy targeted the wrong policy: %s", r.calls[del[0]])
	}
	seen := map[string]bool{}
	for _, i := range r.invocations("iam delete-policy-version") {
		if i > del[0] {
			t.Errorf("delete-policy-version ran after delete-policy: %s", r.calls[i])
		}
		f := strings.Fields(r.calls[i])
		seen[f[len(f)-1]] = true
		if !strings.Contains(r.calls[i], "--policy-arn "+legacyARN) {
			t.Errorf("deleted a version of the wrong policy: %s", r.calls[i])
		}
	}
	for _, v := range []string{"v5", "v6", "v7", "v8"} {
		if !seen[v] {
			t.Errorf("non-default version %s was not deleted", v)
		}
	}
	if seen["v9"] {
		t.Error("the default version v9 was deleted explicitly")
	}
	if !strings.Contains(r.out, "Deleted legacy managed policy") {
		t.Errorf("no deletion reported:\n%s", r.out)
	}
}

func TestRoleScriptDeletesNoLegacyPolicyStillAttachedElsewhere(t *testing.T) {
	for _, att := range []string{"1", "3", "absent"} {
		t.Run(att, func(t *testing.T) {
			r := runRoleScript(t, map[string]string{
				"FAKE_ATTACHED":    "",
				"FAKE_ATTACHMENTS": att,
				"FAKE_INLINE":      "SpawnDashboardLambdaPolicy",
			}, []string{"v5", "v6"})
			for _, op := range []string{"iam delete-policy-version", "iam delete-policy", "iam detach-role-policy"} {
				if n := len(r.invocations(op)); n != 0 {
					t.Errorf("with AttachmentCount %s, %s ran %d times", att, op, n)
				}
			}
		})
	}
}

var handAdded = []string{"DynamoDBStreamsAccess", "SlackSecretsAccess", "SporeSlackWorkspaceAccess"}

// deletedInline returns the policy names delete-role-policy removed.
func (r roleRun) deletedInline(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, i := range r.invocations("iam delete-role-policy") {
		f := strings.Fields(r.calls[i])
		for j := range f {
			if f[j] == "--policy-name" && j+1 < len(f) {
				names = append(names, f[j+1])
			}
		}
		if !strings.Contains(r.calls[i], "--role-name SpawnDashboardLambdaRole") {
			t.Errorf("delete-role-policy on another role: %s", r.calls[i])
		}
	}
	return names
}

func TestRoleScriptRemovesOnlyTheThreeHandAddedInlinePolicies(t *testing.T) {
	// Everything a role can carry: the script's own four, the three
	// hand-added ones, and two nobody here knows about, one of them a near-miss.
	inline := strings.Join([]string{
		"SpawnDashboardLambdaPolicy", "SpawnDashboardLambdaPolicySlack", "SpawnDashboardLambdaPolicyWebSocket",
		"DynamoDBStreamsAccess", "SlackSecretsAccess", "SporeSlackWorkspaceAccess",
		"SomeoneElsesPolicy", "DynamoDBStreamsAccessV2",
	}, "\t")
	base := map[string]string{
		"FAKE_ATTACHED": "", "FAKE_ATTACHMENTS": "absent", "FAKE_INLINE": inline,
		"FAKE_SLACK_ARN": "arn:aws:secretsmanager:us-east-1:966362334030:secret:slack-AbCdEf",
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}

	t.Run("all covered", func(t *testing.T) {
		r := runRoleScript(t, base, nil)
		got := r.deletedInline(t)
		if strings.Join(got, ",") != strings.Join(handAdded, ",") {
			t.Errorf("delete-role-policy removed %v, want exactly %v", got, handAdded)
		}
		for _, n := range handAdded {
			if !strings.Contains(r.out, "document of "+n) || !strings.Contains(r.out, "Removed hand-added inline policy "+n) {
				t.Errorf("%s: removal not printed with its document:\n%s", n, r.out)
			}
		}
	})
	t.Run("an action the script does not grant", func(t *testing.T) {
		r := runRoleScript(t, with("FAKE_SLACK_ACTIONS", "secretsmanager:GetSecretValue\tkms:Decrypt"), nil)
		got := r.deletedInline(t)
		if strings.Join(got, ",") != "DynamoDBStreamsAccess,SporeSlackWorkspaceAccess" {
			t.Errorf("removed %v; SlackSecretsAccess allows kms:Decrypt and must be kept", got)
		}
		if !strings.Contains(r.out, "Kept hand-added inline policy SlackSecretsAccess") {
			t.Errorf("keeping it was not reported:\n%s", r.out)
		}
	})
	t.Run("no Slack secret grant written", func(t *testing.T) {
		r := runRoleScript(t, with("FAKE_SLACK_ARN", ""), nil)
		for _, n := range r.deletedInline(t) {
			if n == "SlackSecretsAccess" {
				t.Error("removed SlackSecretsAccess though the script wrote no Slack secret grant to replace it")
			}
		}
	})
}
