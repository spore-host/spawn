package main

import "testing"

// TestMergeSlackSecrets covers the precedence: a non-empty field in the Secrets
// Manager blob wins; an absent/empty field keeps the env fallback; malformed
// JSON returns the fallback untouched with an error.
func TestMergeSlackSecrets(t *testing.T) {
	fallback := slackSecrets{ClientSecret: "env-client", SigningSecret: "env-signing"}

	tests := []struct {
		name       string
		secretJSON string
		want       slackSecrets
		wantErr    bool
	}{
		{
			name:       "both fields in secret override env",
			secretJSON: `{"client_secret":"sm-client","signing_secret":"sm-signing"}`,
			want:       slackSecrets{ClientSecret: "sm-client", SigningSecret: "sm-signing"},
		},
		{
			name:       "partial secret keeps env for the missing field",
			secretJSON: `{"client_secret":"sm-client"}`,
			want:       slackSecrets{ClientSecret: "sm-client", SigningSecret: "env-signing"},
		},
		{
			name:       "empty-string field does not clobber env",
			secretJSON: `{"client_secret":"","signing_secret":"sm-signing"}`,
			want:       slackSecrets{ClientSecret: "env-client", SigningSecret: "sm-signing"},
		},
		{
			name:       "empty object keeps env for both",
			secretJSON: `{}`,
			want:       fallback,
		},
		{
			name:       "malformed JSON returns fallback + error",
			secretJSON: `{not json`,
			want:       fallback,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeSlackSecrets(tc.secretJSON, fallback)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("merge = %+v, want %+v", got, tc.want)
			}
		})
	}
}
