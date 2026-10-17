// Package provenance records what a task output would cost to regenerate, and
// what produced it, so retention can follow from cost (#1076, design in
// docs/recompute-tag-design.md).
//
// Phase 1 is RECORD ONLY. It writes tags and a provenance annotation on task
// outputs and fills completion.json's cost_estimate. Nothing here tiers or
// deletes anything.
package provenance

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Class is the spawn:regen-class tag: what S3 lifecycle rules match on. They
// can only match a tag by its exact value, so the number has to be bucketed here.
type Class string

const (
	ClassCheap     Class = "cheap"
	ClassModerate  Class = "moderate"
	ClassExpensive Class = "expensive"
)

// Thresholds, decided by the owner on 2026-10-17 (design, Decisions 1). Both
// money and wall-clock count: a cheap but slow job is still a researcher's day.
const (
	CheapMaxUSD       = 1.0  // cheap: < $1 ...
	CheapMaxHours     = 1.0  // ... AND < 1 h
	ExpensiveMinUSD   = 25.0 // expensive: >= $25 ...
	ExpensiveMinHours = 12.0 // ... OR >= 12 h
)

// Classify returns the class for a run's cost and duration. ok is false when the
// rate is unknown: an unclassified object matches no rule, so "we don't know
// what it cost" means kept, never cheap.
func Classify(c Cost) (Class, bool) {
	if !c.RateKnown() {
		return "", false
	}
	h := c.Hours()
	switch {
	case c.USD >= ExpensiveMinUSD || h >= ExpensiveMinHours:
		return ClassExpensive, true
	case c.USD < CheapMaxUSD && h < CheapMaxHours:
		return ClassCheap, true
	default:
		return ClassModerate, true
	}
}

// Rate sources. A dollar figure that doesn't say where it came from can't be
// audited.
const (
	RateSourceSpot           = "spot"             // spawn:spot-price-per-hour: the price paid
	RateSourceOnDemandLive   = "on-demand:live"   // spawn:price-per-hour, from the live AWS Price List
	RateSourceOnDemandStatic = "on-demand:static" // spawn:price-per-hour, from truffle's static table: labelled as static
	RateSourceOnDemand       = "on-demand"        // spawn:price-per-hour with no recorded source (launched before spawn:price-source)
	RateSourceUnknown        = ""                 // no usable rate: no cost, no class
)

// RateFromTags picks the rate that applied to an instance, from its tags: the
// spot price paid when there is one, else the on-demand rate with its recorded
// source. lowerBound is set for Windows, whose rate omits the licence (#884).
func RateFromTags(tags map[string]string) (rate float64, source string, lowerBound bool) {
	lowerBound = strings.EqualFold(tags["spawn:os"], "windows")
	if v, err := strconv.ParseFloat(tags["spawn:spot-price-per-hour"], 64); err == nil && v > 0 {
		return v, RateSourceSpot, lowerBound
	}
	v, err := strconv.ParseFloat(tags["spawn:price-per-hour"], 64)
	if err != nil || v <= 0 {
		return 0, RateSourceUnknown, lowerBound
	}
	switch tags["spawn:price-source"] {
	case "live":
		return v, RateSourceOnDemandLive, lowerBound
	case "static":
		return v, RateSourceOnDemandStatic, lowerBound
	default:
		return v, RateSourceOnDemand, lowerBound
	}
}

// Cost is what one run cost, from its own timestamps and the rate that applied.
type Cost struct {
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at"`
	ComputeSeconds float64   `json:"compute_seconds"`
	RatePerHour    float64   `json:"rate_per_hour,omitempty"`
	RateSource     string    `json:"rate_source,omitempty"`
	// LowerBound is set for Windows: truffle prices Linux only, so the rate
	// omits the Windows licence (#884, truffle#185).
	LowerBound bool    `json:"lower_bound,omitempty"`
	USD        float64 `json:"usd,omitempty"`
}

// NewCost builds a run's cost. A non-positive rate or an unknown source gives a
// Cost with no USD and no class. An end before the start (clock skew) counts as
// zero seconds rather than negative.
func NewCost(started, ended time.Time, rate float64, source string, lowerBound bool) Cost {
	secs := ended.Sub(started).Seconds()
	if secs < 0 {
		secs = 0
	}
	c := Cost{StartedAt: started.UTC(), EndedAt: ended.UTC(), ComputeSeconds: secs, LowerBound: lowerBound}
	if rate > 0 && source != RateSourceUnknown {
		c.RatePerHour, c.RateSource = rate, source
		c.USD = rate * secs / 3600
	}
	return c
}

