package launcher

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestBuildLinuxBootstrap_RequiresUsername(t *testing.T) {
	if _, err := BuildLinuxBootstrap(BootstrapConfig{}); err == nil {
		t.Fatal("expected error for empty username")
	}
}

func TestBuildLinuxBootstrap_RejectsBadUsername(t *testing.T) {
	if _, err := BuildLinuxBootstrap(BootstrapConfig{Username: "bad user!; rm -rf /"}); err == nil {
		t.Fatal("expected validation error for unsafe username")
	}
}

// TestBuildLinuxBootstrap_CoreContent asserts the bootstrap installs spored,
// creates the user, trusts the key, runs the spawn:command tag, and registers
// the systemd service — the invariants that make a lagotto-launched instance a
// real spore (lagotto#19) rather than a naked box.
func TestBuildLinuxBootstrap_CoreContent(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username:  "ec2-user",
		PublicKey: []byte("ssh-ed25519 AAAAC3Nz test@spawn"),
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}

	wantSubstrings := []string{
		"#!/bin/bash",
		`LOCAL_USERNAME="ec2-user"`, // ShellEscape uses strconv.Quote
		"LOCAL_SSH_KEY_BASE64=",
		"mv -f \"$SPORED_TMP\" /usr/local/bin/spored", // installs spored
		"useradd -m -s /bin/bash \"$LOCAL_USERNAME\"", // creates user
		"authorized_keys",                             // trusts the key
		"Name=key,Values=spawn:command",               // reads + runs the command tag (#2)
		"Name=key,Values=spawn:on-complete",           // surfaces on-complete (#3)
		"systemctl start spored",                      // starts the daemon
		"PrivateTmp=true: spored must see the host",   // the #66 guardrail comment
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap missing expected content: %q", want)
		}
	}
}

func TestBuildLinuxBootstrap_KeyIsBase64Encoded(t *testing.T) {
	key := []byte("ssh-rsa AAAAB3Nza-distinctive-marker test")
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", PublicKey: key})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	// The raw key must NOT appear verbatim — it's base64-encoded into the script
	// (then decoded at boot). A verbatim key would mean the encode step regressed.
	if strings.Contains(script, "distinctive-marker") {
		t.Error("public key appears un-encoded in the bootstrap")
	}
}

// TestBuildLinuxBootstrap_EmptyKeyIsValid covers the SSM-only / keyless case
// (lagotto's Lambda has no SSH key on disk): the script must still build, with
// an empty authorized_keys, so a key can be injected later over SSM.
func TestBuildLinuxBootstrap_EmptyKeyIsValid(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap with empty key: %v", err)
	}
	if !strings.Contains(script, `LOCAL_SSH_KEY_BASE64=""`) {
		t.Error("empty key should produce an empty LOCAL_SSH_KEY_BASE64 assignment")
	}
}

func TestBuildLinuxBootstrap_PluginsInjected(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username: "ec2-user",
		Plugins:  []plugin.Declaration{{Ref: "spore-host/jupyter"}},
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if !strings.Contains(script, "/etc/spawn/plugins.json") {
		t.Error("expected plugins.json injection when plugins are declared")
	}
	if !strings.Contains(script, "spore-host/jupyter") {
		t.Error("expected the plugin ref in the declarations JSON")
	}
}

func TestBuildLinuxBootstrap_NoPluginsNoInjection(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if strings.Contains(script, "/etc/spawn/plugins.json") {
		t.Error("did not expect plugins.json injection with no plugins")
	}
}

func TestBuildLinuxBootstrap_CustomUserDataAppended(t *testing.T) {
	marker := "echo CUSTOM_USERDATA_MARKER"
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username:       "ec2-user",
		CustomUserData: marker,
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if !strings.Contains(script, marker) {
		t.Error("custom user-data was not appended")
	}
	// Custom user-data must come AFTER the spored install so the daemon exists
	// when the user's script runs, and under `set +e` so a bootstrap warning
	// doesn't skip it (#27).
	idxInstall := strings.Index(script, "spored installation complete")
	idxCustom := strings.Index(script, marker)
	if idxInstall < 0 || idxCustom < idxInstall {
		t.Error("custom user-data should be appended after the spored install")
	}
}

