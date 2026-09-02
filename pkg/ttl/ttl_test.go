package ttl

import (
	"errors"
	"testing"
	"time"
)

func TestDeadlineForNewTTL_Shorten(t *testing.T) {
	launch := time.Date(2026, 8, 20, 8, 26, 22, 0, time.UTC)
	// Instance has an 8h deadline (16:26:22Z). Operator shortens to 8m total
	// from launch — the exact spawn#553 repro.
	deadline, err := DeadlineForNewTTL(launch, 8*time.Minute)
	if err != nil {
		t.Fatalf("DeadlineForNewTTL: %v", err)
	}
	want := launch.Add(8 * time.Minute)
	if !deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v (8m after launch, not 8h)", deadline, want)
	}

	// The new deadline must be EARLIER than the original 8h deadline — this is
	// the exact assertion that was missing before the fix: a shorter TTL must
	// actually produce an earlier deadline, not an unchanged one.
	originalDeadline := launch.Add(8 * time.Hour)
	if !deadline.Before(originalDeadline) {
		t.Errorf("shortened deadline %v is not before the original deadline %v — TTL was not actually shortened", deadline, originalDeadline)
	}
}

func TestDeadlineForNewTTL_Lengthen(t *testing.T) {
	launch := time.Date(2026, 8, 20, 8, 26, 22, 0, time.UTC)
	deadline, err := DeadlineForNewTTL(launch, 8*time.Hour)
	if err != nil {
		t.Fatalf("DeadlineForNewTTL: %v", err)
	}
	want := launch.Add(8 * time.Hour)
	if !deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", deadline, want)
	}

	// A longer TTL than the original (say 1h) must move the deadline later —
	// same code path as extend's forward case, just expressed as a new total
	// rather than an increment.
	originalDeadline := launch.Add(1 * time.Hour)
	if !deadline.After(originalDeadline) {
		t.Errorf("lengthened deadline %v is not after the original %v", deadline, originalDeadline)
	}
}

func TestDeadlineForNewTTL_LaunchTimeUnknownRefuses(t *testing.T) {
	_, err := DeadlineForNewTTL(time.Time{}, 8*time.Minute)
	if err == nil {
		t.Fatal("expected an error when launch time is unknown, got nil")
	}
	if !errors.Is(err, ErrLaunchTimeUnknown) {
		t.Errorf("expected ErrLaunchTimeUnknown, got %v", err)
	}
}

func TestSetTags_ShortenProducesEarlierDeadlineAndMatchingTags(t *testing.T) {
	launch := time.Date(2026, 8, 20, 8, 26, 22, 0, time.UTC)

	tags, deadline, err := SetTags(launch, 8*time.Minute)
	if err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	wantDeadline := launch.Add(8 * time.Minute)
	if !deadline.Equal(wantDeadline) {
		t.Errorf("deadline = %v, want %v", deadline, wantDeadline)
	}

	wantDeadlineTag := wantDeadline.UTC().Format(time.RFC3339)
	if got := tags["spawn:ttl-deadline"]; got != wantDeadlineTag {
		t.Errorf("spawn:ttl-deadline = %q, want %q", got, wantDeadlineTag)
	}
	wantTTLTag := (8 * time.Minute).String()
	if got := tags["spawn:ttl"]; got != wantTTLTag {
		t.Errorf("spawn:ttl = %q, want %q", got, wantTTLTag)
	}

	// The two tags must agree with each other: parsing spawn:ttl-deadline and
	// subtracting launch must reproduce spawn:ttl. This is the exact
	// user-visible bug from #553 — `config get ttl` and `spored status`
	// disagreeing about the same instance at the same moment because the two
	// tags were never written together.
	parsedDeadline, err := time.Parse(time.RFC3339, tags["spawn:ttl-deadline"])
	if err != nil {
		t.Fatalf("parse spawn:ttl-deadline: %v", err)
	}
	parsedTTL, err := time.ParseDuration(tags["spawn:ttl"])
	if err != nil {
		t.Fatalf("parse spawn:ttl: %v", err)
	}
	if !launch.Add(parsedTTL).Equal(parsedDeadline) {
		t.Errorf("tags disagree: launch+spawn:ttl = %v, spawn:ttl-deadline = %v", launch.Add(parsedTTL), parsedDeadline)
	}
}

func TestSetTags_LaunchTimeUnknownRefuses(t *testing.T) {
	_, _, err := SetTags(time.Time{}, 8*time.Minute)
	if err == nil {
		t.Fatal("expected an error when launch time is unknown, got nil")
	}
	if !errors.Is(err, ErrLaunchTimeUnknown) {
		t.Errorf("expected ErrLaunchTimeUnknown, got %v", err)
	}
}
