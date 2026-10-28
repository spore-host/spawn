package storage

import (
	"strings"
	"testing"
)

// --efs-mount-options (#1390). ParseCustomOptions used to drop every option it
// had no field for, under a comment that said "pass through": nconnect=8
// rendered the same mount string as no options at all, and nothing said so.

// An option the parser does not model reaches the mount string verbatim, in the
// order given, ahead of the trailing _netdev.
func TestParseCustomOptionsPassesUnknownOptionsThrough(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"nconnect=4", "nconnect=4"},
		{"nconnect=8,timeo=150", "nconnect=8"},
		{"noac", "noac"},
		{"lookupcache=none,nconnect=16", "lookupcache=none,nconnect=16"},
		{"sec=sys", "sec=sys"},
	} {
		opts, err := ParseCustomOptions(tc.in)
		if err != nil {
			t.Errorf("ParseCustomOptions(%q): %v", tc.in, err)
			continue
		}
		got := opts.ToMountString()
		if !strings.HasSuffix(got, ","+tc.want+",_netdev") {
			t.Errorf("ParseCustomOptions(%q).ToMountString() = %q; want it to carry %q before _netdev", tc.in, got, tc.want)
		}
	}
	// And the known option beside it still lands in its field.
	opts, err := ParseCustomOptions("nconnect=8,timeo=150")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Timeo != 150 {
		t.Errorf("timeo=150 beside nconnect=8 gave Timeo %d", opts.Timeo)
	}
}

// What the parser cannot vouch for is refused, naming the option: shell or
// fstab metacharacters (the string goes into a mount command and an fstab
// line), a known numeric option whose value is not a number (it used to become
// 0 silently), and the amazon-efs-utils options that `mount -t nfs4` — the
// helper the storage script uses — does not understand.
func TestParseCustomOptionsRefusesWhatItCannotPass(t *testing.T) {
	for _, in := range []string{
		"nconnect=4;reboot",
		"nconnect=$(id)",
		"nconnect=`id`",
		"a b",
		"nconnect=4\tnoac",
		"noac'",
		`noac"`,
		"x|y",
		"x&y",
		"x>y",
		"#x",
		"nfsvers=4.1;id",
		"rsize=big",
		"timeo=",
		"retrans=-1x",
		"=4",
		"tls",
		"iam",
		"accesspoint=fsap-0123",
	} {
		_, err := ParseCustomOptions(in)
		if err == nil {
			t.Errorf("ParseCustomOptions(%q) accepted it", in)
			continue
		}
		if !strings.Contains(err.Error(), "--efs-mount-options") {
			t.Errorf("ParseCustomOptions(%q): the error does not name the flag: %v", in, err)
		}
	}
}

// The options the parser models render exactly as before #1390.
func TestParseCustomOptionsKnownOptionsUnchanged(t *testing.T) {
	for in, want := range map[string]string{
		"":          "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport,_netdev",
		"timeo=150": "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=150,retrans=2,noresvport,_netdev",
		"rsize=262144, wsize=262144, async, actimeo=5": "nfsvers=4.1,rsize=262144,wsize=262144,hard,timeo=600,retrans=2,noresvport,actimeo=5,async,_netdev",
		"soft,nosharecache,nfsvers=4.0,retrans=5":      "nfsvers=4.0,rsize=1048576,wsize=1048576,timeo=600,retrans=5,noresvport,nosharecache,_netdev",
		"hard,noresvport,_netdev":                      "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport,_netdev",
	} {
		opts, err := ParseCustomOptions(in)
		if err != nil {
			t.Errorf("ParseCustomOptions(%q): %v", in, err)
			continue
		}
		if got := opts.ToMountString(); got != want {
			t.Errorf("ParseCustomOptions(%q).ToMountString() = %q, want %q", in, got, want)
		}
	}
}
