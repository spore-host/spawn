package instance

import (
	"strings"
	"testing"
)

func TestParseInstanceTypePattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    []string
		wantErr bool
	}{
		{
			name:    "empty pattern returns error",
			pattern: "",
			wantErr: true,
		},
		{
			name:    "single type returned as-is",
			pattern: "c5.large",
			want:    []string{"c5.large"},
		},
		{
			name:    "pipe-separated list",
			pattern: "c5.large|c5.xlarge|m5.large",
			want:    []string{"c5.large", "c5.xlarge", "m5.large"},
		},
		{
			name:    "pipe list with spaces trimmed",
			pattern: "c5.large | c5.xlarge",
			want:    []string{"c5.large", "c5.xlarge"},
		},
		{
			name:    "wildcard expands to all sizes",
			pattern: "c5.*",
			want:    nil, // checked separately by count and prefix
		},
		{
			name:    "wildcard different family",
			pattern: "m5.*",
			want:    nil,
		},
		{
			name:    "invalid wildcard format (no dot-star suffix)",
			pattern: "c5*",
			wantErr: true,
		},
		{
			name:    "empty family in wildcard",
			pattern: ".*",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInstanceTypePattern(tt.pattern)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			// Wildcard cases: verify count and prefix.
			if strings.Contains(tt.pattern, "*") {
				family := strings.TrimSuffix(tt.pattern, ".*")
				if len(got) == 0 {
					t.Error("wildcard expansion returned empty list")
				}
				for _, typ := range got {
					if !strings.HasPrefix(typ, family+".") {
						t.Errorf("type %q does not start with %q", typ, family+".")
					}
				}
				return
			}

			// Exact match cases.
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseInstanceTypePattern_WildcardSizes(t *testing.T) {
	got, err := ParseInstanceTypePattern("t3.*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should include at least nano, micro, small, medium, large, xlarge, 2xlarge.
	required := []string{"t3.nano", "t3.micro", "t3.small", "t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge"}
	gotSet := make(map[string]bool, len(got))
	for _, s := range got {
		gotSet[s] = true
	}
	for _, r := range required {
		if !gotSet[r] {
			t.Errorf("wildcard expansion missing expected type %q", r)
		}
	}
}
