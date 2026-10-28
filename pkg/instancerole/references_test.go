package instancerole

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// refSource is a ReferenceSource with a fixed answer or a fixed failure.
type refSource struct {
	desc  string
	refs  map[string][]string
	err   error
	calls int
}

func (s *refSource) Describe() string { return s.desc }

func (s *refSource) ProfileReferences(context.Context) (map[string][]string, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.refs, nil
}

// noRefs is a completed collection that found nothing.
func noRefs(t *testing.T) References {
	t.Helper()
	r, err := CollectReferences(context.Background(), &refSource{desc: "nothing"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A role a deferred launcher names is kept, whatever else says it could go,
// and the reason names the launcher.
func TestDecideKeepsAReferencedRole(t *testing.T) {
	f := deletable()
	f.ReferencedBy = []string{"schedule sched-a", "schedule sched-b"}
	ok, why := Decide(f, now)
	if ok {
		t.Fatal("deleted a role an enabled schedule still names")
	}
	if want := "referenced by schedule sched-a, schedule sched-b"; why != want {
		t.Errorf("kept for %q, want %q", why, want)
	}
}

// Collection fails closed: no sources, a nil source, or any source failing is
// an error, never an empty answer.
func TestCollectReferencesFailsClosed(t *testing.T) {
	ctx := context.Background()
	if _, err := CollectReferences(ctx); err == nil {
		t.Error("no sources collected as an empty set; a discovery that found nothing has broken")
	}
	if _, err := CollectReferences(ctx, nil); err == nil {
		t.Error("a nil source collected as an empty set")
	}
	ok := &refSource{desc: "ok", refs: map[string][]string{"spawn-instance-a": {"schedule x"}}}
	bad := &refSource{desc: "schedules (test)", err: errors.New("AccessDeniedException: no")}
	_, err := CollectReferences(ctx, ok, bad)
	var re *ReferencesError
	if !errors.As(err, &re) || re.Source != "schedules (test)" || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("a failing source gave %v, want a ReferencesError naming it and its cause", err)
	}
}

// Names are reduced to what IAM calls the profile, and merged across sources.
func TestCollectReferencesNormalizesAndMerges(t *testing.T) {
	a := &refSource{desc: "a", refs: map[string][]string{
		"arn:aws:iam::111122223333:instance-profile/spawn-instance-x": {"schedule 2"},
		" spawn-instance-x ": {"schedule 1"},
		"":                   {"schedule 3"},
	}}
	b := &refSource{desc: "b", refs: map[string][]string{"spawn-instance-x": {"schedule 1"}}}
	r, err := CollectReferences(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.For("spawn-instance-x"), []string{"schedule 1", "schedule 2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("For(spawn-instance-x) = %q, want %q", got, want)
	}
	if got := r.Sources(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Sources() = %q", got)
	}
}

// Sweep refuses References CollectReferences did not produce, before it reads
// a single role: forgetting to collect must delete nothing, not everything.
func TestSweepRefusesUncollectedReferences(t *testing.T) {
	f := &fakeIAM{roles: map[string]*fakeRole{"spawn-instance-gone": oldRole()}}
	_, err := Sweep(context.Background(), f, map[string]bool{}, References{}, now, true)
	if !errors.Is(err, ErrReferencesNotCollected) {
		t.Errorf("Sweep with uncollected references returned %v, want ErrReferencesNotCollected", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("Sweep called IAM without references: %q", f.calls)
	}
}

// Through Sweep: a referenced role is kept with the reason in the decision,
// in report and reap alike, and the unreferenced one beside it still goes.
func TestSweepKeepsReferencedRoles(t *testing.T) {
	refs, err := CollectReferences(context.Background(), &refSource{desc: "schedules",
		refs: map[string][]string{"spawn-instance-named": {"schedule sched-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, reap := range []bool{false, true} {
		f := &fakeIAM{roles: map[string]*fakeRole{"spawn-instance-named": oldRole(), "spawn-instance-gone": oldRole()}}
		res, err := Sweep(context.Background(), f, map[string]bool{}, refs, now, reap)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Decisions {
			switch d.Name {
			case "spawn-instance-named":
				if d.Delete || d.Reason != "referenced by schedule sched-1" {
					t.Errorf("reap=%t: %s delete=%t reason %q, want kept as referenced by schedule sched-1", reap, d.Name, d.Delete, d.Reason)
				}
			case "spawn-instance-gone":
				if !d.Delete {
					t.Errorf("reap=%t: the unreferenced role was kept: %s", reap, d.Reason)
				}
			}
		}
		for _, m := range f.mutations() {
			if strings.Contains(m, "spawn-instance-named") {
				t.Errorf("reap=%t changed the referenced role: %s", reap, m)
			}
		}
	}
}
