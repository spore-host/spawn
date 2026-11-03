package egressproxy

import "testing"

// curl's no_proxy rule (lib/noproxy.c, 7.86+), case by case.
func TestNoProxyBypasses(t *testing.T) {
	for _, tc := range []struct {
		noProxy, host string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "www.example.com", true},
		{"example.com", "a.b.example.com", true},
		{"example.com", "notexample.com", false},
		{"example.com", "example.com.evil", false},
		{".example.com", "example.com", true}, // the leading dot is ignored
		{".example.com", "www.example.com", true},
		{"example.com.", "www.example.com", true}, // so is a trailing one
		{"EXAMPLE.com", "www.Example.COM.", true},
		{"a.com, example.com", "x.example.com", true},
		{"a.com example.com", "x.example.com", true},
		{"a.com,,", "x.example.com", false},
		{"*", "anything.at.all", true},
		{"*.example.com", "www.example.com", false}, // no globbing
		{"", "example.com", false},
		{"amazonaws.com", "efa-installer.amazonaws.com", true},
		{".s3.amazonaws.com", "efa-installer.amazonaws.com", false},
		{"169.254.169.254", "169.254.169.254", true},
		{"10.0.0.0/8", "10.1.2.3", true},
		{"10.0.0.0/8", "11.1.2.3", false},
		{"10.0.0.0/8", "ten.example.com", false},
		{"localhost", "127.0.0.1", false}, // a name never matches an address
		{"::1", "[::1]", true},
	} {
		if got := NoProxyBypasses(tc.noProxy, tc.host); got != tc.want {
			t.Errorf("NoProxyBypasses(%q, %q) = %v, want %v", tc.noProxy, tc.host, got, tc.want)
		}
	}
}
