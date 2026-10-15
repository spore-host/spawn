package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// helperEnv selects what this test binary does when the wrapper under test runs
// it as the "test binary": reach out the way a leaking test would, or not.
const helperEnv = "HERMETIC_TEST_HELPER"

func TestHelperProcess(t *testing.T) {
	ctx := context.Background()
	switch os.Getenv(helperEnv) {
	case "":
		t.Skip("helper for TestRun_*; does nothing on its own")
	case "quiet":
		// Ambient credentials must be gone and fakes in their place.
		if os.Getenv("AWS_PROFILE") != "" || os.Getenv("AWS_ACCESS_KEY_ID") != "hermetic-test-key-id" {
			os.Exit(3)
		}
	case "dns-api":
		// What pkg/dns does: a plain net/http POST to an HTTPS Function URL. Not an
		// SDK client, so AWS_ENDPOINT_URL alone would not have caught it.
		resp, err := http.Post("https://abc123.lambda-url.us-east-1.on.aws/", "application/json", strings.NewReader("{}"))
		if err == nil {
			_ = resp.Body.Close()
		}
	case "sdk":
		// What registry.NewPeerRegistry does: an SDK client from the ambient chain.
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-west-2"))
		if err != nil {
			os.Exit(4)
		}
		_, _ = dynamodb.NewFromConfig(cfg).PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("t"),
			Item:      map[string]types.AttributeValue{"k": &types.AttributeValueMemberS{Value: "v"}},
		})
	}
}

func runHelper(t *testing.T, mode string) (int, string) {
	t.Helper()
	t.Setenv(helperEnv, mode)
	t.Setenv("AWS_PROFILE", "would-be-real") // must not reach the child
	var out bytes.Buffer
	code := run(os.Args[0], []string{"-test.run=^TestHelperProcess$"}, &out, &out)
	return code, out.String()
}

// The gate must FAIL a package that reaches the network — for both shapes of
// leak #968 found, the SDK and the DNS API's plain HTTPS POST.
func TestRun_FailsATestBinaryThatReachesTheNetwork(t *testing.T) {
	for mode, want := range map[string]string{
		"dns-api": "HTTPS CONNECT abc123.lambda-url.us-east-1.on.aws:443",
		"sdk":     "target=DynamoDB_20120810.PutItem",
	} {
		t.Run(mode, func(t *testing.T) {
			code, out := runHelper(t, mode)
			if code != 1 {
				t.Errorf("exit = %d, want 1 for a binary that reached the network\n%s", code, out)
			}
			if !strings.Contains(out, "HERMETIC GATE FAILED") || !strings.Contains(out, want) {
				t.Errorf("failure does not name the request %q:\n%s", want, out)
			}
		})
	}
}

// ...and must not fail one that stays local, or it is noise, not a gate.
func TestRun_PassesAQuietTestBinary(t *testing.T) {
	if code, out := runHelper(t, "quiet"); code != 0 {
		t.Errorf("exit = %d, want 0 for a binary that made no request\n%s", code, out)
	}
}

// The wrapper only protects the runs that use it. Every `go test` CI runs that
// is not an e2e/integration build tag (those exist to reach real AWS) must go
// through it, and so must `make check` and the nested-module script.
func TestGateIsWired(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Discovered: every `run: go test` step in CI.
	ci := read(".github/workflows/ci.yml")
	runs := regexp.MustCompile(`(?m)^\s*run:\s*(go test\b.*)$`).FindAllStringSubmatch(ci, -1)
	if len(runs) == 0 {
		t.Fatal("found no `run: go test` step in ci.yml — the discovery is broken, not the workflow clean")
	}
	for _, m := range runs {
		if strings.Contains(m[1], "-tags=") {
			continue
		}
		if !strings.Contains(m[1], `-exec "$RUNNER_TEMP/hermetic-exec"`) {
			t.Errorf("ci.yml runs %q outside the hermetic wrapper (#968)", m[1])
		}
	}
	if !regexp.MustCompile(`(?m)^\s*run:\s*go build -o "\$RUNNER_TEMP/hermetic-exec" \./test/hermetic$`).MatchString(ci) {
		t.Error("ci.yml does not build the hermetic wrapper it runs tests under")
	}

	mk := read("Makefile")
	if !regexp.MustCompile(`(?m)^\tgo test -short -exec \$\(HERMETIC_EXEC\) \./\.\.\.$`).MatchString(mk) {
		t.Error("Makefile test-hermetic does not run the short tests under the wrapper")
	}
	check := regexp.MustCompile(`(?ms)^check:\n(.*?)\n\n`).FindStringSubmatch(mk)
	if check == nil {
		t.Fatal("Makefile has no check target")
	}
	if !regexp.MustCompile(`(?m)^\t@\$\(MAKE\) --no-print-directory test-hermetic$`).MatchString(check[1]) {
		t.Error("make check does not run test-hermetic")
	}
	if regexp.MustCompile(`(?m)^\t@?go test -short \./\.\.\.$`).MatchString(check[1]) {
		t.Error("make check still runs the short tests outside the wrapper")
	}

	nested := read("scripts/nested-modules.sh")
	if !regexp.MustCompile(`go test -short -exec "\$hermetic_exec" \./\.\.\.`).MatchString(nested) {
		t.Error("scripts/nested-modules.sh does not run nested modules' tests under the wrapper")
	}
}
