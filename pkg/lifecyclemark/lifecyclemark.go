// Package lifecyclemark is spawn's creator half of the shared-account lease
// (github.com/spore-host/libs/lifecycle, spawn#1654, part of the
// leave-no-trace epic spore-host/spore-host#752).
//
// Every spawn path that creates something an idle teardown could later delete
// (an instance, a security group, a key pair, a placement group, an Elastic
// IP, an IAM role, instance profile or managed policy, a bucket, a pool queue,
// an FSx filesystem) calls Mark first, in the account it creates in, with the
// region it creates in, or ScopeGlobal for IAM. Mark is lifecycle.MarkActive:
// it records spawn ACTIVE in that scope's item of the spore-host-lifecycle
// table, so a teardown that holds the lease loses it, and none can claim one
// until the 24 h grace has passed again. internal/iamcoverage holds every
// creating path to it (TestEveryCreatorMarksTheLease).
//
// # A launch never fails because of the lease
//
// Mark returns nothing. Whatever goes wrong (the caller's identity may not
// read or write the table, DynamoDB is throttling, a teardown lease is held
// past lifecycle's 10 minute wait), it prints one warning per process and the
// launch goes on. An identity that is refused is not asked again in the same
// process, so a pool of a hundred workers waits for the lease at most once.
//
// Nothing is marked in spore.host's own infra account
// (footprintspawn.InfraAccountID): no idle teardown ever runs there (the
// reaper's GuardTeardown refuses it), and a mark would only leave a lifecycle
// table beside the live service. `spawn pipeline launch` and the schedule
// paths create there.
//
// The account is then "lease unavailable" for that user: the idle teardown
// (the TTL reaper's account-idle sweep) only claims a scope whose item a
// creator has marked at least once (lifecycle.Item.Epoch > 0), so an account
// whose creators can never mark it is never torn down for idleness. In a
// shared account where some users can mark and some cannot, a launch by one
// who cannot is still seen by the teardown's own activity re-check, but not
// fenced by the lease; grant the actions in RequiredActions to every user.
package lifecyclemark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"

	"github.com/spore-host/spawn/pkg/footprintspawn"
)

// Tool is the tool spawn marks.
const Tool = tags.ToolSpawn

// ControlRegion is the region whose lifecycle table holds ScopeGlobal's item:
// spawn's account-wide resources (IAM). calque's lease defaults to the same
// region (its --lease-region), and the reaper reads it from here.
const ControlRegion = "us-east-1"

// ScopeGlobal is the scope of spawn's account-wide resources.
const ScopeGlobal = lifecycle.ScopeGlobal

// Owner is how a CLI's marks identify themselves in the table's tags.
const Owner = "spawn-cli"

// RequiredActions are what an identity needs on the lifecycle table
// (arn:aws:dynamodb:*:*:table/spore-host-lifecycle) for Mark to work.
// MarkActive is one transaction of a ConditionCheck and an Update, and creates
// the table, tagged, the first time. DynamoDB authorizes a transaction per
// item action, and internal/iamcoverage lists every item action a
// TransactWriteItems may hold, hence PutItem. DeleteItem is for one rare
// case: removing the closing sentinel a crashed CloseTableIfEmpty abandoned,
// which would otherwise block every mark in that region. A test
// (TestLeaseRequiredActionsAreWhatMarkCalls) holds this list to the actions
// internal/iamcoverage finds Mark calling.
var RequiredActions = []string{
	"dynamodb:ConditionCheckItem", "dynamodb:UpdateItem", "dynamodb:PutItem", "dynamodb:DeleteItem",
	"dynamodb:DescribeTable", "dynamodb:CreateTable", "dynamodb:TagResource",
}

// TableARNPattern is the resource RequiredActions are needed on.
const TableARNPattern = "arn:aws:dynamodb:*:*:table/" + lifecycle.TableName

// cacheFor is how long a successful mark stands in for the next one in the
// same process and scope. Well inside the 24 h grace, so a long-running
// process still marks again before any later create (the lease assumes a
// creator creates within the grace of its MarkActive).
const cacheFor = time.Hour

// Factory returns the DynamoDB client for one region of cfg's account.
type Factory func(cfg aws.Config, region string) (lifecycle.API, error)

// marker is the process's marking state.
type marker struct {
	mu      sync.Mutex
	factory Factory
	out     io.Writer
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	// refused: an identity was refused the table; no more marks this process.
	refused bool
	warned  bool
	waiting map[string]bool
	marked  map[string]time.Time
	// accountOf resolves cfg's account (sts:GetCallerIdentity, which needs no
	// permission); accounts caches it by access key.
	accountOf func(ctx context.Context, cfg aws.Config) (string, error)
	accounts  map[string]string
}

func newMarker() *marker {
	return &marker{
		factory:   defaultFactory,
		out:       os.Stderr,
		now:       time.Now,
		waiting:   map[string]bool{},
		marked:    map[string]time.Time{},
		accountOf: defaultAccountOf,
		accounts:  map[string]string{},
	}
}

func defaultAccountOf(ctx context.Context, cfg aws.Config) (string, error) {
	if testing.Testing() {
		return "", errInTest
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Account), nil
}

var std = newMarker()

// errInTest is the default factory's answer in a test binary: a test reaches
// AWS only through a client it built, never through a mark it did not ask
// for (#968). A test that wants marks installs a Factory with Override.
var errInTest = errors.New("lifecyclemark: no lease client in a test binary")

func defaultFactory(cfg aws.Config, region string) (lifecycle.API, error) {
	if testing.Testing() {
		return nil, errInTest
	}
	c := cfg.Copy()
	c.Region = region
	return dynamodb.NewFromConfig(c), nil
}

