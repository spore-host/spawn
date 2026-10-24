package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/spore-host/spawn/pkg/reaperconfig"
	"github.com/spore-host/spawn/pkg/tagprefix"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// REAPER_TTL_GRACE (#1186). spored enforces the TTL deadline itself and runs
// the task flush on the way out; a reaper cycle landing in that window
// terminated the instance first and the task record was lost. So a RUNNING
// instance whose spored is demonstrably alive (fresh heartbeat) is left up to
// the grace past its deadline. Everything else is reaped at the deadline, as
// the header promises: a stale or missing heartbeat means the reaper is the
// only enforcer, and a stopped instance runs no spored at all (#71).
func TestTTLGraceOnlyForARunningInstanceWhoseSporedIsAlive(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 5, 0, 0, time.UTC)
	const grace = 10 * time.Minute
	hb := func(age time.Duration) string { return now.Add(-age).Format(time.RFC3339) }

	cases := []struct {
		name        string
		state       ec2types.InstanceStateName
		pastDL      time.Duration // how far past the deadline now is
		heartbeat   string        // "" = no tag
		grace       time.Duration
		fallbackTTL bool // use spawn:launch-time + spawn:ttl instead of spawn:ttl-deadline
		launchAge   time.Duration
		wantReaped  bool
	}{
		{"fresh heartbeat, inside the grace: left to spored", ec2types.InstanceStateNameRunning, 2 * time.Minute, hb(time.Minute), grace, false, 0, false},
		{"stale heartbeat: reaped at the deadline", ec2types.InstanceStateNameRunning, 2 * time.Minute, hb(2 * time.Hour), grace, false, 0, true},
		{"no heartbeat: reaped at the deadline", ec2types.InstanceStateNameRunning, 2 * time.Minute, "", grace, false, 0, true},
		{"stopped, even with a fresh heartbeat: reaped", ec2types.InstanceStateNameStopped, 2 * time.Minute, hb(time.Minute), grace, false, 0, true},
		{"fresh heartbeat, past the grace: reaped", ec2types.InstanceStateNameRunning, grace + time.Minute, hb(time.Minute), grace, false, 0, true},
		{"grace 0: reaped at the deadline", ec2types.InstanceStateNameRunning, 2 * time.Minute, hb(time.Minute), 0, false, 0, true},
		{"launch-time + ttl deadline, fresh heartbeat: left to spored", ec2types.InstanceStateNameRunning, 2 * time.Minute, hb(time.Minute), grace, true, 0, false},
		// The control for the case above: without it, a fallback deadline that
		// failed to parse would also read as "spared".
		{"launch-time + ttl deadline, grace 0: reaped", ec2types.InstanceStateNameRunning, 2 * time.Minute, hb(time.Minute), 0, true, 0, true},
		{"inside the grace but past max-age: reaped", ec2types.InstanceStateNameRunning, 2 * time.Minute, hb(time.Minute), grace, false, 8 * 24 * time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deadline := now.Add(-tc.pastDL)
			tags := []ec2types.Tag{tag(tagprefix.Tag("managed"), "true")}
			launch := now.Add(-2 * time.Hour)
			if tc.launchAge > 0 {
				launch = now.Add(-tc.launchAge)
			}
			if tc.fallbackTTL {
				ttl := deadline.Sub(launch)
				tags = append(tags, tag(tagprefix.Tag("launch-time"), launch.Format(time.RFC3339)),
					tag(tagprefix.Tag("ttl"), ttl.String()))
			} else {
				tags = append(tags, tag(tagprefix.Tag("ttl-deadline"), deadline.Format(time.RFC3339)))
			}
			if tc.heartbeat != "" {
				tags = append(tags, tag(tagprefix.Tag("last-heartbeat"), tc.heartbeat))
			}
			inst := ec2types.Instance{
				InstanceId: aws.String("i-task"),
				State:      &ec2types.InstanceState{Name: tc.state},
				LaunchTime: aws.Time(launch),
				Tags:       tags,
			}
			r := &reaper{maxAge: 7 * 24 * time.Hour, ttlGrace: tc.grace}
			if _, expired := r.evaluate(inst, "us-west-2", now); expired != tc.wantReaped {
				t.Errorf("expired = %v, want %v", expired, tc.wantReaped)
			}
		})
	}
}

