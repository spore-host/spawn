package main

import (
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func connIDs(cs []Connection) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ConnectionID)
	}
	return out
}

// A sweep record goes to its owner's connections only, and a record with no
// user_id goes to nobody (#1600): it used to be broadcast to every connection.
func TestRecipientsForFailsClosed(t *testing.T) {
	conns := []Connection{
		{ConnectionID: "alice-1", UserID: "arn:aws:iam::111111111111:user/alice"},
		{ConnectionID: "bob-1", UserID: "arn:aws:iam::222222222222:user/bob"},
		{ConnectionID: "alice-2", UserID: "arn:aws:iam::111111111111:user/alice"},
		{ConnectionID: "anon", UserID: ""},
	}
	for _, tc := range []struct {
		name, user string
		want       []string
	}{
		{"owner", "arn:aws:iam::111111111111:user/alice", []string{"alice-1", "alice-2"}},
		{"other owner", "arn:aws:iam::222222222222:user/bob", []string{"bob-1"}},
		{"no user_id goes to nobody", "", nil},
		{"unknown owner", "arn:aws:iam::333333333333:user/carol", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := connIDs(recipientsFor(conns, tc.user))
			if len(got) != len(tc.want) {
				t.Fatalf("recipients %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("recipients %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The user_id the filter sees is the record's own: a sweep INSERT without one
// parses to "", which recipientsFor turns into no recipients.
func TestParseSweepEventWithoutUserID(t *testing.T) {
	rec := events.DynamoDBEventRecord{
		EventName:      "INSERT",
		EventSourceArn: "arn:aws:dynamodb:us-east-1:111111111111:table/spawn-sweep-orchestration/stream/x",
		Change: events.DynamoDBStreamRecord{NewImage: map[string]events.DynamoDBAttributeValue{
			"sweep_id":       events.NewStringAttribute("s1"),
			"aws_account_id": events.NewStringAttribute("111111111111"),
		}},
	}
	typ, _, user, err := parseStreamRecord(rec)
	if err != nil || typ != "sweep:update" {
		t.Fatalf("parse: type %q err %v", typ, err)
	}
	if user != "" {
		t.Fatalf("user %q, want empty", user)
	}
	if got := recipientsFor([]Connection{{ConnectionID: "c", UserID: "u"}}, user); len(got) != 0 {
		t.Fatalf("an ownerless record reached %v", connIDs(got))
	}
}
