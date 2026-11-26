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
// Mark returns nothing, and prints nothing a user sees when it fails. Whatever
// goes wrong (the caller's identity may not read or write the table, DynamoDB
// is throttling, a teardown lease is held past lifecycle's 10 minute wait), it
// writes a line to spawn's diagnostic log (~/.spawn/spawn.log, and stderr with
// --verbose: the standard log package, which cmd routes there) and the launch
// goes on. A user whose IAM lacks the lease actions is the normal case today,
// and every CLI invocation is a new process, so even a once-per-process
// warning would be a warning on every launch. `spawn doctor` reports it
// instead ("idle teardown", Probe). An identity that is refused is not asked
// again in the same process, so a pool of a hundred workers waits for the
// lease at most once. The one thing a user does see is a long wait: when a
// teardown really holds the lease, a mark waits (up to lifecycle's 10
// minutes), and after waitNotice it says why on stderr.
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
// fenced by the lease. `spawn setup-account` provisions these actions as the
// customer-managed policy UserPolicy (spawn#1666); an account is lease-enabled
// for the users an administrator attaches it to.
//
// Outside the aws partition (GovCloud, China, the ISO partitions) ScopeGlobal
// is not marked: its table is in ControlRegion, which those credentials
// cannot reach (GlobalReachable). Regional scopes are marked as anywhere else.
package lifecyclemark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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
// region (its --lease-region), and the reaper reads it from here. It is in the
// aws partition: credentials for another partition cannot reach it
// (GlobalReachable).
const ControlRegion = "us-east-1"

// awsPartitionRegion is the aws partition's region pattern, as the SDK's
// endpoint rules give it (internal/endpoints/awsrulesfn, partition "aws").
// GovCloud (us-gov-*), China (cn-*), the ISO partitions and the European
// Sovereign Cloud do not match.
var awsPartitionRegion = regexp.MustCompile(`^(us|eu|ap|sa|ca|me|af|il|mx)-\w+-\d+$`)

// GlobalReachable reports whether credentials used in region can reach
// ControlRegion's table, which holds ScopeGlobal: whether region is in the aws
// partition. An empty region says nothing about the partition and counts as
// reachable, as Mark has always treated it. Outside the aws partition Mark
// does not mark ScopeGlobal (spawn#1672): the call could only fail, and a
// failure there is a refusal that would stop the process's regional marks
// too.
func GlobalReachable(region string) bool {
	return region == "" || awsPartitionRegion.MatchString(region)
}

// ScopeGlobal is the scope of spawn's account-wide resources.
const ScopeGlobal = lifecycle.ScopeGlobal

// Owner is how a CLI's marks identify themselves in the table's tags.
const Owner = "spawn-cli"

// RequiredActions are what an identity needs on the lifecycle table
// (arn:aws:dynamodb:*:*:table/spore-host-lifecycle) for Mark to work.
// MarkActive is one transaction of a ConditionCheck and an Update, and creates
// the table, tagged, the first time. DynamoDB authorizes a transaction per
// item action. internal/iamcoverage follows a transaction's items where the
// call site builds them, but MarkActive's condition check comes from a helper
// (libs/lifecycle sentinelAbsent) it does not look into, so it counts every
// item action a TransactWriteItems may hold, hence PutItem. DeleteItem is for
// one rare case: removing the closing sentinel a crashed CloseTableIfEmpty
// abandoned, which would otherwise block every mark in that region. A test
// (TestLeaseRequiredActionsAreWhatMarkCalls) holds this list to the actions
// internal/iamcoverage finds Mark calling.
var RequiredActions = []string{
	"dynamodb:ConditionCheckItem", "dynamodb:UpdateItem", "dynamodb:PutItem", "dynamodb:DeleteItem",
	"dynamodb:DescribeTable", "dynamodb:CreateTable", "dynamodb:TagResource",
}

// TableARNPattern is the resource RequiredActions are needed on.
const TableARNPattern = "arn:aws:dynamodb:*:*:table/" + lifecycle.TableName

// VerdictReadActions are what `spawn reaper teardown --if-idle-for` needs on
// the lifecycle table beyond RequiredActions: it reads the idle verdict
// (pkg/reaperdeploy.LeaseReader).
var VerdictReadActions = []string{"dynamodb:GetItem"}

// UserPolicy is the customer-managed policy, at its IAM path, that
// `spawn setup-account` creates to grant a CLI user UserActions on the
// lifecycle table (spawn#1666). An administrator attaches it to the groups or
// roles spawn users sign in with; `spawn setup-account --remove` deletes it.
const UserPolicy = "spore-host/spawn-user-lifecycle"

