package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/provider"
)

// Defaults for `spored egress-proxy` (#1515).
const (
	defaultEgressProxyPort = egressproxy.Port
	// defaultEgressProxyIdle is the owner's default idle period (2026-11-03):
	// three days with nothing proxied, then the proxy instance terminates. It
	// was 14 days until the proxy's life was capped under the TTL reaper's
	// ceiling, which a 14-day idle period could never reach.
	defaultEgressProxyIdle = egressproxy.DefaultIdle
	// egressProxyPersistEvery is how often the last-activity time is written
	// to disk while it changes.
	egressProxyPersistEvery = time.Minute
	// egressProxyListenRetry is the wait between attempts to bind.
	egressProxyListenRetry = 30 * time.Second
)

// newEgressProxyCmd is `spored egress-proxy`: the lifecycle daemon, exactly as
// the bare `spored` runs it (TTL, cost limit, completion, Spot, DNS), plus the
// egress forward proxy, whose activity replaces spored's own idle signals.
//
// It is a subcommand rather than a tag-driven mode of the bare daemon so that
// what an instance is for is fixed by its systemd unit, which only its user
// data writes, and not by a tag anything with ec2:CreateTags on it can change;
// and so that no other spored ever opens a listener.
func newEgressProxyCmd() *cobra.Command {
	m := &egressProxyMode{}
	cmd := &cobra.Command{
		Use:   "egress-proxy",
		Short: "Run the lifecycle daemon as a spawn-managed egress proxy",
		Long: "Runs the spored lifecycle daemon together with a forward proxy that carries HTTPS CONNECT (port 443) " +
			"and plain-HTTP GET/HEAD (port 80) to the hosts spawn's bootstrap fetches from, and nothing else. " +
			"When nothing has been proxied for --idle, the instance terminates.\n\n" +
			"By default it listens on the instance's private IP only. --listen 0.0.0.0:PORT binds every interface; " +
			"do that only behind a security group that admits nothing but spawn's instance security groups on PORT.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := m.validate(); err != nil {
				return err
			}
			runDaemonWith(m)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&m.listen, "listen", "", "host:port to listen on (default: the instance's private IP, port --port)")
	f.IntVar(&m.port, "port", defaultEgressProxyPort, "port to listen on when --listen is not set")
	f.DurationVar(&m.idle, "idle", defaultEgressProxyIdle, "terminate the instance after this long with nothing proxied")
	f.StringVar(&m.statePath, "state-file", egressProxyStatePath(), "where the last-activity time is persisted across restarts")
	f.IntVar(&m.maxConns, "max-conns", egressproxy.DefaultMaxConns, "maximum concurrent proxied connections")
	return cmd
}

// egressProxyMode is the daemonMode `spored egress-proxy` attaches.
type egressProxyMode struct {
	listen    string
	port      int
	idle      time.Duration
	statePath string
	maxConns  int

	// Seams. nil means the real thing.
	now        func() time.Time
	listenFunc func(network, addr string) (net.Listener, error)
	retryWait  time.Duration
	persist    time.Duration

	proxy *egressproxy.Proxy
	// publisher throttles the activity tags (#1515 phase 5). Touched only by
	// persistLoop, and by start before it runs.
	publisher egressproxy.ActivityPublisher
}

func (m *egressProxyMode) validate() error {
	if m.idle <= 0 {
		return errors.New("--idle must be positive")
	}
	if m.listen == "" && (m.port <= 0 || m.port > 65535) {
		return fmt.Errorf("--port %d is not a TCP port", m.port)
	}
	if m.listen != "" {
		if _, _, err := net.SplitHostPort(m.listen); err != nil {
			return fmt.Errorf("--listen %q: %w", m.listen, err)
		}
	}
	if m.statePath == "" {
		return errors.New("--state-file must be set")
	}
	return nil
}

func (m *egressProxyMode) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// listenAddr is where the proxy binds: --listen, or the private IP and --port.
func (m *egressProxyMode) listenAddr(identity *provider.Identity) (string, error) {
	if m.listen != "" {
		return m.listen, nil
	}
	if identity == nil || identity.PrivateIP == "" {
		return "", errors.New("no private IP to listen on; pass --listen")
	}
	return net.JoinHostPort(identity.PrivateIP, strconv.Itoa(m.port)), nil
}

// idleSetter is the part of *agent.Agent the mode needs.
type idleSetter interface {
	SetExternalIdle(agent.ExternalIdle)
	GetConfig() *provider.Config
	// PutInstanceTags is spored's own tag path (ec2:CreateTags on itself),
	// for the activity tags.
	PutInstanceTags(ctx context.Context, tags map[string]string) error
}

// attach implements daemonMode.
func (m *egressProxyMode) attach(ctx context.Context, ag *agent.Agent, identity *provider.Identity) func() {
	return m.start(ctx, ag, identity)
}

