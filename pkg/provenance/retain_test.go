package provenance

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeTags struct {
	objs map[string]map[string]string
	puts []*s3.PutObjectTaggingInput
}

func (f *fakeTags) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	for k := range f.objs {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
		}
	}
	return out, nil
}

func (f *fakeTags) GetObjectTagging(_ context.Context, in *s3.GetObjectTaggingInput, _ ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error) {
	out := &s3.GetObjectTaggingOutput{VersionId: aws.String("v-" + aws.ToString(in.Key))}
	for k, v := range f.objs[aws.ToString(in.Key)] {
		out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fakeTags) PutObjectTagging(_ context.Context, in *s3.PutObjectTaggingInput, _ ...func(*s3.Options)) (*s3.PutObjectTaggingOutput, error) {
	f.puts = append(f.puts, in)
	m := map[string]string{}
	for _, t := range in.Tagging.TagSet {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.objs[aws.ToString(in.Key)] = m
	return &s3.PutObjectTaggingOutput{}, nil
}

func spawnOut(extra map[string]string) map[string]string {
	m := map[string]string{TagRunID: "r1", TagClass: "cheap", TagUSD: "0.0100", TagHours: "0.1000"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// PutObjectTagging replaces the whole set, so setting spawn:retain must write
// every other tag back: losing spawn:regen-class would silently take the
// object out of the rules, and losing a user's own tag would be worse.
func TestSetRetainKeepsEveryOtherTag(t *testing.T) {
	f := &fakeTags{objs: map[string]map[string]string{"out/a": spawnOut(map[string]string{"lab:project": "x", TagRetain: "false"})}}
	res, err := SetRetain(context.Background(), f, "b", "out/a", true)
	if err != nil || len(res) != 1 || res[0].Outcome != RetainSet {
		t.Fatalf("SetRetain = %+v, %v", res, err)
	}
	got := f.objs["out/a"]
	want := spawnOut(map[string]string{"lab:project": "x", TagRetain: "true"})
	if len(got) != len(want) {
		t.Errorf("tags after = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tag %s = %q after, want %q", k, got[k], v)
		}
	}
	if aws.ToString(f.puts[0].VersionId) != "v-out/a" {
		t.Errorf("wrote tags to version %q, not the version they were read from", aws.ToString(f.puts[0].VersionId))
	}
}

// --off on an output written before spawn:retain existed adds the tag, which
// is how a researcher opts old outputs in to tiering (Decisions 7).
func TestSetRetainOffOptsInAnOldOutput(t *testing.T) {
	f := &fakeTags{objs: map[string]map[string]string{"out/old": spawnOut(nil)}}
	res, _ := SetRetain(context.Background(), f, "b", "out/old", false)
	if res[0].Outcome != RetainSet || f.objs["out/old"][TagRetain] != "false" {
		t.Errorf("old output: %+v, tags %v", res, f.objs["out/old"])
	}
}

// Only spawn outputs are touched; an object with no spawn:run-id is reported
// and left alone, and an object already as asked isn't rewritten.
func TestSetRetainPrefixTouchesOnlyWhatItShould(t *testing.T) {
	f := &fakeTags{objs: map[string]map[string]string{
		"out/a":       spawnOut(nil),
		"out/b":       spawnOut(map[string]string{TagRetain: "true"}),
		"out/notes":   {"lab:project": "x"},
		"other/thing": spawnOut(nil),
	}}
	res, err := SetRetain(context.Background(), f, "b", "out/", true)
	if err != nil {
		t.Fatal(err)
	}
	outcome := map[string]string{}
	for _, r := range res {
		outcome[r.Key] = r.Outcome
	}
	if outcome["out/a"] != RetainSet || outcome["out/b"] != RetainUnchanged || outcome["out/notes"] != RetainNotOutput {
		t.Errorf("outcomes = %v", outcome)
	}
	if _, listed := outcome["other/thing"]; listed {
		t.Error("an object outside the prefix was considered")
	}
	if len(f.puts) != 1 || aws.ToString(f.puts[0].Key) != "out/a" {
		t.Errorf("wrote tags on %d objects; only out/a needed it", len(f.puts))
	}
	if len(f.objs["out/notes"]) != 1 {
		t.Errorf("a non-spawn object's tags were changed: %v", f.objs["out/notes"])
	}
}

func TestSetRetainRefusesMoreThanTenTags(t *testing.T) {
	full := spawnOut(map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6"})
	f := &fakeTags{objs: map[string]map[string]string{"k": full}}
	res, _ := SetRetain(context.Background(), f, "b", "k", true)
	if res[0].Outcome != RetainError || len(f.puts) != 0 {
		t.Errorf("an 11th tag was written or not reported: %+v", res)
	}
}

func TestSetRetainEmptyPrefixIsAnError(t *testing.T) {
	if _, err := SetRetain(context.Background(), &fakeTags{objs: map[string]map[string]string{}}, "b", "nothing/", true); err == nil {
		t.Error("an empty prefix must be an error, not a silent success")
	}
}
