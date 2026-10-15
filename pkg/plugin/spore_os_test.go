package plugin

import (
	"errors"
	"testing"
)

func TestCheckSporeOS(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want error
	}{
		{"windows", ErrWindowsSporeUnsupported},
		{"Windows", ErrWindowsSporeUnsupported},
		{" windows ", ErrWindowsSporeUnsupported},
		{"linux", nil},
		{"", nil}, // spawn:os absent means Linux
		{"darwin", nil},
	} {
		if got := CheckSporeOS(tc.goos); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("CheckSporeOS(%q) = %v, want %v", tc.goos, got, tc.want)
		}
	}
}
