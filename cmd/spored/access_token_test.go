package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/accesstoken"
)

// The command spawn runs over SSM (accesstoken.AddCommand) is accepted by
// spored's own subcommand: the two halves of the wire contract agree.
func TestAccessTokenAddAcceptsWhatSpawnSends(t *testing.T) {
	store := &accesstoken.Store{Dir: t.TempDir()}
	tok, _ := accesstoken.Mint()
	line, err := accesstoken.AddCommand(accesstoken.KindWeb, accesstoken.Hash(tok), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(line)
	if fields[0] != accesstoken.SporedPath {
		t.Fatalf("command runs %q, not spored", fields[0])
	}
	cmd := newAccessTokenCmdWith(store)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(fields[2:]) // after "spored access-token"
	if err := cmd.Execute(); err != nil {
		t.Fatalf("spored access-token %v: %v", fields[2:], err)
	}
	if ok, _ := store.Verify(accesstoken.KindWeb, tok); !ok {
		t.Error("the registered token does not verify")
	}
	if strings.Contains(out.String(), tok) || strings.Contains(out.String(), accesstoken.Hash(tok)) {
		t.Errorf("output echoes the token or hash: %q", out.String())
	}

	rev := newAccessTokenCmdWith(store)
	rev.SetOut(&out)
	rev.SetArgs([]string{"revoke-all"})
	if err := rev.Execute(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.Verify(accesstoken.KindWeb, tok); ok {
		t.Error("revoke-all left the token valid")
	}
}

func TestAccessTokenAddRejectsBadInput(t *testing.T) {
	store := &accesstoken.Store{Dir: t.TempDir()}
	for _, args := range [][]string{
		{"add", "--kind", "dcv"},
		{"add", "--kind", "ssh", "--sha256", strings.Repeat("a", 64)},
		{"add", "--kind", "dcv", "--sha256", "abc"},
		{"add", "--kind", "dcv", "--sha256", strings.Repeat("a", 64), "--ttl", "9999h"},
	} {
		cmd := newAccessTokenCmdWith(store)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("spored access-token %v: accepted", args)
		}
	}
}
