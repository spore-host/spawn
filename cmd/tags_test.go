package cmd

import "testing"

func TestParseKVTags(t *testing.T) {
	t.Run("parses key=value pairs", func(t *testing.T) {
		got, err := parseKVTags([]string{"project=demo", "db-version=k2_20260226"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["project"] != "demo" || got["db-version"] != "k2_20260226" {
			t.Errorf("parsed = %v", got)
		}
	})

	t.Run("value may contain =", func(t *testing.T) {
		got, err := parseKVTags([]string{"expr=a=b=c"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["expr"] != "a=b=c" {
			t.Errorf("value = %q, want a=b=c", got["expr"])
		}
	})

	t.Run("nil/empty yields empty map", func(t *testing.T) {
		got, err := parseKVTags(nil)
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, err %v", got, err)
		}
	})

	t.Run("rejects bad forms", func(t *testing.T) {
		for _, bad := range []string{"noequals", "=novalue", "  =x", "spawn:managed=false", "SPAWN:x=y"} {
			if _, err := parseKVTags([]string{bad}); err == nil {
				t.Errorf("expected error for %q", bad)
			}
		}
	})
}
