package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// connectIngress counts security-group reads for linuxConnectTransport.
type connectIngress struct {
	calls int
	ing   aws.PortIngress
	err   error
}

func (c *connectIngress) read(_ context.Context, port int32) (aws.PortIngress, error) {
	c.calls++
	ing := c.ing
	ing.Port = port
	return ing, c.err
}

// `spawn connect` on a Linux instance whose groups admit no SSH goes straight
// to SSM, without trying SSH (#1296), and says why.
func TestConnectGoesStraightToSSMWhenNoGroupAllowsSSH(t *testing.T) {
	r := &connectIngress{ing: aws.PortIngress{Groups: []string{"sg-0abc (default)"}}}
	tr, note, err := linuxConnectTransport(context.Background(), false, false, false, "203.0.113.10", 22, r.read)
	if err != nil || tr != connectTransportSSM {
		t.Fatalf("transport = %v, err = %v; want SSM", tr, err)
	}
	if r.calls != 1 {
		t.Errorf("read the groups %d time(s), want 1", r.calls)
	}
	for _, want := range []string{"tcp/22", "sg-0abc (default)", "--ssh"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q does not mention %q", note, want)
		}
	}
}

// Everything else keeps SSH: a group that may admit it, a read that failed
// (no evidence), and --ssh, which does not read the groups at all.
func TestConnectKeepsSSHUnlessThePortIsKnownClosed(t *testing.T) {
	for name, r := range map[string]*connectIngress{
		"open":       {ing: aws.PortIngress{Open: true}},
		"read error": {err: errors.New("UnauthorizedOperation")},
	} {
		tr, note, err := linuxConnectTransport(context.Background(), false, false, false, "203.0.113.10", 22, r.read)
		if err != nil || tr != connectTransportSSH || note != "" {
			t.Errorf("%s: transport = %v, note %q, err %v; want SSH, silently", name, tr, note, err)
		}
	}

	r := &connectIngress{}
	if tr, _, _ := linuxConnectTransport(context.Background(), false, true, false, "203.0.113.10", 22, r.read); tr != connectTransportSSH || r.calls != 0 {
		t.Errorf("--ssh: transport = %v after %d group read(s); want SSH with none", tr, r.calls)
	}
}

// --session-manager and no public IP are SSM, as before, without a group read.
func TestConnectSessionManagerAndNoPublicIPUseSSM(t *testing.T) {
	r := &connectIngress{ing: aws.PortIngress{Open: true}}
	if tr, _, _ := linuxConnectTransport(context.Background(), true, false, false, "203.0.113.10", 22, r.read); tr != connectTransportSSM {
		t.Errorf("--session-manager: transport = %v, want SSM", tr)
	}
	if tr, _, _ := linuxConnectTransport(context.Background(), false, false, false, "", 22, r.read); tr != connectTransportSSM {
		t.Errorf("no public IP: transport = %v, want SSM", tr)
	}
	if r.calls != 0 {
		t.Errorf("read the groups %d time(s) when the transport was already decided", r.calls)
	}
}

// The port checked is the one SSH would use (--port), and a one-shot command on
// a closed port is an immediate error — the interactive SSM session would
// ignore the command and sit on stdin.
func TestConnectChecksTheSSHPortAndRefusesOneShotWhenClosed(t *testing.T) {
	var asked int32
	read := func(_ context.Context, port int32) (aws.PortIngress, error) {
		asked = port
		return aws.PortIngress{Groups: []string{"sg-1 (x)"}}, nil
	}
	_, _, err := linuxConnectTransport(context.Background(), false, false, true, "203.0.113.10", 2222, read)
	if asked != 2222 {
		t.Errorf("checked port %d, want --port 2222", asked)
	}
	if err == nil || !strings.Contains(err.Error(), "tcp/2222") {
		t.Errorf("one-shot on a closed port: err = %v, want an immediate error naming tcp/2222", err)
	}
}

// runConnect must route the Linux decision through linuxConnectTransport and
// act on its SSM verdict. Anchored on the call line so a comment can't pass it.
func TestRunConnectUsesLinuxConnectTransport(t *testing.T) {
	src := readSourceFile(t, "connect.go")
	if !strings.Contains(src, "\ttransport, note, err := linuxConnectTransport(ctx, connectSessionMgr, connectSSH, len(args) > 1, instance.PublicIP, int32(connectPort),") {
		t.Error("runConnect does not decide the Linux transport with linuxConnectTransport")
	}
	if !strings.Contains(src, "\tif transport == connectTransportSSM {\n\t\treturn connectViaSessionManager(instance.InstanceID, instance.Region)") {
		t.Error("runConnect does not go to Session Manager on an SSM verdict")
	}
}
