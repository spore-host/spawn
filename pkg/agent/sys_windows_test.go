//go:build windows

package agent

import "testing"

func TestQuserIdleIsRecent(t *testing.T) {
	cases := []struct {
		idle string
		want bool
	}{
		{"none", true},
		{".", true},
		{"", true},
		{"2", true},     // 2 minutes < 5
		{"4", true},     // 4 minutes < 5
		{"5", false},    // 5 minutes not < 5
		{"30", false},   // 30 minutes
		{"1:05", false}, // over an hour
		{"3+02:00", false},
		{"garbage", true}, // unknown → conservative
	}
	for _, tc := range cases {
		if got := quserIdleIsRecent(tc.idle); got != tc.want {
			t.Errorf("quserIdleIsRecent(%q) = %v, want %v", tc.idle, got, tc.want)
		}
	}
}
