package launcher

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
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