// TestBuildLinuxBootstrap_StorageMountsBeforeUserScript is the #166 regression
// guard: attached storage must be mounted BEFORE the user's script runs, so the
// workload sees the volumes live. Mounting after the script (the old append bug)
// meant a program in user-data validated an unmounted path and failed.
func TestBuildLinuxBootstrap_StorageMountsBeforeUserScript(t *testing.T) {
	storage := "echo STORAGE_MOUNT_MARKER"
	user := "echo USER_SCRIPT_MARKER"
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username:       "ec2-user",
		StorageScript:  storage,
		CustomUserData: user,
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	idxStorage := strings.Index(script, "STORAGE_MOUNT_MARKER")
	idxUser := strings.Index(script, "USER_SCRIPT_MARKER")
	if idxStorage < 0 {
		t.Fatal("storage script was not included")
	}
	if idxUser < 0 {
		t.Fatal("user script was not included")
	}
	if idxStorage > idxUser {
		t.Errorf("storage mount (%d) must come BEFORE the user script (%d) — #166", idxStorage, idxUser)
	}
}

func TestBuildLinuxBootstrap_NoStorageScriptWhenEmpty(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", CustomUserData: "echo hi"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if strings.Contains(script, "Attached storage") {
		t.Error("no storage section should appear when StorageScript is empty")
	}
}

// TestBuildLinuxBootstrap_ContainerRunsAfterStorageBeforeUserScript is the #353
// ordering guard: a headless container run must see any mounted storage already
// live (same rationale as #166 for StorageScript vs CustomUserData), and must
// itself run before the user's own script.
func TestBuildLinuxBootstrap_ContainerRunsAfterStorageBeforeUserScript(t *testing.T) {
	storage := "echo STORAGE_MOUNT_MARKER"
	container := "echo CONTAINER_RUN_MARKER"
	user := "echo USER_SCRIPT_MARKER"
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username:        "ec2-user",
		StorageScript:   storage,
		ContainerScript: container,
		CustomUserData:  user,
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	idxStorage := strings.Index(script, "STORAGE_MOUNT_MARKER")
	idxContainer := strings.Index(script, "CONTAINER_RUN_MARKER")
	idxUser := strings.Index(script, "USER_SCRIPT_MARKER")
	if idxStorage < 0 || idxContainer < 0 || idxUser < 0 {
		t.Fatal("storage, container, and user scripts must all be included")
	}
	if idxStorage > idxContainer {
		t.Errorf("storage mount (%d) must come BEFORE the container run (%d) — #353", idxStorage, idxContainer)
	}
	if idxContainer > idxUser {
		t.Errorf("container run (%d) must come BEFORE the user script (%d) — #353", idxContainer, idxUser)
	}
}

func TestBuildLinuxBootstrap_NoContainerScriptWhenEmpty(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", CustomUserData: "echo hi"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if strings.Contains(script, "Headless container run") {
		t.Error("no container section should appear when ContainerScript is empty")
	}
}

// TestBuildLinuxBootstrap_CommandEmbedded is the #214/#246 guard: a --command is
// embedded in user-data (written to /etc/spawn/command) so it bypasses the
// 256-char spawn:command tag cap. A long command (impossible via a tag) must
// round-trip into the script verbatim, and the body must prefer the embedded
// file over the tag.
func TestBuildLinuxBootstrap_CommandEmbedded(t *testing.T) {
	longCmd := "aws s3 cp s3://b/run.sh /tmp/run.sh && " + strings.Repeat("X", 400) + " bash /tmp/run.sh"
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", Command: longCmd})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if !strings.Contains(script, "/etc/spawn/command") {
		t.Error("command should be embedded to /etc/spawn/command")
	}
	if !strings.Contains(script, longCmd) {
		t.Error("the (long) command text was not embedded verbatim")
	}
	// The body must write the embedded file before the command-exec block reads
	// it: the mkdir/here-doc write precedes the `if [ -s /etc/spawn/command ]` read.
	idxWrite := strings.Index(script, "cat > /etc/spawn/command")
	idxRead := strings.Index(script, "if [ -s /etc/spawn/command ]")
	if idxWrite < 0 || idxRead < 0 || idxWrite > idxRead {
		t.Errorf("embedded command must be written before it's read (write=%d read=%d)", idxWrite, idxRead)
	}
}

