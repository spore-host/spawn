package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// recordingReload is a reloadTransport whose SSH and SSM halves record what
// they were asked to run.
type recordingReload struct {
	keyErr   error
	sshErr   error
	ssmErr   error
	ssmRes   *aws.SSMRunResult
	sshCalls [][]string
	ssmCalls []string // "region|instance|command"
}

func (r *recordingReload) transport() reloadTransport {
	return reloadTransport{
		findKey: func(string) (string, error) {
			if r.keyErr != nil {
				return "", r.keyErr
			}
			return "/home/u/.ssh/k.pem", nil
		},
		runSSH: func(_ context.Context, args []string) error {
			r.sshCalls = append(r.sshCalls, args)
			return r.sshErr
		},
		runSSM: func(_ context.Context, region, id, command string) (*aws.SSMRunResult, error) {
			r.ssmCalls = append(r.ssmCalls, region+"|"+id+"|"+command)
			return r.ssmRes, r.ssmErr
		},
	}
}

func keylessTaskInstance() *aws.InstanceInfo {
	return &aws.InstanceInfo{InstanceID: "i-0abc", Region: "us-west-2", State: "running", PublicIP: "1.2.3.4",
		Tags: map[string]string{"spawn:managed": "true"}}
}

// A `spawn task run` instance has no launch key on disk (#1113). The reload
// must reach spored over SSM, running the same `spored reload` the SSH path
// runs, and must not attempt SSH at all.
func TestTriggerReload_KeylessInstanceReloadsOverSSM(t *testing.T) {
	r := &recordingReload{keyErr: errors.New("no key name"), ssmRes: &aws.SSMRunResult{Status: "Success"}}
	if err := r.transport().triggerReload(context.Background(), keylessTaskInstance()); err != nil {
		t.Fatalf("keyless reload over SSM should succeed, got %v", err)
	}
	if len(r.sshCalls) != 0 {
		t.Errorf("SSH must not be attempted without a key, got %v", r.sshCalls)
	}
	want := "us-west-2|i-0abc|sudo /usr/local/bin/spored reload"
	if len(r.ssmCalls) != 1 || r.ssmCalls[0] != want {
		t.Fatalf("SSM calls = %q, want exactly [%q]", r.ssmCalls, want)
	}
}

// A key on disk but an SSH failure (private subnet, wrong user, sshd down)
// is no reason to leave the new limit unapplied: fall back to SSM.
func TestTriggerReload_SSHFailureFallsBackToSSM(t *testing.T) {
	r := &recordingReload{sshErr: errors.New("connection timed out"), ssmRes: &aws.SSMRunResult{Status: "Success"}}
	if err := r.transport().triggerReload(context.Background(), keylessTaskInstance()); err != nil {
		t.Fatalf("SSM fallback should succeed, got %v", err)
	}
	if len(r.sshCalls) != 1 || len(r.ssmCalls) != 1 {
		t.Fatalf("want one SSH attempt then one SSM attempt, got ssh=%d ssm=%d", len(r.sshCalls), len(r.ssmCalls))
	}
}

// When SSH works, SSM is not also sent.
func TestTriggerReload_SSHSuccessDoesNotSendSSM(t *testing.T) {
	r := &recordingReload{}
	if err := r.transport().triggerReload(context.Background(), keylessTaskInstance()); err != nil {
		t.Fatal(err)
	}
	if len(r.sshCalls) != 1 || len(r.ssmCalls) != 0 {
		t.Fatalf("want ssh=1 ssm=0, got ssh=%d ssm=%d", len(r.sshCalls), len(r.ssmCalls))
	}
	if got := strings.Join(r.sshCalls[0], " "); !strings.HasSuffix(got, " "+reloadSporedCommand) {
		t.Errorf("SSH argv must end with the reload command, got %q", got)
	}
}

// An SSM invocation that ran but failed on the box (spored missing, non-zero
// exit) is a failed reload, not a success.
func TestTriggerReload_SSMNonZeroExitIsAFailure(t *testing.T) {
	for name, res := range map[string]*aws.SSMRunResult{
		"failed status": {Status: "Failed", ResponseCode: 1, Stderr: "spored: not found"},
		"nonzero exit":  {Status: "Success", ResponseCode: 127},
		"nil result":    nil,
	} {
		r := &recordingReload{keyErr: errors.New("no key"), ssmRes: res}
		if err := r.transport().triggerReload(context.Background(), keylessTaskInstance()); err == nil {
			t.Errorf("%s: want an error, got nil", name)
		}
	}
}

// Both paths failing must make `spawn extend` exit non-zero (#1113): the tags
// already say the new limit, spored is enforcing the old one, and a warning on
// a zero exit is the silent no-op #581 described.
func TestReloadAfterExtend_BothPathsFailingIsANonZeroExit(t *testing.T) {
	r := &recordingReload{keyErr: errors.New("no key"), ssmErr: errors.New("InvalidInstanceId: not online")}
	tr := r.transport()
	reload := func(inst *aws.InstanceInfo) error { return tr.triggerReload(context.Background(), inst) }
	var out, errw bytes.Buffer
	err := reloadAfterExtend(keylessTaskInstance(), reload, &out, &errw)
	if err == nil {
		t.Fatal("reload that reached no path must return an error (non-zero exit)")
	}
	for _, want := range []string{"SSH:", "SSM:", "not online"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
	if strings.Contains(out.String(), "reloaded") {
		t.Errorf("must not claim a reload happened, stdout=%q", out.String())
	}
	if len(r.ssmCalls) != 1 {
		t.Errorf("SSM must have been tried before giving up, got %d calls", len(r.ssmCalls))
	}
}

func TestReloadAfterExtend_SuccessIsZeroExit(t *testing.T) {
	var out, errw bytes.Buffer
	if err := reloadAfterExtend(keylessTaskInstance(), func(*aws.InstanceInfo) error { return nil }, &out, &errw); err != nil {
		t.Fatal(err)
	}
}

// A job array where any member was tagged but not reloaded exits non-zero too.
func TestJobArrayExtendResult_ReloadFailureIsNonZero(t *testing.T) {
	if jobArrayExtendResult(nil, nil) != nil {
		t.Error("all reloaded: want nil")
	}
	if jobArrayExtendResult(nil, []string{"i-1"}) == nil {
		t.Error("a reload failure must be a non-zero exit")
	}
	if jobArrayExtendResult([]string{"i-1"}, nil) == nil {
		t.Error("a tag failure must be a non-zero exit")
	}
}
