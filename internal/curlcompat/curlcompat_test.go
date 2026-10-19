package curlcompat

import (
	"reflect"
	"testing"
)

// The captured help must parse into a plausible option set. If the file is
// truncated or its format changes, every gate built on it would pass vacuously
// (an empty set rejects everything; a regexp that stops matching accepts
// nothing) — so pin known members and a known non-member.
func TestEL8OptionSetIsTheRealOne(t *testing.T) {
	o := EL8()
	if len(o.Long) < 150 {
		t.Fatalf("parsed only %d long options from the curl 7.61.1 help; the file or the parser has broken", len(o.Long))
	}
	for _, want := range []string{"--retry", "--retry-delay", "--retry-connrefused", "--fail", "--head", "--output", "--silent", "--location", "--header", "--request"} {
		if !o.Long[want] {
			t.Errorf("curl 7.61.1 has %s, but it is missing from the parsed set", want)
		}
	}
	for _, c := range "fsSLIoHXO" {
		if !o.Short[byte(c)] {
			t.Errorf("curl 7.61.1 has -%c, but it is missing from the parsed set", c)
		}
	}
	// Added after 7.61.1: --retry-all-errors 7.71, --fail-with-body 7.76,
	// --parallel/-Z 7.66, --no-progress-meter 7.67, --json 7.82.
	for _, newer := range []string{"--retry-all-errors", "--fail-with-body", "--parallel", "--no-progress-meter", "--json"} {
		if o.Long[newer] {
			t.Errorf("%s is newer than curl 7.61.1 but is in the parsed set", newer)
		}
	}
	if o.Short['Z'] {
		t.Error("-Z (--parallel, 7.66) is newer than curl 7.61.1 but is in the parsed set")
	}
}

func TestInvocationsMatchCommandsNotProse(t *testing.T) {
	script := `# the old code used curl --retry-all-errors here
// curl --retry-all-errors in a Go comment
echo "libcurl is fine"
TOKEN=$(curl -sX PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60" 2>/dev/null || true)
    if ! spore_curl_retry 5 2 -f --retry-all-errors -o "$tmp" "$url"; then
curl -fsSL https://example.com/x | sudo tee /etc/x --json
`
	invs := Invocations(script)
	if len(invs) != 3 {
		t.Fatalf("found %d invocations, want 3 (the two comments and the echo are not curl commands): %+v", len(invs), invs)
	}
	o := EL8()
	var got [][]string
	for _, inv := range invs {
		got = append(got, o.Unsupported(inv))
	}
	want := [][]string{nil, {"--retry-all-errors"}, nil}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unsupported options per invocation = %q, want %q (the --json after the pipe belongs to tee)", got, want)
	}
}