// TestBuildLinuxBootstrap_NoCommandNoEmbed: without a Command, no embed file is
// written — the bootstrap falls back to the spawn:command tag (sweeps / older
// callers).
func TestBuildLinuxBootstrap_NoCommandNoEmbed(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if strings.Contains(script, "cat > /etc/spawn/command") {
		t.Error("no command embed should appear when Command is empty")
	}
}

// TestEncodeLinuxUserData_ValidBase64Gzip is the #127 regression guard: the
// encoded user-data MUST be valid base64 that gunzips back to the original
// script. The original bug shipped raw text into RunInstances, which substrate
// accepted but real EC2 rejected ("Invalid BASE64 encoding of user data").
func TestEncodeLinuxUserData_ValidBase64Gzip(t *testing.T) {
	script := "#!/bin/bash\necho hello spore\n"
	encoded := EncodeLinuxUserData(script)

	// Must be valid base64...
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("EncodeLinuxUserData did not produce valid base64: %v", err)
	}
	// ...and gunzip back to the exact script (cloud-init gunzips it).
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoded user-data is not gzip: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if string(got) != script {
		t.Errorf("round-trip mismatch: got %q, want %q", got, script)
	}
}

// TestProvisionEncodesUserData_NotRaw guards that Provision's encoder is wired
// in — the encoded form must NOT equal the raw bootstrap (which is what #127
// shipped). Indirectly: EncodeLinuxUserData of a script never equals the script.
func TestEncodeLinuxUserData_NotRaw(t *testing.T) {
	script := "#!/bin/bash\ntrue\n"
	if EncodeLinuxUserData(script) == script {
		t.Error("encoded user-data must differ from the raw script (#127)")
	}
}

// TestBuildLinuxBootstrap_SigAwareFallback covers the #440 fix: the generated
// download logic must probe a source's .sig before committing to it (when
// signature verification is on), so a stale/unsigned regional bucket falls
// through to the us-east-1 fallback instead of downloading then hard-failing.
func TestBuildLinuxBootstrap_SigAwareFallback(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	// Verification on by default.
	if !strings.Contains(script, "SPORED_SIG_VERIFY=1") {
		t.Error("expected SPORED_SIG_VERIFY=1 default")
	}
	// The candidate list must include both the regional base and the us-east-1
	// fallback, so a source that can't satisfy the trust requirement falls through.
	for _, want := range []string{
		"CANDIDATES=(",
		"${S3_BASE_URL}/${PROJECT}",
		"${FALLBACK_URL}/${PROJECT}",
		// HEAD-probes the .sig up front when verifying (the crux of the fix).
		// Retry flags tolerate a transient S3 hiccup on the probe (#462).
		`curl -fsI --retry 3 --retry-delay 1 --retry-all-errors "$sig_url"`,
		"trying next source",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("generated bootstrap missing %q", want)
		}
	}
	// The old brittle shape — commit to the regional binary, then separately fail
	// on a missing .sig — must be gone.
	if strings.Contains(script, "Regional bucket unavailable, trying us-east-1...") {
		t.Error("old fallback logic still present; #440 refactor not applied")
	}

	// The binary + checksum fetches must RETRY (#462): a single-shot curl that
	// dies on a transient S3 empty-reply hard-fails the whole install. Lock in the
	// retry flags so a future edit can't silently drop them.
	for _, want := range []string{
		`curl -f --retry 5 --retry-delay 2 --retry-all-errors -o "$SPORED_TMP"`,      // binary
		`curl -f --retry 5 --retry-delay 2 --retry-all-errors -o /tmp/spored.sha256`, // checksum
	} {
		if !strings.Contains(script, want) {
			t.Errorf("generated bootstrap missing retry on a critical fetch: %q", want)
		}
	}
}

