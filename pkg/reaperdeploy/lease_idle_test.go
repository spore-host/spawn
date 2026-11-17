package reaperdeploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"
)

// leaseWorld is an account's lifecycle table, with a clock.
type leaseWorld struct {
	now time.Time
	c   *lifecycle.Client
}

func newLeaseWorld(t *testing.T) *leaseWorld {
	t.Helper()
	w := &leaseWorld{now: time.Now().Truncate(time.Second)}
	f := lifecycletest.New()
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(string) (lifecycle.API, error) { return f, nil },
		ControlRegion: "us-east-1", Now: func() time.Time { return w.now },
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.c = c
	return w
}

// mark is a spawn launch marking scope (pkg/lifecyclemark), observe the
// reaper's account-idle sweep recording what it saw.
func (w *leaseWorld) mark(t *testing.T, scope string) {
	t.Helper()
	if err := w.c.MarkActive(context.Background(), tags.ToolSpawn, scope); err != nil {
		t.Fatal(err)
	}
}

func (w *leaseWorld) observe(t *testing.T, scope string, active bool) {
	t.Helper()
	if _, _, err := w.c.Observe(context.Background(), tags.ToolSpawn, scope, active); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseIdlenessReadsTheVerdict(t *testing.T) {
	regions := []string{"us-east-1"}
	t.Run("no verdict is undetermined", func(t *testing.T) {
		w := newLeaseWorld(t)
		if v := LeaseIdleness(context.Background(), w.c, regions, w.now); v.Determined {
			t.Fatalf("%+v: want undetermined with no item", v)
		}
	})
	t.Run("an item no creator marked is undetermined", func(t *testing.T) {
		w := newLeaseWorld(t)
		w.observe(t, "us-east-1", false)
		w.observe(t, lifecycle.ScopeGlobal, false)
		w.now = w.now.Add(72 * time.Hour)
		if v := LeaseIdleness(context.Background(), w.c, regions, w.now); v.Determined {
			t.Fatalf("%+v: want undetermined for epoch-0 items (lease unavailable)", v)
		}
	})
	t.Run("idle in every scope is the shortest idleness", func(t *testing.T) {
		w := newLeaseWorld(t)
		w.mark(t, "us-east-1")
		w.mark(t, lifecycle.ScopeGlobal)
		w.observe(t, "us-east-1", false)
		w.now = w.now.Add(10 * time.Hour)
		w.observe(t, lifecycle.ScopeGlobal, false)
		w.now = w.now.Add(5 * time.Hour)
		v := LeaseIdleness(context.Background(), w.c, regions, w.now)
		if !v.Determined || v.Active != "" || v.IdleFor != 5*time.Hour {
			t.Fatalf("%+v: want determined, idle for 5h (the global scope's)", v)
		}
	})
	t.Run("any active scope is active", func(t *testing.T) {
		w := newLeaseWorld(t)
		w.mark(t, "us-east-1")
		w.mark(t, lifecycle.ScopeGlobal)
		w.observe(t, lifecycle.ScopeGlobal, false)
		w.observe(t, "us-east-1", true)
		v := LeaseIdleness(context.Background(), w.c, regions, w.now.Add(48*time.Hour))
		if !v.Determined || !strings.Contains(v.Active, "us-east-1") {
			t.Fatalf("%+v: want active in us-east-1", v)
		}
	})
	t.Run("an unreadable table is undetermined", func(t *testing.T) {
		c, err := lifecycle.New(lifecycle.Options{DynamoDB: func(string) (lifecycle.API, error) { return nil, errors.New("AccessDenied") }})
		if err != nil {
			t.Fatal(err)
		}
		if v := LeaseIdleness(context.Background(), c, regions, time.Now()); v.Determined {
			t.Fatalf("%+v: want undetermined", v)
		}
	})
}

// --if-idle-for (spawn#1654) judges idleness by the lifecycle verdict, and
// keeps the refusals it had: undetermined, and a reaper that does not run.
func TestIdleTeardownRefusal(t *testing.T) {
	ranLogs := func(now time.Time) *fakeLogs {
		return &fakeLogs{events: runs(now.Add(-40*time.Minute), now.Add(-10*time.Minute), 10*time.Minute)}
	}
	deployer := func(now time.Time, env map[string]string, logs *fakeLogs) *Deployer {
		d := idleDeployer(logs)
		if env == nil {
			env = map[string]string{"REAPER_REGIONS": "us-east-1"}
		}
		d.Lambda = &fakeLambda{exists: true, env: env}
		d.Events = &fakeEvents{}
		return d
	}
	idleFor := func(t *testing.T, d time.Duration) *leaseWorld {
		w := newLeaseWorld(t)
		for _, sc := range []string{"us-east-1", lifecycle.ScopeGlobal} {
			w.mark(t, sc)
			w.observe(t, sc, false)
		}
		w.now = w.now.Add(d)
		return w
	}
	ctx := context.Background()

	w := idleFor(t, 20*time.Hour)
	if why := deployer(w.now, nil, ranLogs(w.now)).IdleTeardownRefusal(ctx, w.c, nil, "us-east-1", 12*time.Hour, w.now); why != "" {
		t.Errorf("idle 20h, --if-idle-for 12h: refused (%s), want allowed", why)
	}
	for name, c := range map[string]struct {
		w    *leaseWorld
		env  map[string]string
		logs func(time.Time) *fakeLogs
		want time.Duration
		lerr error
		says string
	}{
		"idle too briefly":        {w: idleFor(t, 2*time.Hour), want: 12 * time.Hour, says: "less than"},
		"beyond the grace":        {w: idleFor(t, 20*time.Hour), want: 48 * time.Hour, says: "longer than the reaper's account-idle grace"},
		"the sweep is off":        {w: idleFor(t, 20*time.Hour), env: map[string]string{"REAPER_ACCOUNT_IDLE": "off"}, want: time.Hour, says: "is off"},
		"the reaper never ran":    {w: idleFor(t, 20*time.Hour), logs: func(time.Time) *fakeLogs { return &fakeLogs{} }, want: time.Hour, says: "broken schedule"},
		"the logs are unreadable": {w: idleFor(t, 20*time.Hour), logs: func(time.Time) *fakeLogs { return &fakeLogs{err: errors.New("denied")} }, want: time.Hour, says: "could not determine"},
		"no lifecycle table":      {w: idleFor(t, 20*time.Hour), lerr: errors.New("no client"), want: time.Hour, says: "could not determine"},
		"no verdict":              {w: newLeaseWorld(t), want: time.Hour, says: "no lifecycle verdict"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := ranLogs(c.w.now)
			if c.logs != nil {
				logs = c.logs(c.w.now)
			}
			why := deployer(c.w.now, c.env, logs).IdleTeardownRefusal(ctx, c.w.c, c.lerr, "us-east-1", c.want, c.w.now)
			if !strings.Contains(why, c.says) {
				t.Errorf("refusal %q, want one saying %q", why, c.says)
			}
		})
	}

	// A longer grace lets a longer wait through.
	w = idleFor(t, 40*24*time.Hour)
	env := map[string]string{"REAPER_REGIONS": "us-east-1", "REAPER_ACCOUNT_IDLE_GRACE": "30d"}
	if why := deployer(w.now, env, ranLogs(w.now)).IdleTeardownRefusal(ctx, w.c, nil, "us-east-1", 30*24*time.Hour, w.now); why != "" {
		t.Errorf("grace 30d, idle 40d, --if-idle-for 30d: refused (%s)", why)
	}

	// Activity in one scope keeps the reaper.
	w = idleFor(t, 20*time.Hour)
	w.observe(t, lifecycle.ScopeGlobal, true)
	if why := deployer(w.now, nil, ranLogs(w.now)).IdleTeardownRefusal(ctx, w.c, nil, "us-east-1", time.Hour, w.now); !strings.Contains(why, "active") {
		t.Errorf("active global scope: %q, want refused as active", why)
	}
}
