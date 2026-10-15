package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// seedWithoutAWS runs seedDetachedLaunchKeys with no client: any attempt to
// resolve an AMI dereferences it, which is reported as "tried to resolve".
func seedWithoutAWS(t *testing.T, pf *ParamFileFormat) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("seeding tried to resolve an AMI the file already supplies: %v", r)
		}
	}()
	if err := seedDetachedLaunchKeys(context.Background(), nil, pf, &aws.LaunchConfig{}, "us-west-2"); err != nil {
		t.Fatalf("seedDetachedLaunchKeys: %v", err)
	}
}

// A file-level `defaults: ami:` must reach every row that has no ami of its own.
// The orchestrator merges each row over the defaults, so a per-row AMI resolved
// here would silently replace the user's — and `spawn schedule create` passes no
// --ami, so this is the only way a scheduled sweep can name one (#1026).
func TestSeedDetachedLaunchKeys_FileDefaultAMIIsNotOverriddenPerRow(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-from-file"},
		Params:   []map[string]interface{}{{"trial": "a"}, {"trial": "b", "ami": "ami-row"}},
	}
	seedWithoutAWS(t, pf)
	if got, ok := pf.Params[0]["ami"]; ok {
		t.Errorf("row 0 was given ami %v, which overrides the file's default", got)
	}
	if got := pf.Params[1]["ami"]; got != "ami-row" {
		t.Errorf("row 1 ami = %v, want its own", got)
	}
}

// What `spawn schedule create` uploads is the seeded params as JSON, carrying
// the schedule's region — not the user's file verbatim (#1026).
func TestSeedScheduleParams_UploadsSeededJSON(t *testing.T) {
	p := &parsedParams{
		defaults: map[string]interface{}{"instance_type": "t3.micro"},
		params:   []map[string]interface{}{{"trial": "a", "ami": "ami-row"}},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("seeding tried to resolve an AMI the row already supplies: %v", r)
		}
	}()
	data, err := seedScheduleParams(context.Background(), nil, p, "eu-west-1")
	if err != nil {
		t.Fatalf("seedScheduleParams: %v", err)
	}
	var got ParamFileFormat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	if got.Defaults["region"] != "eu-west-1" {
		t.Errorf("defaults.region = %v, want the schedule's region", got.Defaults["region"])
	}
	if len(got.Params) != 1 || got.Params[0]["ami"] != "ami-row" {
		t.Errorf("params = %+v", got.Params)
	}
}
