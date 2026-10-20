package reaperdeploy

import (
	"context"
	"strings"
	"testing"
)

func deployWith(t *testing.T, ev *fakeEvents, logs *fakeLogs, exists bool, opts Options) *Result {
	t.Helper()
	d := newTestDeployer(&fakeIAM{roleExists: exists}, &fakeLambda{exists: exists, dryRun: "true"}, ev, &fakeS3{bucketExists: exists})
	d.Logs, d.UpdatePoll = logs, -1
	opts.AccountID, opts.Region, opts.Version, opts.Artifact = "1", "us-east-1", "0.137.0", "/tmp/x.zip"
	res, err := d.Deploy(context.Background(), opts)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	return res
}

// #1214: a re-deploy without --schedule or --log-retention-days reset both to
// their defaults. Like #1154's settings, the live values must survive.
func TestRedeployKeepsScheduleAndRetention(t *testing.T) {
	ev := &fakeEvents{schedule: "rate(1 hour)"}
	logs := &fakeLogs{groupExists: true, retention: 90}
	deployWith(t, ev, logs, true, Options{})
	if ev.putSchedule != "rate(1 hour)" {
		t.Errorf("re-deploy wrote schedule %q, want the live rate(1 hour)", ev.putSchedule)
	}
	if logs.retention != 90 {
		t.Errorf("re-deploy changed retention to %d, want the live 90", logs.retention)
	}
}

func TestRedeployExplicitScheduleAndRetentionWin(t *testing.T) {
	ev := &fakeEvents{schedule: "rate(1 hour)"}
	logs := &fakeLogs{groupExists: true, retention: 90}
	deployWith(t, ev, logs, true, Options{Schedule: "rate(5 minutes)", LogRetentionDays: 14})
	if ev.putSchedule != "rate(5 minutes)" || logs.retention != 14 {
		t.Errorf("schedule %q retention %d, want the explicit rate(5 minutes) and 14", ev.putSchedule, logs.retention)
	}
}

// Defaults apply only where there is nothing live: a fresh deploy, or a group
// that has no retention (the one Lambda creates itself, #827).
func TestDeployDefaultsOnlyWithoutALiveValue(t *testing.T) {
	ev, logs := &fakeEvents{}, &fakeLogs{}
	res := deployWith(t, ev, logs, false, Options{})
	if ev.putSchedule != DefaultSchedule || logs.retention != DefaultLogRetentionDays {
		t.Errorf("fresh deploy: schedule %q retention %d, want the defaults", ev.putSchedule, logs.retention)
	}
	if !strings.Contains(strings.Join(res.Actions, "\n"), DefaultSchedule) {
		t.Errorf("deploy report should name the schedule: %v", res.Actions)
	}

	unretained := &fakeLogs{groupExists: true}
	deployWith(t, &fakeEvents{schedule: "rate(1 hour)"}, unretained, true, Options{})
	if unretained.retention != DefaultLogRetentionDays {
		t.Errorf("an existing group with no retention got %d, want the %d-day default", unretained.retention, DefaultLogRetentionDays)
	}
}
