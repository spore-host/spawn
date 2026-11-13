package plugin

import (
	"strings"
	"testing"
)

// #1590: the secret config the registry plugins take, by the names they use
// (code-server/rstudio-server password, jupyterlab token, tailscale auth_key,
// globus setup_key), is recognised; ordinary settings are not.
func TestIsSecretConfigKey(t *testing.T) {
	for _, k := range []string{"password", "PASSWORD", "admin_password", "passwd", "token", "jupyter_token",
		"auth_key", "authkey", "api_key", "setup_key", "private_key", "client_secret", "passphrase", "aws_credentials"} {
		if !IsSecretConfigKey(k) {
			t.Errorf("%q is not treated as secret", k)
		}
	}
	for _, k := range []string{"port", "endpoint_name", "key_name", "public_key", "ssh_public_key", "version", "hostname"} {
		if IsSecretConfigKey(k) {
			t.Errorf("%q is treated as secret", k)
		}
	}
}

func TestCheckDeclarationsCarryNoSecrets(t *testing.T) {
	ok := []Declaration{{Ref: "globus-connect", Config: map[string]string{"endpoint_name": "lab"}}, {Ref: "jupyterlab"}}
	if err := CheckDeclarationsCarryNoSecrets(ok); err != nil {
		t.Errorf("declarations with no secret refused: %v", err)
	}
	bad := append(ok, Declaration{Ref: "code-server", Config: map[string]string{"password": "hunter2", "port": "8080"}})
	err := CheckDeclarationsCarryNoSecrets(bad)
	if err == nil {
		t.Fatal("a declaration carrying code-server's password was accepted for user data")
	}
	if !strings.Contains(err.Error(), "code-server: password") || strings.Contains(err.Error(), "hunter2") ||
		!strings.Contains(err.Error(), "spawn plugin install") {
		t.Errorf("refusal %q must name the plugin and key, never the value, and say how to set it", err)
	}
}

func TestSpecSecretKeys(t *testing.T) {
	spec, err := ParseSpec([]byte("name: demo\nconfig:\n  admin_pw:\n    secret: true\n  port:\n    default: 80\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.SecretKeys(); len(got) != 1 || got[0] != "admin_pw" {
		t.Errorf("SecretKeys = %v, want [admin_pw]", got)
	}
}
