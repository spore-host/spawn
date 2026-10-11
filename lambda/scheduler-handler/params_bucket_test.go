package main

import "testing"

// A recorded bucket must win over the region template (#804).
//
// resolveSchedulesBucket falls back to an account-scoped
// `spawn-schedules-<account>-<region>` when the shared name is taken by another
// account, and returns which one it used. The CLI used to discard that, so the
// record carried only a key and this handler rebuilt the name from the region —
// meaning every scheduled execution in such an account read a bucket it had
// never written to.
//
// Tested as a pure decision rather than by reading the source for the order of
// two expressions. That ordering gate passed a revert which computed the
// template first and then overrode it, because computing-then-overriding is
// equally correct — it was checking style, not the mechanism.
func TestParamsBucketPrefersTheRecordedBucket(t *testing.T) {
	cases := []struct {
		name, recorded, region, want string
	}{
		{
			name:     "account-scoped fallback bucket is honoured",
			recorded: "spawn-schedules-123456789012-us-east-1",
			region:   "us-east-1",
			want:     "spawn-schedules-123456789012-us-east-1",
		},
		{
			name:     "a recorded shared bucket is still used verbatim",
			recorded: "spawn-schedules-eu-west-1",
			region:   "eu-west-1",
			want:     "spawn-schedules-eu-west-1",
		},
		{
			name:     "a recorded bucket in a DIFFERENT region wins over the region",
			recorded: "spawn-schedules-999999999999-us-west-2",
			region:   "us-east-1",
			want:     "spawn-schedules-999999999999-us-west-2",
		},
		{
			// Records written before the field existed. The template is right for
			// them: it is what they were created under, and an account on the
			// fallback path would already have been failing.
			name:     "empty falls back to the region template",
			recorded: "",
			region:   "ap-southeast-2",
			want:     "spawn-schedules-ap-southeast-2",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paramsBucket(c.recorded, c.region); got != c.want {
				t.Errorf("paramsBucket(%q, %q) = %q, want %q", c.recorded, c.region, got, c.want)
			}
		})
	}
}