// UserActions are what UserPolicy grants: RequiredActions (the mark, and
// `spawn doctor`'s read-only probe, which uses three of them: DescribeTable,
// ConditionCheckItem and UpdateItem) and VerdictReadActions, sorted.
func UserActions() []string {
	out := append(append([]string{}, RequiredActions...), VerdictReadActions...)
	sort.Strings(out)
	return out
}

// cacheFor is how long a successful mark stands in for the next one in the
// same process and scope. Well inside the 24 h grace, so a long-running
// process still marks again before any later create (the lease assumes a
// creator creates within the grace of its MarkActive).
const cacheFor = time.Hour

// waitNotice is how long a mark waits on the lease before it tells the user
// on stderr. Creating the table on a region's first mark takes a few seconds
// and is not worth a line; a teardown holding the lease is.
const waitNotice = 30 * time.Second

// Factory returns the DynamoDB client for one region of cfg's account.
type Factory func(cfg aws.Config, region string) (lifecycle.API, error)

// marker is the process's marking state.
type marker struct {
	mu      sync.Mutex
	factory Factory
	// out is user-visible (stderr): only the long-wait notice goes there.
	out io.Writer
	// debugf is spawn's diagnostic log; every failure goes there, and only
	// there.
	debugf func(format string, args ...any)
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	// refused: an identity was refused the table; no more marks this process.
	refused bool
	logged  bool
	// loggedGlobal: the skip of an unreachable global scope was logged.
	loggedGlobal bool
	waiting      map[string]time.Duration
	marked       map[string]time.Time
	// accountOf resolves cfg's account (sts:GetCallerIdentity, which needs no
	// permission); accounts caches it by access key.
	accountOf func(ctx context.Context, cfg aws.Config) (string, error)
	accounts  map[string]string
}

