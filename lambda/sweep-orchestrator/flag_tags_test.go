package main

import (
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
)

// TestMemberKeepsTheCLIsTeamAndUserTags is #1356's Lambda half: the
// `spawn launch --tag` and `--team` tags the CLI renders into a detached
// member's spawn_tags (TestSweepMembersCarryTagAndTeamFlags in cmd/) are the
// member's, unchanged. Only the tags this Lambda owns are overridden.
func TestMemberKeepsTheCLIsTeamAndUserTags(t *testing.T) {
	flags := map[string]string{
		"project":         "p1",
		"cost-center":     "cc",
		"spawn:team-id":   "team-1",
		"spawn:team-name": "Team One",
	}
	state := &SweepRecord{SweepID: "sw-20261026", SweepName: "sw"}
	tags, err := memberTags(state, map[string]interface{}{tagsParam: cliTags(flags)}, 3, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tg := range tags {
		got[awssdk.ToString(tg.Key)] = awssdk.ToString(tg.Value)
	}
	for k, v := range flags {
		if got[k] != v {
			t.Errorf("member tag %s = %q, want the CLI's %q", k, got[k], v)
		}
	}
}
