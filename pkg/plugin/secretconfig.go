package plugin

import (
	"fmt"
	"sort"
	"strings"
)

// Secret-valued plugin config in launch declarations (#1590).
//
// A launch's plugin declarations (`spawn launch --config`, a sweep, or an SDK
// caller's launcher.Options.Plugins) travel in EC2 user data, which any
// principal with ec2:DescribeInstanceAttribute can read; ReadOnlyAccess grants
// it. So a declaration may not carry a secret. Registry plugins take secrets as
// config: code-server's and rstudio-server's password, jupyterlab's token,
// tailscale's auth_key. A secret is set after launch instead, with
// `spawn plugin install <instance> <ref> --config key=value`, which goes to
// spored over SSH or sealed over SSM (pkg/pushseal) and never into user data.
//
// The CLI cannot see the spec at that point (the instance resolves the ref),
// so the test is on the key's name. A spec may also mark a key secret: true,
// which spawn honours wherever it has the spec.

// secretKeyWords are the words that make a config key secret wherever they
// appear in it.
var secretKeyWords = []string{"password", "passwd", "passphrase", "secret", "token", "credential"}

// IsSecretConfigKey reports whether a plugin config key names a secret: it
// contains one of secretKeyWords, or it is "key" or ends in "key" (auth_key,
// authkey, api_key, private_key, setup_key) and is not a public key.
func IsSecretConfigKey(key string) bool {
	k := strings.ToLower(key)
	for _, w := range secretKeyWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	if strings.HasSuffix(k, "key") && !strings.Contains(k, "public") {
		return true
	}
	return false
}

// SecretKeys are the keys of s.Config marked secret: true.
func (s *PluginSpec) SecretKeys() []string {
	var out []string
	for k, p := range s.Config {
		if p.Secret {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// CheckDeclarationsCarryNoSecrets refuses declarations whose config sets a
// secret-named key (IsSecretConfigKey), naming every one and how to set it
// instead.
func CheckDeclarationsCarryNoSecrets(decls []Declaration) error {
	var found []string
	for _, d := range decls {
		keys := make([]string, 0, len(d.Config))
		for k := range d.Config {
			if IsSecretConfigKey(k) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			found = append(found, fmt.Sprintf("%s: %s", d.Ref, k))
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to put plugin secrets in EC2 user data, where anyone with "+
		"ec2:DescribeInstanceAttribute can read them (#1590): %s. Drop them from the launch config and set them "+
		"once the instance is up with `spawn plugin install <instance> <ref> --config key=value`, which reaches "+
		"spored over SSH or sealed over SSM", strings.Join(found, ", "))
}