// start builds the proxy, hands its activity to the agent as the idle signal,
// and serves it in the background. The idle signal is set first, whether or
// not the listener binds, so a proxy that cannot serve still expires.
func (m *egressProxyMode) start(ctx context.Context, ag idleSetter, identity *provider.Identity) func() {
	now := m.clock()
	// An idle period longer than the instance's own life is refused: the
	// deadline would end the proxy first, so the setting would promise what
	// nothing keeps (#1515). spawn never launches one; this is the backstop
	// for a unit written by hand. Refusing means not serving at all and
	// retiring the instance at the next tick, through the same terminate the
	// idle timeout uses, rather than serving under a period it cannot honour.
	if err := idleRefusal(ag.GetConfig(), m.idle); err != nil {
		log.Printf("egress-proxy: refusing to serve: %v; terminating the instance", err)
		ag.SetExternalIdle(agent.ExternalIdle{
			Name:         "egress proxy (refused: " + err.Error() + ")",
			LastActivity: func() time.Time { return time.Time{} },
			Timeout:      time.Nanosecond,
		})
		return func() {}
	}
	persisted, err := egressproxy.LoadActivity(m.statePath)
	if err != nil {
		log.Printf("egress-proxy: %v; counting idle from the launch time", err)
	}
	var anchor time.Time
	if cfg := ag.GetConfig(); cfg != nil {
		anchor = cfg.LaunchTime
	}
	initial := egressproxy.InitialActivity(persisted, anchor, now)
	log.Printf("egress-proxy: idle period %v, last activity %s (persisted %s, launched %s)",
		m.idle, initial.UTC().Format(time.RFC3339), fmtTime(persisted), fmtTime(anchor))

	m.proxy = egressproxy.New(egressproxy.Config{
		MaxConns:        m.maxConns,
		InitialActivity: initial,
		Logger:          log.Default(),
		Now:             m.now,
	})
	ag.SetExternalIdle(agent.ExternalIdle{Name: "egress proxy", LastActivity: m.proxy.LastActivity, Timeout: m.idle})

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.serve(ctx, identity) }()
	go func() { defer wg.Done(); m.persistLoop(ctx, ag) }()
	return func() {
		cancel()
		wg.Wait()
		m.save()
	}
}

// idleRefusal is why idle cannot be this instance's idle period: it is longer
// than the instance's life, from its launch to its spawn:ttl-deadline. nil when
// the instance has no deadline or launch time to measure against.
func idleRefusal(cfg *provider.Config, idle time.Duration) error {
	if cfg == nil || cfg.TTLDeadline.IsZero() || cfg.LaunchTime.IsZero() {
		return nil
	}
	life := cfg.TTLDeadline.Sub(cfg.LaunchTime)
	if idle > life {
		return fmt.Errorf("--idle %v is longer than this instance's life of %v (launched %s, deadline %s)",
			idle, life, fmtTime(cfg.LaunchTime), fmtTime(cfg.TTLDeadline))
	}
	return nil
}

// serve binds, retrying until it can or ctx ends, then serves until ctx ends.
func (m *egressProxyMode) serve(ctx context.Context, identity *provider.Identity) {
	listen := m.listenFunc
	if listen == nil {
		listen = net.Listen
	}
	wait := m.retryWait
	if wait <= 0 {
		wait = egressProxyListenRetry
	}
	for {
		addr, err := m.listenAddr(identity)
		var ln net.Listener
		if err == nil {
			ln, err = listen("tcp", addr)
		}
		if err == nil {
			log.Printf("egress-proxy: listening on %s", ln.Addr())
			if err := m.proxy.Serve(ctx, ln); err != nil {
				log.Printf("egress-proxy: serve: %v", err)
			}
			if ctx.Err() != nil {
				return
			}
		} else {
			log.Printf("egress-proxy: cannot listen: %v; retrying in %v", err, wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// persistLoop writes the last-activity time to disk whenever it has moved,
// and publishes it as the activity tags at the publisher's pace: on the first
// tick, at once for the first activity after a quiet period, and otherwise at
// most once per [egressproxy.ActivityPublishEvery] while it moves (see
// [egressproxy.ActivityPublisher]).
func (m *egressProxyMode) persistLoop(ctx context.Context, tags idleSetter) {
	every := m.persist
	if every <= 0 {
		every = egressProxyPersistEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var saved time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if last := m.proxy.LastActivity(); !last.Equal(saved) {
				if m.save() {
					saved = last
				}
			}
			m.publishActivity(ctx, tags)
		}
	}
}

// publishActivity writes [egressproxy.ActivityTag] and [egressproxy.IdleTag]
// when the publisher says they are due (#1515 phase 5), so `spawn egress-proxy
// status` can say how long the proxy has been idle and when its idle period
// ends without SSM. Best effort: a refused write is logged and retried at the
// publisher's pace; the idle expiry never depends on it.
func (m *egressProxyMode) publishActivity(ctx context.Context, tags idleSetter) {
	if m.proxy == nil || tags == nil {
		return
	}
	v, due := m.publisher.Due(m.proxy.LastActivity(), m.clock())
	if !due {
		return
	}
	if err := tags.PutInstanceTags(ctx, map[string]string{
		egressproxy.ActivityTag: v,
		egressproxy.IdleTag:     m.idle.String(),
	}); err != nil {
		log.Printf("egress-proxy: publishing %s: %v", egressproxy.ActivityTag, err)
		return
	}
	m.publisher.Written(v)
}

// save persists the current last-activity time, reporting success.
func (m *egressProxyMode) save() bool {
	if m.proxy == nil {
		return false
	}
	if err := egressproxy.SaveActivity(m.statePath, m.proxy.LastActivity(), m.clock()); err != nil {
		log.Printf("egress-proxy: persisting last activity: %v", err)
		return false
	}
	return true
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}