func newMarker() *marker {
	return &marker{
		factory:   defaultFactory,
		out:       os.Stderr,
		debugf:    log.Printf,
		now:       time.Now,
		waiting:   map[string]time.Duration{},
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

// Override replaces the client factory, the user-visible writer and the
// diagnostic logger (nil keeps the current one), and clears the process's
// state, for a test. It returns the function that restores them.
func Override(f Factory, out io.Writer, debugf func(format string, args ...any)) (restore func()) {
	std.mu.Lock()
	defer std.mu.Unlock()
	saved := struct {
		factory Factory
		out     io.Writer
		debugf  func(format string, args ...any)
		now     func() time.Time
		sleep   func(ctx context.Context, d time.Duration) error
	}{std.factory, std.out, std.debugf, std.now, std.sleep}
	if f != nil {
		std.factory = f
	}
	if out != nil {
		std.out = out
	}
	if debugf != nil {
		std.debugf = debugf
	}
	std.refused, std.logged, std.loggedGlobal = false, false, false
	std.waiting, std.marked, std.accounts = map[string]time.Duration{}, map[string]time.Time{}, map[string]string{}
	return func() {
		std.mu.Lock()
		defer std.mu.Unlock()
		std.factory, std.out, std.debugf, std.now, std.sleep = saved.factory, saved.out, saved.debugf, saved.now, saved.sleep
		std.refused, std.logged, std.loggedGlobal = false, false, false
		std.waiting, std.marked, std.accounts = map[string]time.Duration{}, map[string]time.Time{}, map[string]string{}
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
		if s == ScopeGlobal && !GlobalReachable(cfg.Region) {
			if !m.loggedGlobal {
				m.loggedGlobal = true
				m.debugf("lifecyclemark: not marking the global scope: its table is in %s, which credentials for %s cannot reach",
					ControlRegion, cfg.Region)
			}
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

// sleeper is lifecycle's Sleep. MarkActive waits while the table is being
// created (seconds, on a region's first mark) or a teardown holds the lease
// (up to 10 minutes). The wait goes to the diagnostic log at once, and to the
// user once it passes waitNotice.
func (m *marker) sleeper(scopes []string) func(ctx context.Context, d time.Duration) error {
	what := strings.Join(scopes, ", ")
	return func(ctx context.Context, d time.Duration) error {
		m.mu.Lock()
		before := m.waiting[what]
		m.waiting[what] = before + d
		if before == 0 {
			m.debugf("lifecyclemark: waiting on the %s table in %s (being created, or a teardown holds the lease)", lifecycle.TableName, what)
		}
		if before < waitNotice && before+d >= waitNotice {
			fmt.Fprintf(m.out, "waiting for spawn's teardown lease in %s (a teardown holds it; up to %s)...\n",
				what, lifecycle.DefaultWaitTimeout)
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
// will then most likely fail, and log.
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

// fail records a failed mark: one diagnostic log line per process, nothing
// the user sees, and no more marks after a refusal.
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
	if m.logged {
		return
	}
	m.logged = true
	m.debugf("lifecyclemark: could not mark spawn active in the shared-account lifecycle lease (DynamoDB table %s): %v. "+
		"The create goes on; the idle teardown treats this account's lease as unavailable and never tears it down for idleness. "+
		"It needs %s on %s, which the policy %s grants: `spawn setup-account` creates it, and an administrator attaches it "+
		"to this identity's group or role. `spawn doctor` checks it.",
		lifecycle.TableName, err, strings.Join(RequiredActions, ", "), TableARNPattern, UserPolicy)
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

// ErrNoTable is Probe's answer when the region has no lifecycle table yet:
// the identity may describe it, and the first mark creates it if it may.
var ErrNoTable = errors.New("no " + lifecycle.TableName + " table in this region yet")

// ErrInconclusive is Probe's answer when its transaction was cancelled for a
// reason other than its conditions (a throttle, a conflicting transaction):
// that proves nothing either way, so it is neither a pass nor a refusal.
var ErrInconclusive = errors.New("the lease probe was inconclusive")

// probeKey and probeCheckKey are the items Probe's transaction names: the
// update's and the condition check's (a transaction may not name one item
// twice). Neither condition can ever hold, so neither item is ever written:
// a stray item would stop CloseTableIfEmpty from ever removing the table.
const (
	probeKey      = "probe#spawn-doctor"
	probeCheckKey = "probe#spawn-doctor-check"
)

// probeNever is a condition no item satisfies.
const probeNever = "attribute_exists(#k) AND attribute_not_exists(#k)"

// Probe reports, changing nothing, whether api's identity can use the lease
// table, for `spawn doctor`: a DescribeTable, then a transaction of the shape
// a mark makes (lifecycle.MarkActive: a ConditionCheck and an Update) whose
// conditions can never hold. DynamoDB authorizes a transaction per item
// action, dynamodb:ConditionCheckItem and dynamodb:UpdateItem here, before it
// evaluates any condition, so a cancellation by those conditions proves both
// are allowed and that nothing was written; a refusal of either is an
// AccessDeniedException for the whole call. A cancellation for any other
// reason is ErrInconclusive. Probing the UpdateItem alone would pass an
// identity granted UpdateItem but not ConditionCheckItem, whose every mark is
// refused (spawn#1672). nil means usable; ErrNoTable that the table does not
// exist yet; ErrInconclusive that the probe proved nothing; any other error is
// the refusal.
func Probe(ctx context.Context, api lifecycle.API) error {
	_, err := api.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(lifecycle.TableName)})
	var nf *ddbtypes.ResourceNotFoundException
	switch {
	case errors.As(err, &nf):
		return ErrNoTable
	case err != nil:
		return fmt.Errorf("dynamodb:DescribeTable: %w", err)
	}
	key := func(k string) map[string]ddbtypes.AttributeValue {
		return map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: k}}
	}
	_, err = api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []ddbtypes.TransactWriteItem{
		{ConditionCheck: &ddbtypes.ConditionCheck{
			TableName:                aws.String(lifecycle.TableName),
			Key:                      key(probeCheckKey),
			ConditionExpression:      aws.String(probeNever),
			ExpressionAttributeNames: map[string]string{"#k": "pk"},
		}},
		{Update: &ddbtypes.Update{
			TableName:                 aws.String(lifecycle.TableName),
			Key:                       key(probeKey),
			UpdateExpression:          aws.String("SET #p = :p"),
			ConditionExpression:       aws.String(probeNever),
			ExpressionAttributeNames:  map[string]string{"#p": "probe", "#k": "pk"},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":p": &ddbtypes.AttributeValueMemberS{Value: "spawn doctor"}},
		}},
	}})
	var cancelled *ddbtypes.TransactionCanceledException
	switch {
	case errors.As(err, &cancelled):
		if codes, ok := cancelledByConditions(cancelled.CancellationReasons); !ok {
			return fmt.Errorf("%w: its transaction was cancelled for %s; try again", ErrInconclusive, codes)
		}
		return nil
	case err == nil:
		return errors.New("the probe's never-true conditions held; the table is not behaving as DynamoDB")
	}
	return fmt.Errorf("the mark's transaction (dynamodb:ConditionCheckItem, dynamodb:UpdateItem): %w", err)
}

// cancelledByConditions reports whether a cancellation is the probe's own:
// at least one ConditionalCheckFailed and nothing but ConditionalCheckFailed
// or None. It also returns the reasons' codes, for the message when it is not.
func cancelledByConditions(reasons []ddbtypes.CancellationReason) (string, bool) {
	codes := make([]string, 0, len(reasons))
	failed, other := false, false
	for _, r := range reasons {
		code := aws.ToString(r.Code)
		codes = append(codes, code)
		switch code {
		case "ConditionalCheckFailed":
			failed = true
		case "None":
		default:
			other = true
		}
	}
	if len(codes) == 0 {
		return "no reason", false
	}
	return strings.Join(codes, ", "), failed && !other
}
