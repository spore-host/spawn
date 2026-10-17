package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// deploy-custom-dns.sh adds a zone to a DNS updater that may already serve
// others — spore.host's own, in the default configuration (#891). Two of its
// steps replace whole documents (`--environment`, put-role-policy), so each
// must be built from what is already deployed. Before #891 neither was:
// adding a zone made it the DEFAULT_DOMAIN and granted Route53 on it ALONE,
// which on a role this script created revoked every zone added before it.
//
// Executed, not string-matched: the test lifts the script's own merge function
// and zone computation out of the file and runs them with bash.

func scriptSection(t *testing.T, src string, start, end *regexp.Regexp) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if start.MatchString(l) {
			for j := i; j < len(lines); j++ {
				if end.MatchString(lines[j]) {
					return strings.Join(lines[i:j+1], "\n")
				}
			}
		}
	}
	t.Fatalf("section %s..%s not found in deploy-custom-dns.sh; the gate is reading the wrong code", start, end)
	return ""
}

func TestCustomDNSDeployIsAdditiveToADeployedUpdater(t *testing.T) {
	raw, err := os.ReadFile("deploy-custom-dns.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	merge := scriptSection(t, src, regexp.MustCompile(`^merge_lambda_env\(\) \{$`), regexp.MustCompile(`^}$`))
	zones := scriptSection(t, src, regexp.MustCompile(`^ZONE_ARNS=\$\(printf`), regexp.MustCompile(`'\)$`))

	// Shaped like the live spore.host updater.
	deployed := `{"DOMAIN_ZONES":"spore.host=ZSPOREHOST","DEFAULT_DOMAIN":"spore.host","HOSTED_ZONE_ID":"ZSPOREHOST","DNS_ALLOWED_ACCOUNTS":"111111111111,222222222222","DNS_AUTHZ_MODE":"enforce"}`
	envArgs := scriptSection(t, src, regexp.MustCompile(`^ENV_ARGS=\($`), regexp.MustCompile(`^done$`))
	harness := merge + "\n" +
		// The script's own inputs for this run: a new zone, from a new account.
		`DOMAIN=spore.example.edu; HOSTED_ZONE_ID=ZNEW; ALLOW_ACCOUNTS=(333333333333 111111111111)` + "\n" +
		envArgs + "\n" +
		`MERGED_ENV=$(printf '%s' "$1" | merge_lambda_env "${ENV_ARGS[@]}")` + "\n" +
		"info() { :; }\n" + zones + "\n" +
		`printf '%s\n%s\n' "$MERGED_ENV" "$ZONE_ARNS"`
	// The harness is this test's own text plus two sections of a committed script.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.Command("bash", "-c", harness, "_", deployed).CombinedOutput() //nolint:gosec // the test's own harness
	if err != nil {
		t.Fatalf("running the script's merge: %v\n%s", err, out)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected output:\n%s", out)
	}
	var env struct{ Variables map[string]string }
	if err := json.Unmarshal([]byte(parts[0]), &env); err != nil {
		t.Fatalf("merged env is not JSON: %v\n%s", err, parts[0])
	}
	v := env.Variables
	if v["DEFAULT_DOMAIN"] != "spore.host" || v["HOSTED_ZONE_ID"] != "ZSPOREHOST" {
		t.Errorf("adding a zone changed the default to (%q, %q); it must stay (spore.host, ZSPOREHOST)", v["DEFAULT_DOMAIN"], v["HOSTED_ZONE_ID"])
	}
	if v["DOMAIN_ZONES"] != "spore.example.edu=ZNEW,spore.host=ZSPOREHOST" {
		t.Errorf("DOMAIN_ZONES = %q, want both zones", v["DOMAIN_ZONES"])
	}
	if v["DNS_ALLOWED_ACCOUNTS"] != "111111111111,222222222222,333333333333" {
		t.Errorf("DNS_ALLOWED_ACCOUNTS = %q, want the deployed accounts kept and the new one added once", v["DNS_ALLOWED_ACCOUNTS"])
	}
	if v["DNS_AUTHZ_MODE"] != "enforce" {
		t.Errorf("DNS_AUTHZ_MODE = %q: a deploy turned enforcement off", v["DNS_AUTHZ_MODE"])
	}

	var arns []string
	if err := json.Unmarshal([]byte(parts[1]), &arns); err != nil {
		t.Fatalf("ZONE_ARNS is not a JSON list: %v\n%s", err, parts[1])
	}
	want := map[string]bool{"arn:aws:route53:::hostedzone/ZSPOREHOST": true, "arn:aws:route53:::hostedzone/ZNEW": true}
	if len(arns) != 2 || !want[arns[0]] || !want[arns[1]] {
		t.Errorf("Route53 grant covers %v; it must cover every served zone, or the deploy revokes the existing ones", arns)
	}

	// And the policy document must actually use that list.
	if !regexp.MustCompile(`(?m)^\s*"Resource": \$ZONE_ARNS\s*$`).MatchString(src) {
		t.Error("the Route53 statement's Resource is not $ZONE_ARNS")
	}
}

// The updater trusts requestContext.authorizer.iam.accountId. A direct
// invocation lets the caller write that field, so granting lambda:InvokeFunction
// to another account would let it register records as anyone. Only
// lambda:InvokeFunctionUrl, which AWS populates from the verified signature.
func TestCustomDNSDeployGrantsOnlyTheFunctionURL(t *testing.T) {
	raw, err := os.ReadFile("deploy-custom-dns.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := joinContinuations(string(raw))
	adds := regexp.MustCompile(`(?m)^\s*aws lambda add-permission\b.*$`).FindAllString(src, -1)
	if len(adds) == 0 {
		t.Fatal("no `aws lambda add-permission` found; the gate is reading the wrong code")
	}
	for _, a := range adds {
		if !strings.Contains(a, "--action lambda:InvokeFunctionUrl") || !strings.Contains(a, "--function-url-auth-type AWS_IAM") {
			t.Errorf("add-permission grants something other than InvokeFunctionUrl under AWS_IAM:\n  %s", a)
		}
	}
	if regexp.MustCompile(`(?m)^\s*aws lambda create-function-url-config\b.*--auth-type NONE`).MatchString(src) {
		t.Error("the script creates an unauthenticated Function URL")
	}
}

func joinContinuations(s string) string {
	return regexp.MustCompile(`\\\n\s*`).ReplaceAllString(s, " ")
}