// Override replaces the client factory and the warning writer (nil keeps the
// current one), and clears the process's state, for a test. It returns the
// function that restores them.
func Override(f Factory, out io.Writer) (restore func()) {
	std.mu.Lock()
	defer std.mu.Unlock()
	saved := struct {
		factory Factory
		out     io.Writer
		now     func() time.Time
		sleep   func(ctx context.Context, d time.Duration) error
	}{std.factory, std.out, std.now, std.sleep}
	if f != nil {
		std.factory = f
	}
	if out != nil {
		std.out = out
	}
	std.refused, std.warned = false, false
	std.waiting, std.marked, std.accounts = map[string]bool{}, map[string]time.Time{}, map[string]string{}
	return func() {
		std.mu.Lock()
		defer std.mu.Unlock()
		std.factory, std.out, std.now, std.sleep = saved.factory, saved.out, saved.now, saved.sleep
		std.refused, std.warned = false, false
		std.waiting, std.marked, std.accounts = map[string]bool{}, map[string]time.Time{}, map[string]string{}
	}
}

// Mark records that spawn is about to create resources in each scope (a
// region, or ScopeGlobal) of cfg's account. Call it before creating, then
// create. It never fails: see the package documentation.
func Mark(ctx context.Context, cfg aws.Config, scopes ...string) {
	std.mark(ctx, cfg, scopes...)
}

func (m *marker) mark(ctx context.Context, cfg aws.Config, scopes ...string) {
	if len(scopes) == 0 || cfg.Credentials == nil {
		// No credentials, nothing to mark with (a zero Config in a seam).
		return
	}
	who := identity(ctx, cfg)
	m.mu.Lock()
	if m.refused {
		m.mu.Unlock()
		return
	}
	now := m.now()
	var todo []string
	for _, s := range scopes {
		if s == "" {
			continue
		}
		if t, ok := m.marked[who+"|"+s]; ok && who != "" && now.Sub(t) < cacheFor {
			continue
		}
		todo = append(todo, s)
	}
	factory := m.factory
	m.mu.Unlock()
	if len(todo) == 0 || m.isInfra(ctx, cfg, who) {
		return
	}

	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(region string) (lifecycle.API, error) { return factory(cfg, region) },
		ControlRegion: ControlRegion,
		Owner:         Owner,
		Now:           m.now,
		Sleep:         m.sleeper(todo),
	})
	if err != nil {
		m.fail(err)
		return
	}
	for _, s := range todo {
		if err := c.MarkActive(ctx, Tool, s); err != nil {
			m.fail(fmt.Errorf("scope %s: %w", s, err))
			if m.isRefused() {
				return
			}
			continue
		}
		if who != "" {
			m.mu.Lock()
			m.marked[who+"|"+s] = m.now()
			m.mu.Unlock()
		}
	}
}

// sleeper is lifecycle's Sleep, saying once per scope that the mark is
// waiting: MarkActive waits while a teardown holds the lease.
func (m *marker) sleeper(scopes []string) func(ctx context.Context, d time.Duration) error {
	what := strings.Join(scopes, ", ")
	return func(ctx context.Context, d time.Duration) error {
		m.mu.Lock()
		if !m.waiting[what] {
			m.waiting[what] = true
			fmt.Fprintf(m.out, "waiting for spawn's teardown lease in %s (the %s table is being created, or a teardown holds the lease; up to %s)...\n",
				what, lifecycle.TableName, lifecycle.DefaultWaitTimeout)
		}
		sleep := m.sleep
		m.mu.Unlock()
		if sleep != nil {
			return sleep(ctx, d)
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
}

// isInfra reports whether cfg's account is spore.host's infra account. An
// account that cannot be resolved is marked as any other: the mark itself
// will then most likely fail, and warn.
func (m *marker) isInfra(ctx context.Context, cfg aws.Config, who string) bool {
	m.mu.Lock()
	acct, ok := m.accounts[who]
	lookup := m.accountOf
	m.mu.Unlock()
	if !ok || who == "" {
		var err error
		if acct, err = lookup(ctx, cfg); err != nil {
			return false
		}
		if who != "" {
			m.mu.Lock()
			m.accounts[who] = acct
			m.mu.Unlock()
		}
	}
	return acct == footprintspawn.InfraAccountID
}

func (m *marker) isRefused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refused
}

// fail records a failed mark: one warning per process, and no more marks
// after a refusal.
func (m *marker) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if errors.Is(err, errInTest) {
		m.refused = true
		return
	}
	if isRefusal(err) {
		m.refused = true
	}
	if m.warned {
		return
	}
	m.warned = true
	fmt.Fprintf(m.out, "warning: could not mark spawn active in the shared-account lifecycle lease (DynamoDB table %s): %v\n"+
		"  Continuing: this does not stop the launch. Until it can be marked, spawn's idle teardown treats the lease\n"+
		"  in this account as unavailable and never tears the account down for idleness. To enable it, grant\n"+
		"  %s on %s.\n",
		lifecycle.TableName, err, strings.Join(RequiredActions, ", "), TableARNPattern)
}

// isRefusal reports whether err means this identity may not use the table at
// all (rather than a passing failure), so it is not asked again.
func isRefusal(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code := ae.ErrorCode()
		return strings.Contains(code, "AccessDenied") || strings.Contains(code, "UnrecognizedClient") ||
			strings.Contains(code, "InvalidClientTokenId") || strings.Contains(code, "ExpiredToken")
	}
	s := err.Error()
	return strings.Contains(s, "AccessDenied") || strings.Contains(s, "not authorized")
}

// identity keys the cache: the credentials' access key ID, or "" (no cache)
// when they cannot be read.
func identity(ctx context.Context, cfg aws.Config) string {
	if cfg.Credentials == nil {
		return ""
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return ""
	}
	return creds.AccessKeyID
}