// RateKnown reports whether the cost has a usable rate.
func (c Cost) RateKnown() bool { return c.RatePerHour > 0 && c.RateSource != RateSourceUnknown }

// Hours is the run's wall-clock duration in hours.
func (c Cost) Hours() float64 { return c.ComputeSeconds / 3600 }

// Input is one staged-in object, as it was when the run read it. The ETag and
// version are what a later deletion sweeper compares against to prove the input
// is unchanged (design: delete only after re-verifying at deletion time).
type Input struct {
	URI       string `json:"uri"`
	ETag      string `json:"etag,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
}

// Recipe is what produced the output. Env records NAMES only, never values,
// which can hold secrets.
type Recipe struct {
	Command       []string `json:"command"`
	EnvNames      []string `json:"env_names,omitempty"`
	Image         string   `json:"image,omitempty"`
	ImageDigest   string   `json:"image_digest,omitempty"` // what actually ran, read after the pull
	AMI           string   `json:"ami,omitempty"`
	InstanceType  string   `json:"instance_type,omitempty"`
	SpawnVersion  string   `json:"spawn_version,omitempty"`
	SporedVersion string   `json:"spored_version,omitempty"`
}

// Record is the provenance annotation (AnnotationName) on every output a run
// wrote. One per run: regenerating any one output means rerunning the whole run,
// so each carries the whole run's cost.
type Record struct {
	Schema      int     `json:"schema"`
	TaskID      string  `json:"task_id"`
	RunID       string  `json:"run_id"`
	Regenerable bool    `json:"regenerable"` // the spec's opt-in to deletion (design, Decisions 2)
	Recipe      Recipe  `json:"recipe"`
	Inputs      []Input `json:"inputs,omitempty"`
	Cost        Cost    `json:"cost"`
	Class       Class   `json:"class,omitempty"`
}

// SchemaVersion is Record.Schema. Bump it on an incompatible change.
const SchemaVersion = 1

// AnnotationName is the S3 object annotation the Record is written to. Names
// may not start with "aws" or "s3".
const AnnotationName = "spawn.provenance"

// Tag keys. S3 allows 10 tags per object; these use at most five.
const (
	TagClass       = "spawn:regen-class"
	TagUSD         = "spawn:regen-usd"
	TagHours       = "spawn:regen-hours"
	TagRunID       = "spawn:run-id"
	TagRegenerable = "spawn:regenerable"
)

// Tags returns the object tags for a run's outputs. With no known rate there is
// no class, no USD and no hours tag, so no lifecycle rule can match.
// spawn:regenerable appears only when the spec opted in.
func (r Record) Tags() map[string]string {
	t := map[string]string{TagRunID: r.RunID}
	if cls, ok := Classify(r.Cost); ok {
		t[TagClass] = string(cls)
		t[TagUSD] = strconv.FormatFloat(r.Cost.USD, 'f', 4, 64)
		t[TagHours] = strconv.FormatFloat(r.Cost.Hours(), 'f', 4, 64)
	}
	if r.Regenerable {
		t[TagRegenerable] = "true"
	}
	return t
}

// EncodeTags renders tags as the x-amz-tagging header value: URL query form.
// Keys are sorted so the header is deterministic.
func EncodeTags(t map[string]string) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = url.QueryEscape(k) + "=" + url.QueryEscape(t[k])
	}
	return strings.Join(parts, "&")
}

// ObjectKey maps one local file of an output to its S3 key, exactly as
// `aws s3 cp` does. Pinned against the real CLI on a throwaway bucket
// (2026-10-17):
//
//	cp --recursive out/ s3://b/dest     -> dest/<rel>  (the slash is inserted)
//	cp --recursive out/ s3://b/dest/    -> dest/<rel>
//	cp --recursive out/ s3://b/         -> <rel>
//	cp file.txt s3://b/f1/named.txt     -> f1/named.txt
//	cp file.txt s3://b/f2/prefix/       -> f2/prefix/file.txt
//
// For a single file, rel is the file's base name. recursive is
// `aws s3 cp --recursive`, i.e. the manifest source ends in "/".
func ObjectKey(destKey, rel string, recursive bool) string {
	if recursive {
		if destKey == "" {
			return rel
		}
		return strings.TrimSuffix(destKey, "/") + "/" + rel
	}
	if destKey == "" || strings.HasSuffix(destKey, "/") {
		return destKey + rel
	}
	return destKey
}

// SplitS3URI splits s3://bucket/key into its parts.
func SplitS3URI(uri string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok || rest == "" {
		return "", "", fmt.Errorf("not an s3:// URI: %q", uri)
	}
	bucket, key, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("no bucket in %q", uri)
	}
	return bucket, key, nil
}
