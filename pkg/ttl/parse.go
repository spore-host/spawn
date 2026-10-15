package ttl

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ParseDuration is the one parser for every TTL and lifecycle duration spawn
// accepts (#990): --ttl, --idle-timeout, `spawn extend`, `spored config set`,
// job-array, sweep, task, service, app and pipeline lifetimes, and every reader
// of the resulting tags — spored (pkg/provider) and the TTL reaper included.
//
// It accepts Go's time.ParseDuration syntax plus a day unit, because users type
// days: "1d", "2d12h", "1.5d", "1d30m". The grammar is one or more
// <decimal><unit> segments, where unit is one of ns, us, µs, ms, s, m, h, d
// (d = 24h), plus the bare "0". Signs, spaces, empty strings and overflow are
// rejected.
//
// Why one parser matters: `spawn launch --ttl 1d` used to tag spawn:ttl=1d,
// but the deadline was only written when time.ParseDuration (no day unit)
// accepted the value, and spored parsed the tag the same way. The launch
// "succeeded" with no deadline anywhere, so only the reaper's 7-day max-age
// ceiling bounded an instance the user asked to keep for one day. Two parsers
// that disagree on a single spelling are a silent cost leak; one parser cannot
// disagree with itself.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if s == "0" {
		return 0, nil
	}
	if s[0] == '-' || s[0] == '+' {
		return 0, fmt.Errorf("invalid duration %q: a duration cannot be signed", s)
	}

	var total time.Duration
	rest := s
	for rest != "" {
		// <decimal>
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration %q: expected a number at %q", s, rest)
		}
		num := rest[:i]
		rest = rest[i:]
		// <unit>: the longest unit that prefixes rest.
		unit := ""
		for _, u := range unitsLongestFirst {
			if strings.HasPrefix(rest, u) {
				unit = u
				break
			}
		}
		if unit == "" {
			return 0, fmt.Errorf("invalid duration %q: missing or unknown unit after %q (use one of %s)",
				s, num, strings.Join(Units(), ", "))
		}
		rest = rest[len(unit):]

		var seg time.Duration
		if unit == "d" {
			// Parse as hours, then scale: time.ParseDuration does the exact
			// decimal arithmetic (including fractions) and the overflow check
			// for the hours, and the scale is checked below.
			h, err := time.ParseDuration(num + "h")
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q: %w", s, err)
			}
			if h > math.MaxInt64/24 {
				return 0, fmt.Errorf("invalid duration %q: too large", s)
			}
			seg = h * 24
		} else {
			d, err := time.ParseDuration(num + unit)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q: %w", s, err)
			}
			seg = d
		}
		if seg > math.MaxInt64-total {
			return 0, fmt.Errorf("invalid duration %q: too large", s)
		}
		total += seg
	}
	return total, nil
}

// ParseTTL parses a TTL: a ParseDuration duration that must be positive. A TTL
// of zero would put the deadline at the launch instant, so the instance would
// be terminated on spored's first tick; the way to ask for no TTL is to omit it
// (or --no-timeout), never "0".
func ParseTTL(s string) (time.Duration, error) {
	d, err := ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid TTL %q: must be greater than zero", s)
	}
	return d, nil
}

// TagValue is the spelling to store in a spawn:* duration tag for a value the
// user supplied. A value time.ParseDuration already understands is kept as
// typed. A value only ParseDuration understands ("1d", "2d12h") is rewritten
// in Go's canonical form ("24h0m0s"), so a reader that still uses
// time.ParseDuration — an older spored, the dashboard, another repo reading the
// tag — gets the same duration instead of silently ignoring it.
func TagValue(s string) (string, error) {
	d, err := ParseDuration(s)
	if err != nil {
		return "", err
	}
	if _, err := time.ParseDuration(s); err == nil {
		return s, nil
	}
	return d.String(), nil
}

// units lists every unit ParseDuration accepts: time.ParseDuration's, plus d.
var units = []string{"ns", "us", "µs" /* U+00B5 */, "μs" /* U+03BC */, "ms", "s", "m", "h", "d"}

// unitsLongestFirst is units, longest first, so "ms" matches before "m".
var unitsLongestFirst = func() []string {
	us := Units()
	sort.SliceStable(us, func(i, j int) bool { return len(us[i]) > len(us[j]) })
	return us
}()

// Units returns every unit ParseDuration accepts, sorted. Tests elsewhere
// enumerate it rather than keep their own list, so a unit added here is
// automatically covered by every "each accepted form yields a deadline" check.
func Units() []string {
	out := append([]string(nil), units...)
	sort.Strings(out)
	return out
}
