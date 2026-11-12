package accesstoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMintAndHash(t *testing.T) {
	a, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Mint()
	if a == b || len(a) != 2*tokenBytes {
		t.Fatalf("Mint gave %q and %q", a, b)
	}
	if h := Hash(a); !ValidHash(h) || h == a || strings.Contains(h, a) {
		t.Errorf("Hash(%q) = %q", a, h)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("g", 64), "../" + strings.Repeat("a", 61)} {
		if ValidHash(bad) {
			t.Errorf("ValidHash(%q) = true", bad)
		}
	}
}

func TestStoreAddVerifyExpire(t *testing.T) {
	now := time.Now()
	s := &Store{Dir: filepath.Join(t.TempDir(), "tokens"), Now: func() time.Time { return now }}
	tok, _ := Mint()
	exp, err := s.Add(KindWeb, Hash(tok), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(time.Hour).UTC()) {
		t.Errorf("expires %s, want %s", exp, now.Add(time.Hour))
	}
	fi, err := os.Stat(s.Dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	fi, err = os.Stat(filepath.Join(s.Dir, Hash(tok)))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir, Hash(tok)))
	if strings.Contains(string(data), tok) {
		t.Error("the record on disk contains the token")
	}

	if ok, got := s.Verify(KindWeb, tok); !ok || !got.Equal(exp) {
		t.Errorf("Verify = %v %s, want true %s", ok, got, exp)
	}
	if ok, _ := s.Verify(KindDCV, tok); ok {
		t.Error("a web token verified as a DCV token")
	}
	if ok, _ := s.Verify(KindWeb, Hash(tok)); ok {
		t.Error("the hash verified as the token")
	}
	if n, _ := s.Prune(); n != 1 {
		t.Errorf("Prune left %d, want 1", n)
	}

	now = now.Add(time.Hour)
	if ok, _ := s.Verify(KindWeb, tok); ok {
		t.Error("an expired token verified")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, Hash(tok))); !os.IsNotExist(err) {
		t.Errorf("expired record not removed (%v)", err)
	}
}

func TestStoreRejects(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	tok, _ := Mint()
	for name, f := range map[string]func() error{
		"bad kind":  func() error { _, err := s.Add("ssh", Hash(tok), time.Hour); return err },
		"bad hash":  func() error { _, err := s.Add(KindDCV, tok[:10], time.Hour); return err },
		"zero ttl":  func() error { _, err := s.Add(KindDCV, Hash(tok), 0); return err },
		"ttl > max": func() error { _, err := s.Add(KindDCV, Hash(tok), MaxTTL+time.Second); return err },
	} {
		if f() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPruneAndRevokeAll(t *testing.T) {
	now := time.Now()
	s := &Store{Dir: filepath.Join(t.TempDir(), "t"), Now: func() time.Time { return now }}
	if n, err := s.Prune(); n != 0 || err != nil {
		t.Fatalf("Prune on a missing dir = %d, %v", n, err)
	}
	a, _ := Mint()
	b, _ := Mint()
	_, _ = s.Add(KindDCV, Hash(a), time.Minute)
	_, _ = s.Add(KindDCV, Hash(b), time.Hour)
	now = now.Add(2 * time.Minute)
	if n, _ := s.Prune(); n != 1 {
		t.Errorf("Prune left %d, want 1", n)
	}
	if err := s.RevokeAll(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Verify(KindDCV, b); ok {
		t.Error("a token verified after RevokeAll")
	}
	if err := s.RevokeAll(); err != nil {
		t.Errorf("RevokeAll on a missing dir: %v", err)
	}
}

func TestEnsureDirRefusesASymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "tokens")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	tok, _ := Mint()
	if _, err := (&Store{Dir: link}).Add(KindDCV, Hash(tok), time.Hour); err == nil {
		t.Error("Add wrote through a symlinked token directory")
	}
}

// AddCommand is all that SSM's history records: it must carry the hash and
// never the token, and nothing a shell would interpret.
func TestAddCommand(t *testing.T) {
	tok, _ := Mint()
	cmd, err := AddCommand(KindDCV, Hash(tok), 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := SporedPath + " access-token add --kind dcv --sha256 " + Hash(tok) + " --ttl 43200s"
	if cmd != want {
		t.Errorf("AddCommand = %q, want %q", cmd, want)
	}
	if strings.Contains(cmd, tok) {
		t.Error("the command carries the token")
	}
	if _, err := AddCommand(KindDCV, tok+"; rm -rf /", time.Hour); err == nil {
		t.Error("AddCommand accepted a non-hash")
	}
	if _, err := AddCommand("dcv; id", Hash(tok), time.Hour); err == nil {
		t.Error("AddCommand accepted a bad kind")
	}
}

func TestURLs(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{DCVURL("box.x.spore.host", "abc", "console"), "https://box.x.spore.host:8443/?authToken=abc#console"},
		{DCVURL("1.2.3.4", "", "console"), "https://1.2.3.4:8443/#console"},
		{WebURL("box.x.spore.host", "abc"), "https://box.x.spore.host/?spore_token=abc"},
		{WebURL("box.x.spore.host", ""), "https://box.x.spore.host/"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
