package plugin

import (
	"context"
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

// The blanket Windows refusal and a spec's platform condition must agree about
// which OS a spore runs (#1055): a spore CheckSporeOS refuses must fail the
// registry's conditions.remote os: linux and pass os: windows, and a spore it
// lets through must not be Windows to the condition. They now share
// NormalizeOS; this keeps them from drifting back to two normalisers.
func TestCheckSporeOSAgreesWithPlatformConditions(t *testing.T) {
	ctx := context.Background()
	platform := func(os, goos string) error {
		return CheckCondition(ctx, Condition{Type: "platform", OS: os}, NewTemplateContext(), ConditionHost{GOOS: goos})
	}
	for _, g := range []string{"windows", "Windows", " windows ", "\tWINDOWS\n", "linux", "LINUX", " linux ", "darwin", ""} {
		refused := CheckSporeOS(g) != nil
		if refused != (platform("windows", g) == nil) {
			t.Errorf("GOOS %q: CheckSporeOS refused=%v, but a platform os: windows condition passes=%v", g, refused, platform("windows", g) == nil)
		}
		if refused && platform("linux", g) == nil {
			t.Errorf("GOOS %q: refused as Windows, yet passes the registry's os: linux condition", g)
		}
		if NormalizeOS(g) == "linux" && (refused || platform("linux", g) != nil) {
			t.Errorf("GOOS %q is Linux: refused=%v, os: linux condition error=%v; want neither", g, refused, platform("linux", g))
		}
	}
}