// extractParamTagLoop pulls the exact `while IFS=$'\t' read -r key value; do
// ... done` block that parses spawn:param:* tags into /etc/profile.d out of
// the generated bootstrap script, and rewrites the hardcoded
// /etc/profile.d/spawn-params.sh path to outFile so a test can run the real
// generated shell text against a temp file instead of needing root. Fails the
// test (rather than returning an error) if the markers this depends on ever
// move, so a refactor of bootstrap.go that breaks the extraction is caught
// immediately instead of silently testing stale text.
func extractParamTagLoop(t *testing.T, script, outFile string) string {
	t.Helper()
	const start = `echo "$PARAM_TAGS" | while IFS=$'\t' read -r key value; do`
	startIdx := strings.Index(script, start)
	if startIdx < 0 {
		t.Fatalf("could not find the param-tag parsing loop in the generated bootstrap (marker moved?)")
	}
	rest := script[startIdx:]
	const end = "\n    done\n"
	endIdx := strings.Index(rest, end)
	if endIdx < 0 {
		t.Fatalf("could not find the end of the param-tag parsing loop (marker moved?)")
	}
	loop := rest[:endIdx+len(end)]
	replaced := strings.ReplaceAll(loop, "/etc/profile.d/spawn-params.sh", outFile)
	if replaced == loop {
		t.Fatalf("extracted loop did not reference /etc/profile.d/spawn-params.sh — extraction is wrong")
	}
	return replaced
}

// TestBuildLinuxBootstrap_ParamValueSurvivesRealShellParsing is the #531
// regression guard. Before the fix, pkg/launcher/bootstrap.go wrote
//
//	export PARAM_<name>="<value>"
//
// with the value double-quoted and unescaped, so a login shell sourcing
// /etc/profile.d/spawn-params.sh reinterpreted $, `, and " in the value
// instead of treating it as literal text. This test does not compare Go-side
// escaping logic against expectations — that would only prove the Go string
// manipulation does what it says, not that the shell agrees. Instead it
// extracts the ACTUAL while-loop bootstrap.go generates, feeds it a
// tab-separated "tag\tvalue" line exactly like `aws ec2 describe-tags --output
// text` would produce, runs that loop for real under bash, sources the
// resulting file for real under bash, and reads back $PARAM_<name> — so a
// regression to double-quoting (or any other quoting mistake) is caught by
// bash's own parser, not by Go arithmetic on strings.
func TestBuildLinuxBootstrap_ParamValueSurvivesRealShellParsing(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{name: "embedded double quote", value: `run "A"`},
		{name: "dollar-sign expansion attempt", value: "$HOME/out"},
		{name: "backtick command substitution attempt", value: "`hostname`"},
		{name: "embedded single quote", value: "it's a test"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outFile := filepath.Join(t.TempDir(), "spawn-params.sh")
			loop := extractParamTagLoop(t, script, outFile)

			shellScript := fmt.Sprintf(`#!/bin/bash
set -e
PARAM_TAGS=$(printf 'spawn:param:label\t%%s\n' %s)
%s
source %s
printf '%%s' "$PARAM_label"
`, shellQuoteForTest(tc.value), loop, outFile)

			cmd := exec.Command("bash", "-c", shellScript)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("shell exec failed: %v\noutput: %s\nscript:\n%s", err, out, shellScript)
			}
			got := string(out)
			if got != tc.value {
				t.Errorf("value did not survive real shell sourcing: got %q, want %q (raw tag value; "+
					"this is the exact PARAM_label the workload would see on the instance)", got, tc.value)
			}
		})
	}
}

// shellQuoteForTest single-quotes a string for embedding in the *test driver*
// shell script (the printf that manufactures a fake AWS describe-tags line).
// This is deliberately separate from — and simpler than — the fix under test:
// it only needs to survive going into printf's %s once, not round-trip
// through a second layer of tag storage and profile.d sourcing.
func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// TestBuildLinuxBootstrap_ValidBash syntax-checks the generated script with
// `bash -n` so a shell error in the #440 download refactor can't ship silently.
func TestBuildLinuxBootstrap_ValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	// Strip the cloud-init "#cloud-config"/shebang handling isn't needed — the
	// body is plain bash. `bash -n` parses without executing.
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("generated bootstrap is not valid bash: %v\n%s", err, out)
	}
}
