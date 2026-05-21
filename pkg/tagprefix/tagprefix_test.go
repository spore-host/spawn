package tagprefix

import (
	"os"
	"testing"
)

func TestDefaultPrefix(t *testing.T) {
	// Reset to default
	prefix = "spawn"

	if got := Prefix(); got != "spawn" {
		t.Errorf("Prefix() = %q, want %q", got, "spawn")
	}
	if got := Tag("ttl"); got != "spawn:ttl" {
		t.Errorf("Tag(\"ttl\") = %q, want %q", got, "spawn:ttl")
	}
	if got := FilterTag("job-array-id"); got != "tag:spawn:job-array-id" {
		t.Errorf("FilterTag(\"job-array-id\") = %q, want %q", got, "tag:spawn:job-array-id")
	}
}

func TestCustomPrefix(t *testing.T) {
	orig := os.Getenv("SPORED_TAG_PREFIX")
	defer func() {
		os.Setenv("SPORED_TAG_PREFIX", orig)
		prefix = "spawn" // restore default
	}()

	os.Setenv("SPORED_TAG_PREFIX", "prism")
	Init()

	if got := Prefix(); got != "prism" {
		t.Errorf("Prefix() = %q, want %q", got, "prism")
	}
	if got := Tag("ttl"); got != "prism:ttl" {
		t.Errorf("Tag(\"ttl\") = %q, want %q", got, "prism:ttl")
	}
	if got := FilterTag("managed"); got != "tag:prism:managed" {
		t.Errorf("FilterTag(\"managed\") = %q, want %q", got, "tag:prism:managed")
	}
}

func TestEmptyEnvKeepsDefault(t *testing.T) {
	orig := os.Getenv("SPORED_TAG_PREFIX")
	defer func() {
		os.Setenv("SPORED_TAG_PREFIX", orig)
		prefix = "spawn"
	}()

	os.Setenv("SPORED_TAG_PREFIX", "")
	prefix = "spawn"
	Init()

	if got := Prefix(); got != "spawn" {
		t.Errorf("Prefix() = %q after empty env, want %q", got, "spawn")
	}
}
