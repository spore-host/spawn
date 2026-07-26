package main

import "testing"

// TestPortalAccountFromARN covers the federated-portal identity recognizer: a
// caller assuming a trusted portal launch role is scoped by its OWN verified
// account; anything else (IAM users, unknown roles, malformed ARNs) is not a
// portal identity and falls through to the existing Cognito/CLI paths.
func TestPortalAccountFromARN(t *testing.T) {
	t.Setenv("SPAWN_DASHBOARD_PORTAL_ROLES", "") // use built-in default

	tests := []struct {
		name        string
		arn         string
		wantAccount string
		wantOK      bool
	}{
		{
			name:        "trusted portal launch role → account is the scope key",
			arn:         "arn:aws:sts::435415984226:assumed-role/spore-portal-launch/globus-abc123",
			wantAccount: "435415984226",
			wantOK:      true,
		},
		{
			name:   "IAM user ARN is not a portal identity",
			arn:    "arn:aws:iam::435415984226:user/alice",
			wantOK: false,
		},
		{
			name:   "assumed-role for an untrusted role is rejected",
			arn:    "arn:aws:sts::435415984226:assumed-role/some-other-role/session",
			wantOK: false,
		},
		{
			name:   "malformed ARN is rejected",
			arn:    "not-an-arn",
			wantOK: false,
		},
		{
			name:   "empty string is rejected",
			arn:    "",
			wantOK: false,
		},
		{
			name:   "assumed-role without a session segment is rejected",
			arn:    "arn:aws:sts::435415984226:assumed-role/spore-portal-launch",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			account, ok := portalAccountFromARN(tc.arn)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && account != tc.wantAccount {
				t.Errorf("account = %q, want %q", account, tc.wantAccount)
			}
		})
	}
}

// TestPortalRoleAllowList_EnvOverride verifies a per-deploy role list overrides
// the built-in default, and that a portal role NOT in a custom list is rejected.
func TestPortalRoleAllowList_EnvOverride(t *testing.T) {
	t.Setenv("SPAWN_DASHBOARD_PORTAL_ROLES", "spore-ucla-launch, custom-portal-role ")

	got := portalRoleAllowList()
	want := []string{"spore-ucla-launch", "custom-portal-role"}
	if len(got) != len(want) {
		t.Fatalf("allow list = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allow list = %v, want %v (trimmed)", got, want)
		}
	}

	// A role in the custom list is accepted.
	if account, ok := portalAccountFromARN("arn:aws:sts::111122223333:assumed-role/spore-ucla-launch/s"); !ok || account != "111122223333" {
		t.Errorf("custom-list role: account=%q ok=%v, want 111122223333/true", account, ok)
	}
	// The built-in default is NOT accepted once a custom list is set.
	if _, ok := portalAccountFromARN("arn:aws:sts::111122223333:assumed-role/spore-portal-launch/s"); ok {
		t.Error("spore-portal-launch should be rejected when a custom allow list excludes it")
	}
}

// TestPortalRoleAllowList_DefaultWhenUnset verifies the built-in default applies
// when the env var is unset or blank.
func TestPortalRoleAllowList_DefaultWhenUnset(t *testing.T) {
	t.Setenv("SPAWN_DASHBOARD_PORTAL_ROLES", "")
	got := portalRoleAllowList()
	if len(got) != 1 || got[0] != "spore-portal-launch" {
		t.Errorf("default allow list = %v, want [spore-portal-launch]", got)
	}
}
