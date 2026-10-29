package launchconfig

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestEncodeIsCanonicalAndDecodeVerifies(t *testing.T) {
	a, shaA, err := Encode(map[string]string{NotifyURL: "https://n/?a=1&b=2", IdleCPU: "7", WebAuth: ""})
	if err != nil {
		t.Fatal(err)
	}
	b, shaB, _ := Encode(map[string]string{IdleCPU: "7", NotifyURL: "https://n/?a=1&b=2"})
	if shaA != shaB || string(a) != string(b) {
		t.Fatalf("the same settings encoded differently: %s / %s", a, b)
	}
	if shaA != SHA256Hex(a) || !ValidSHA(shaA) {
		t.Fatalf("sha %q is not the body's sha256", shaA)
	}
	doc, unknown, err := Decode(a, shaA)
	if err != nil || len(unknown) > 0 || doc.Settings[NotifyURL] != "https://n/?a=1&b=2" || len(doc.Settings) != 2 {
		t.Fatalf("Decode = %+v, %v, %v", doc, unknown, err)
	}
	if _, ok := doc.Settings[WebAuth]; ok {
		t.Error("an empty setting was kept")
	}

	tampered := []byte(strings.Replace(string(a), "7", "8", 1))
	if _, _, err := Decode(tampered, shaA); !errors.Is(err, ErrDigest) {
		t.Errorf("a tampered body decoded: %v", err)
	}
	if _, _, err := Decode(a, "not-a-sha"); err == nil {
		t.Error("a malformed sha was accepted")
	}
	other := []byte(`{"schema":2,"settings":{}}`)
	if _, _, err := Decode(other, SHA256Hex(other)); err == nil || !strings.Contains(err.Error(), "schema 2") {
		t.Errorf("schema 2 = %v, want a refusal", err)
	}
	future := []byte(`{"schema":1,"settings":{"notify-url":"u","from-the-future":"x"}}`)
	doc, unknown, err = Decode(future, SHA256Hex(future))
	if err != nil || strings.Join(unknown, ",") != "from-the-future" || doc.Settings[NotifyURL] != "u" {
		t.Errorf("an unknown key: %+v %v %v; want it reported and the rest read", doc, unknown, err)
	}
}

func TestEncodeRefusesAKeyThatIsNotASetting(t *testing.T) {
	if _, _, err := Encode(map[string]string{"ttl": "1h"}); err == nil {
		t.Error("Encode accepted ttl, which stays a tag")
	}
	if body, s, err := Encode(nil); body != nil || s != "" || err != nil {
		t.Errorf("Encode(nil) = %q %q %v; want no object", body, s, err)
	}
}

func TestKeysAndARN(t *testing.T) {
	s := strings.Repeat("0", 64)
	if got, ok := SHAOfKey(Key(s)); !ok || got != s {
		t.Errorf("SHAOfKey(Key) = %q, %v", got, ok)
	}
	for _, k := range []string{"launch-config/config-abc.json", "bootstrap/t/bootstrap-" + s + ".sh", "launch-config/" + s + ".json"} {
		if _, ok := SHAOfKey(k); ok {
			t.Errorf("SHAOfKey(%q) accepted it", k)
		}
	}
	pat := ObjectARNPattern("111122223333")
	re := regexp.MustCompile("^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pat)) + "$")
	if !re.MatchString("arn:aws:s3:::spawn-staging-111122223333-us-east-1/" + Key(s)) {
		t.Errorf("%s does not match a config object", pat)
	}
	for _, other := range []string{
		"arn:aws:s3:::spawn-staging-111122223333-us-east-1/bootstrap/t/bootstrap-" + s + ".sh",
		"arn:aws:s3:::spawn-staging-999999999999-us-east-1/" + Key(s),
		"arn:aws:s3:::spawn-staging-111122223333-us-east-1/launch-config/config-x.json",
	} {
		if re.MatchString(other) {
			t.Errorf("%s matches %s", pat, other)
		}
	}
}

// Every key is classified once: a setting is never also listed as a tag.
func TestNoKeyIsBothObjectAndTag(t *testing.T) {
	for k := range Settings {
		if _, ok := TagKeys[k]; ok {
			t.Errorf("%s is both a launch config setting and a tag", k)
		}
	}
	if len(Settings) < 20 || len(TagKeys) < 40 {
		t.Fatalf("classification has %d settings and %d tags; it has been emptied", len(Settings), len(TagKeys))
	}
}
