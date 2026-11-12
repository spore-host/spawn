package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/accesstoken"
	"github.com/spore-host/spawn/pkg/aws"
)

// recordingRunner is a shellRunner that records each script and answers from
// a queue.
type recordingRunner struct {
	scripts []string
	results []*aws.SSMRunResult
	errs    []error
}

func (r *recordingRunner) run(_ context.Context, _, _, script string) (*aws.SSMRunResult, error) {
	r.scripts = append(r.scripts, script)
	i := len(r.scripts) - 1
	var res *aws.SSMRunResult
	var err error
	if i < len(r.results) {
		res = r.results[i]
	}
	if i < len(r.errs) {
		err = r.errs[i]
	}
	return res, err
}

var ssmOK = &aws.SSMRunResult{Status: "Success", Stdout: "access-token registered"}

// The token comes back to the caller; only its hash goes over SSM.
func TestIssueAccessTokenSendsOnlyTheHash(t *testing.T) {
	r := &recordingRunner{results: []*aws.SSMRunResult{ssmOK}}
	tok, err := issueAccessTokenWith(context.Background(), r.run, "us-east-1", "i-1", accesstoken.KindDCV, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.scripts) != 1 {
		t.Fatalf("ran %d scripts, want 1", len(r.scripts))
	}
	if strings.Contains(r.scripts[0], tok) {
		t.Fatalf("the SSM command carries the token: %q", r.scripts[0])
	}
	if !strings.Contains(r.scripts[0], accesstoken.Hash(tok)) {
		t.Errorf("the SSM command does not register this token's hash: %q", r.scripts[0])
	}
}

func TestIssueAccessTokenRetriesUntilSSMIsUp(t *testing.T) {
	r := &recordingRunner{
		results: []*aws.SSMRunResult{nil, nil, ssmOK},
		errs:    []error{errors.New("InvalidInstanceId"), errors.New("InvalidInstanceId"), nil},
	}
	if _, err := issueAccessTokenWith(context.Background(), r.run, "r", "i-1", accesstoken.KindWeb, time.Hour, 0); err != nil {
		t.Fatalf("gave up after %d attempts: %v", len(r.scripts), err)
	}
}

func TestIssueAccessTokenOlderSpored(t *testing.T) {
	r := &recordingRunner{results: []*aws.SSMRunResult{{Status: "Failed", ResponseCode: 1, Stderr: `Error: unknown command "access-token" for "spored"`}}}
	_, err := issueAccessTokenWith(context.Background(), r.run, "r", "i-1", accesstoken.KindWeb, time.Hour, 0)
	if !errors.Is(err, errAgentPredatesAccessTokens) {
		t.Errorf("err = %v, want errAgentPredatesAccessTokens", err)
	}
	if len(r.scripts) != 1 {
		t.Errorf("retried an answer (%d runs); only an unreachable SSM is retried", len(r.scripts))
	}
}

func TestSessionToken(t *testing.T) {
	ctx := context.Background()
	var w bytes.Buffer

	r := &recordingRunner{results: []*aws.SSMRunResult{ssmOK}}
	tok, err := sessionToken(ctx, r.run, "r", "i-1", dcvScanResult{status: "ready", auth: readyAuthToken}, accesstoken.KindDCV, time.Hour, &w)
	if err != nil || tok == "" || len(r.scripts) != 1 {
		t.Errorf("auth=token: tok=%q err=%v runs=%d, want a registered token", tok, err, len(r.scripts))
	}

	r = &recordingRunner{}
	tok, err = sessionToken(ctx, r.run, "r", "i-1", dcvScanResult{status: "ready", auth: readyAuthNone}, accesstoken.KindWeb, time.Hour, &w)
	if err != nil || tok != "" || len(r.scripts) != 0 {
		t.Errorf("auth=none: tok=%q err=%v runs=%d, want no token and no SSM", tok, err, len(r.scripts))
	}

	// An older spored: its published token, with a warning, and no SSM.
	tok, err = sessionToken(ctx, r.run, "r", "i-1", dcvScanResult{legacyToken: "old"}, accesstoken.KindDCV, time.Hour, &w)
	if err != nil || tok != "old" || len(r.scripts) != 0 {
		t.Errorf("legacy: tok=%q err=%v runs=%d", tok, err, len(r.scripts))
	}
	if !strings.Contains(w.String(), "upgrade-spored") {
		t.Errorf("legacy: no upgrade warning in %q", w.String())
	}
}

func TestAppURLFor(t *testing.T) {
	ctx := context.Background()
	inst := func(tags map[string]string) *aws.InstanceInfo {
		return &aws.InstanceInfo{InstanceID: "i-1", Region: "us-east-1", State: "running", PublicIP: "1.2.3.4", Tags: tags}
	}

	r := &recordingRunner{results: []*aws.SSMRunResult{ssmOK}}
	link, err := appURLFor(ctx, r.run, inst(map[string]string{
		"spawn:dcv-session-id": "console",
		"spawn:ready-status":   "ready",
		"spawn:ready-auth":     "token",
		"spawn:ready-url":      "https://box.x.spore.host:8443/#console",
	}), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://box.x.spore.host:8443/?authToken=") || !strings.HasSuffix(link, "#console") {
		t.Errorf("DCV link = %q", link)
	}
	tok := link[strings.Index(link, "=")+1 : strings.Index(link, "#")]
	if !strings.Contains(r.scripts[0], "--kind dcv --sha256 "+accesstoken.Hash(tok)) {
		t.Errorf("registered %q, not this link's token", r.scripts[0])
	}

	r = &recordingRunner{results: []*aws.SSMRunResult{ssmOK}}
	link, err = appURLFor(ctx, r.run, inst(map[string]string{
		"spawn:ready-status": "ready",
		"spawn:ready-auth":   "token",
		"spawn:ready-url":    "https://web.x.spore.host/",
	}), time.Hour)
	if err != nil || !strings.HasPrefix(link, "https://web.x.spore.host/?spore_token=") || !strings.Contains(r.scripts[0], "--kind web") {
		t.Errorf("web link = %q err = %v scripts = %v", link, err, r.scripts)
	}

	if _, err := appURLFor(ctx, r.run, inst(map[string]string{"spawn:ready-status": "dcv-waiting"}), time.Hour); err == nil {
		t.Error("a link for a session not yet ready")
	}
	stopped := inst(map[string]string{"spawn:ready-status": "ready", "spawn:ready-auth": "token"})
	stopped.State = "stopped"
	if _, err := appURLFor(ctx, r.run, stopped, time.Hour); err == nil {
		t.Error("a link for a stopped instance")
	}
}
