// Package tagprefix provides a configurable EC2 tag prefix for the spored daemon.
//
// By default the prefix is "spawn", producing tags like "spawn:ttl". Setting the
// SPORED_TAG_PREFIX environment variable changes the prefix (e.g. "prism" produces
// "prism:ttl"), allowing sister projects to reuse spored with their own tag namespace.
package tagprefix

import "os"

var prefix = "spawn"

// Init reads the SPORED_TAG_PREFIX environment variable and sets the tag prefix.
// Call once at process startup, before any tag lookups.
func Init() {
	if p := os.Getenv("SPORED_TAG_PREFIX"); p != "" {
		prefix = p
	}
}

// Prefix returns the current tag prefix (e.g. "spawn" or "prism").
func Prefix() string { return prefix }

// Tag returns "prefix:suffix", e.g. "spawn:ttl" or "prism:ttl".
func Tag(suffix string) string { return prefix + ":" + suffix }

// FilterTag returns "tag:prefix:suffix" for use in EC2 DescribeInstances filters.
func FilterTag(suffix string) string { return "tag:" + prefix + ":" + suffix }