// An unset REAPER_TTL_GRACE means the documented default, and a value the CLI
// would reject falls back to it rather than to "no grace" or a huge one.
func TestParseTTLGraceAgreesWithReaperconfig(t *testing.T) {
	for _, v := range []string{"", "0", "5m", "10m", "1h", "61m", "2h", "-1m", "soon"} {
		d, err := reaperconfig.ParseTTLGrace(v)
		got := parseTTLGrace(v)
		switch {
		case err == nil && got != d:
			t.Errorf("REAPER_TTL_GRACE=%q: CLI accepts it as %s, reaper reads %s", v, d, got)
		case err != nil && got != reaperconfig.DefaultTTLGrace:
			t.Errorf("REAPER_TTL_GRACE=%q: CLI rejects it, reaper reads %s instead of the default", v, got)
		}
	}
}

// taskFlushEC2 serves expired instances and records terminates into a shared
// event log, so the order against the SSM flush is visible.
type taskFlushEC2 struct {
	insts  []ec2types.Instance
	events *[]string
}

func (f *taskFlushEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: f.insts}}}, nil
}

func (f *taskFlushEC2) CancelSpotInstanceRequests(context.Context, *ec2.CancelSpotInstanceRequestsInput, ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (f *taskFlushEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	*f.events = append(*f.events, "terminate:"+strings.Join(in.InstanceIds, ","))
	return &ec2.TerminateInstancesOutput{}, nil
}

// taskFlushSSM records SendCommand and reports every invocation a success.
type taskFlushSSM struct {
	events   *[]string
	commands []string
}

func (f *taskFlushSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	*f.events = append(*f.events, "ssm:"+strings.Join(in.InstanceIds, ","))
	f.commands = append(f.commands, strings.Join(in.Parameters["commands"], "\n"))
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd-1")}}, nil
}

func (f *taskFlushSSM) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	return &ssm.GetCommandInvocationOutput{Status: ssmtypes.CommandInvocationStatusSuccess}, nil
}

// TestReaperFlushesOnlyTaskInstancesBeforeTerminating (#1186): a task the
// reaper ends gets the same command.log and completion record a spored exit
// would, by running the flush hook over SSM before the terminate. Only a
// RUNNING instance tagged spawn:task-id: a plain launch has no hook, and a
// stopped instance has no agent to receive the command.
func TestReaperFlushesOnlyTaskInstancesBeforeTerminating(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	mk := func(id string, state ec2types.InstanceStateName, taskID string) ec2types.Instance {
		tags := []ec2types.Tag{tag("spawn:managed", "true"), tag("spawn:ttl-deadline", past)}
		if taskID != "" {
			tags = append(tags, tag("spawn:task-id", taskID))
		}
		return ec2types.Instance{InstanceId: aws.String(id), State: &ec2types.InstanceState{Name: state}, Tags: tags}
	}
	var events []string
	e := &taskFlushEC2{events: &events, insts: []ec2types.Instance{
		mk("i-task", ec2types.InstanceStateNameRunning, "cookbook-su2-real-probe0-b"),
		mk("i-plain", ec2types.InstanceStateNameRunning, ""),
		mk("i-task-stopped", ec2types.InstanceStateNameStopped, "stopped-task"),
	}}
	s := &taskFlushSSM{events: &events}
	r := &reaper{
		accounts: []account{{label: "acct", accountID: "111111111111",
			ec2For: func(string) ec2API { return e },
			ssmFor: func(string) ssmAPI { return s },
		}},
		regions:       []string{"us-west-2"},
		maxAge:        defaultMaxAge,
		taskFlushPoll: time.Millisecond,
	}
	sum, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Reaped != 3 {
		t.Fatalf("reaped %d, want 3 — the flush must never stand between an expired instance and its terminate (events %v)", sum.Reaped, events)
	}
	want := "ssm:i-task,terminate:i-task,terminate:i-plain,terminate:i-task-stopped"
	if got := strings.Join(events, ","); got != want {
		t.Errorf("calls = %s\nwant    %s", got, want)
	}
	if len(s.commands) == 1 && s.commands[0] != taskproto.FlushCommand(taskproto.ExitTTLExpired) {
		t.Errorf("the reaper sent a different command from the shared, trust-checked one:\n%s", s.commands[0])
	}
}
