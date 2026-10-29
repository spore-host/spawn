package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// #1366: a foreground parameter sweep (--no-detach) accepted --fsx-create,
// --fsx-recall and --vpc without --subnet and launched every member without
// them, and it never mounted --efs-id, --fsx-id or an --attach-volume volume.
// These tests run the foreground member-building path (foregroundSweepMembers,
// renderForegroundSweepUserData) and look at what each member is launched
// with: its config, which aws.Client.Launch turns into RunInstances, and its
// user data.

// fakeStorageLookups answers a sweep member's storage lookups without AWS.
type fakeStorageLookups struct {
	mu        sync.Mutex
	fsxCalls  []string
	missingIn string // the region whose GetFSxFilesystem finds nothing
}

func (f *fakeStorageLookups) EFSMountTargetIP(_ context.Context, _, _, _, _ string) (string, error) {
	return "10.1.2.3", nil
}

func (f *fakeStorageLookups) GetFSxFilesystem(_ context.Context, id, region string) (*aws.FSxInfo, error) {
	f.mu.Lock()
	f.fsxCalls = append(f.fsxCalls, id+"|"+region)
	f.mu.Unlock()
	if region == f.missingIn {
		return nil, fmt.Errorf("FileSystemNotFound: %s", id)
	}
	return &aws.FSxInfo{FileSystemID: id, DNSName: id + ".fsx." + region + ".amazonaws.com", MountName: "abcdmnt"}, nil
}

// testSweepKeyName is a key pair the test platform holds a key for.
const testSweepKeyName = "sample-keyname"

// testSweepPlat is a platform with a default public key and one for
// testSweepKeyName.
func testSweepPlat(t *testing.T) *platform.Platform {
	t.Helper()
	dir := t.TempDir()
	ssh := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(ssh, "id_default.pub")
	for path, body := range map[string]string{
		pub:                                  "ssh-ed25519 AAAAsweepdefault spawn\n",
		filepath.Join(ssh, testSweepKeyName): "not read: the .pub beside it is\n",
		filepath.Join(ssh, testSweepKeyName+".pub"): "ssh-ed25519 AAAAsweepnamed spawn\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &platform.Platform{OS: "linux", HomeDir: dir, SSHPubKeyPath: pub}
}

// foregroundMembers builds a foreground sweep's members over base and pf as
// launchParameterSweep does, with the AMI and target OS it resolves from AWS
// fixed, and renders their user data with storage.
func foregroundMembers(t *testing.T, plat *platform.Platform, base aws.LaunchConfig, pf *ParamFileFormat, storage sweepStorageLookups) ([]*aws.LaunchConfig, error) {
	t.Helper()
	cfgs, _, err := foregroundSweepMembers(base, pf, "sw-1", "sw")
	if err != nil {
		return nil, err
	}
	for _, c := range cfgs {
		if c.AMI == "" {
			c.AMI = "ami-0123456789abcdef0"
		}
		if c.TargetOS == "" {
			c.TargetOS = "linux"
		}
	}
	if err := renderForegroundSweepUserData(context.Background(), plat, cfgs, newSweepStorage(storage, io.Discard)); err != nil {
		return nil, err
	}
	return cfgs, nil
}

func twoRows() *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "m7i.large", "region": "us-east-1", "ttl": "1h"},
		Params:   []map[string]interface{}{{"trial": "a"}, {"trial": "b", "az": "us-east-1c"}},
	}
}

// memberScript is a member's user data as cloud-init runs it.
func memberScript(t *testing.T, cfg *aws.LaunchConfig) string {
	t.Helper()
	return string(decodeUserDataOnce(t, cfg.UserData))
}

// --- carried: storage mounts -------------------------------------------------

// --efs-id: every member's user data mounts the filesystem at --efs-mount-point
// before the workload, with the mount-target IP fallback (#704), and its
// --command waits for the mount (#668), as a single launch's does.
func TestForegroundSweepMembersMountEFS(t *testing.T) {
	base := aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", EFSID: "fs-0123456789abcdef0", EFSMountPoint: "/shared"}
	cfgs, err := foregroundMembers(t, testSweepPlat(t), base, twoRows(), &fakeStorageLookups{})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cfgs {
		s := memberScript(t, c)
		for _, want := range []string{"fs-0123456789abcdef0.efs.us-east-1.amazonaws.com", "/shared", "10.1.2.3"} {
			if !strings.Contains(s, want) {
				t.Errorf("member %d's user data does not mount the --efs-id filesystem: no %q", i, want)
			}
		}
	}
}

// --fsx-id: every member mounts the filesystem, found in its region, and its
// tags carry the mount name the lookup found, as a single launch's do (#314).
func TestForegroundSweepMembersMountFSx(t *testing.T) {
	base := aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", FSxLustreID: "fs-0fsx", FSxMountPoint: "/lustre"}
	f := &fakeStorageLookups{}
	cfgs, err := foregroundMembers(t, testSweepPlat(t), base, twoRows(), f)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cfgs {
		s := memberScript(t, c)
		for _, want := range []string{"fs-0fsx.fsx.us-east-1.amazonaws.com", "abcdmnt", "/lustre"} {
			if !strings.Contains(s, want) {
				t.Errorf("member %d's user data does not mount the --fsx-id filesystem: no %q", i, want)
			}
		}
		if tags := gateTags(t, *c); tags["spawn:fsx-mount-name"] != "abcdmnt" || tags["spawn:fsx-id"] != "fs-0fsx" {
			t.Errorf("member %d's tags do not carry the filesystem and its mount name: %v", i, tags)
		}
	}
	if len(f.fsxCalls) != 1 {
		t.Errorf("the filesystem was looked up %d times for one region; want once: %v", len(f.fsxCalls), f.fsxCalls)
	}

	// A member in a region the filesystem is not in fails the sweep.
	pf := twoRows()
	pf.Params[1]["region"] = "us-west-2"
	if _, err := foregroundMembers(t, testSweepPlat(t), base, pf, &fakeStorageLookups{missingIn: "us-west-2"}); err == nil ||
		!strings.Contains(err.Error(), "--fsx-id fs-0fsx in us-west-2") {
		t.Errorf("a member in a region without the filesystem was rendered: %v", err)
	}
}

// --attach-volume: every member's volume is attached (the launch settings) and
// mounted (the user data).
func TestForegroundSweepMembersMountAttachedVolumes(t *testing.T) {
	base := aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1",
		AttachVolumes: []aws.AttachVolumeSpec{{SnapshotID: "snap-0123", MountPoint: "/refdata"}}}
	cfgs, err := foregroundMembers(t, testSweepPlat(t), base, twoRows(), &fakeStorageLookups{})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cfgs {
		if !strings.Contains(memberScript(t, c), "/refdata") {
			t.Errorf("member %d's user data does not mount its attached volume at /refdata", i)
		}
		if len(aws.NewLaunchSettings(*c, "", 0).AttachVolumes) != 1 {
			t.Errorf("member %d's launch settings do not attach the volume", i)
		}
	}
}

// The detached path renders the same mounts into the user data it ships.
func TestDetachedSweepMembersMountStorage(t *testing.T) {
	base := aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1",
		EFSID: "fs-0123456789abcdef0", EFSMountPoint: "/shared", FSxLustreID: "fs-0fsx", FSxMountPoint: "/lustre"}
	pf := twoRows()
	pf.Defaults["ami"] = "ami-1"
	if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
		t.Fatal(err)
	}
	for i := range pf.Params {
		ud, _ := pf.Params[i][detachedUserDataKey].(string)
		if ud == "" {
			ud, _ = pf.Defaults[detachedUserDataKey].(string)
		}
		s := string(decodeUserDataOnce(t, ud))
		for _, want := range []string{"fs-0123456789abcdef0.efs.us-east-1.amazonaws.com", "/shared", "fs-0fsx.fsx.us-east-1.amazonaws.com", "/lustre"} {
			if !strings.Contains(s, want) {
				t.Errorf("detached member %d's user data does not mount its storage: no %q", i, want)
			}
		}
	}
}

// --- carried: --spot-max-price ------------------------------------------------

// --spot-max-price reaches every member's Spot request; a row's own
// spot_max_price: wins. buildLaunchConfig never read the flag (#1366).
func TestSweepMembersCarrySpotMaxPrice(t *testing.T) {
	keepLaunchGlobals(t)
	isolateAWSEnv(t)
	for name, v := range map[string]string{"instance-type": "m7i.large", "region": "us-east-1", "spot": "true", "spot-max-price": "0.25"} {
		setLaunchFlag(t, name, v)
	}
	base, err := sweepBaseConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pf := twoRows()
	pf.Params[1]["spot_max_price"] = "0.40"
	cfgs, err := foregroundMembers(t, testSweepPlat(t), *base, pf, &fakeStorageLookups{})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"0.25", "0.40"} {
		if got := aws.NewLaunchSettings(*cfgs[i], "", 0); !got.Spot || got.SpotMaxPrice != want {
			t.Errorf("member %d launches with spot=%t spot_max_price=%q; want %q", i, got.Spot, got.SpotMaxPrice, want)
		}
	}
}

// --- refused ------------------------------------------------------------------

// The issue's three, and the rest of the refused flags, are refused by
// runLaunch for a sweep before it makes a single AWS call: not even the audit
// identity lookup. The issue's three are named outright, since a test that
// reads the same table as the guard cannot catch one being reclassified.
func TestSweepRefusalsMakeNoAWSCall(t *testing.T) {
	cases := map[string]struct {
		flags map[string]string
		want  string // in the error: the flag and the remedy
		alt   string
	}{
		"fsx-create":         {map[string]string{"fsx-create": "true", "fsx-lifecycle": "ephemeral", "fsx-s3-bucket": "b"}, "--fsx-create:", "--fsx-id"},
		"fsx-recall":         {map[string]string{"fsx-recall": "my-stack"}, "--fsx-recall:", "--fsx-id"},
		"vpc without subnet": {map[string]string{"vpc": "vpc-0123"}, "--vpc:", "Pass --subnet"},
	}
	for name, rule := range sweepLaunchFlags {
		if rule.kind == sweepRefused && cases[name].want == "" {
			cases["table: "+name] = struct {
				flags     map[string]string
				want, alt string
			}{map[string]string{name: ""}, "--" + name + ":", ""}
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := serveTagLimitAWS(t)
			dir := t.TempDir()
			pf := filepath.Join(dir, "sweep.yaml")
			if err := os.WriteFile(pf, []byte("defaults:\n  instance_type: m7i.large\n  ttl: 1h\nparams:\n  - alpha: 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.flags["param-file"], tc.flags["region"], tc.flags["no-detach"] = pf, "us-east-1", "true"
			for k, v := range tc.flags {
				if v == "" {
					// A table case: the guard reads only whether the flag was
					// set. (A map flag cannot be set and put back.)
					launchCmd.Flags().Lookup(k).Changed = true
					continue
				}
				setLaunchFlag(t, k, v)
			}
			stubTeamName(t)
			err := runLaunch(launchCmd, nil)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.actions) != 0 {
				t.Errorf("the sweep made %d AWS call(s) before it was refused: %v", len(f.actions), f.actions)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.alt) {
				t.Errorf("runLaunch = %v; want a refusal naming %q and %q", err, tc.want, tc.alt)
			}
		})
	}
}

// --vpc with --subnet is not refused: the subnet names the VPC.
func TestSweepAcceptsVPCWithSubnet(t *testing.T) {
	for _, alias := range []string{"subnet", "subnet-id"} {
		if err := sweepUnsupportedFlags(changedOnly("vpc", alias)); err != nil {
			t.Errorf("--vpc with --%s was refused: %v", alias, err)
		}
	}
}

// --cartesian is deprecated and inert (#674) and cobra already warns; refusing
// it on a sweep would break scripts and protect nothing, so it is accepted.
func TestSweepAcceptsDeprecatedCartesian(t *testing.T) {
	if err := sweepUnsupportedFlags(changedOnly("cartesian")); err != nil {
		t.Errorf("--cartesian on a sweep was refused: %v", err)
	}
}

// The backstop where each path builds its members: a member config with one
// of the three is refused, by the foreground path and the detached one alike,
// for a caller that did not come through runLaunch.
func TestSweepMemberBuildersRefuseWhatNoSweepCanLaunch(t *testing.T) {
	for name, tc := range map[string]struct {
		base     aws.LaunchConfig
		want     string
		instead  string
		detached string
	}{
		"fsx-create": {aws.LaunchConfig{FSxLustreCreate: true, FSxLifecycle: "ephemeral", FSxS3Bucket: "b"}, "--fsx-create", "--fsx-id", ""},
		"fsx-recall": {aws.LaunchConfig{FSxLustreRecall: "stack"}, "--fsx-recall", "--fsx-id", ""},
		"vpc":        {aws.LaunchConfig{VPCID: "vpc-1"}, "--vpc without --subnet", "Pass --subnet", ""},
	} {
		tc.base.InstanceType, tc.base.Region = "m7i.large", "us-east-1"
		_, err := foregroundMembers(t, testSweepPlat(t), tc.base, twoRows(), &fakeStorageLookups{})
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.instead) {
			t.Errorf("%s: the foreground path built the members: %v", name, err)
		}
		pf := twoRows()
		pf.Defaults["ami"] = "ami-1"
		derr := seedDetachedBootstrap(pf, tc.base, "sw-1", "sw", testDetachedBootstrap(t))
		if derr == nil || !strings.Contains(derr.Error(), tc.want) || !strings.Contains(derr.Error(), tc.instead) {
			t.Errorf("%s: the detached path seeded the members: %v", name, derr)
		}
		// Same message on both paths: one remedy, whichever the sweep takes.
		if err != nil && derr != nil && strings.TrimPrefix(err.Error(), "parameter set 0: ") != strings.TrimPrefix(derr.Error(), "param set 0: ") {
			t.Errorf("%s: the two paths refuse differently:\n  foreground: %v\n  detached:   %v", name, err, derr)
		}
	}
}

// --- discovery: every LaunchConfig field --------------------------------------

// How a foreground sweep member gets each LaunchConfig field, or why it does
// not.
type foregroundDisposition struct {
	settings bool // in aws.LaunchSettings, which Client.Launch applies to RunInstances
	tags     bool // in the member's instance tags
	userData bool // in the member's user data
	refused  bool // sweepMemberGuard refuses it
	// direct: read by aws.Client.Launch itself, outside LaunchSettings.
	direct string
	// why explains a field carried another way, or not at all. Required when
	// nothing above is claimed.
	why    string
	prereq func(*aws.LaunchConfig)
}

const whyDirect = "read by aws.Client.Launch itself"

// foregroundDispositions is every LaunchConfig field's disposition on a
// foreground sweep. TestForegroundSweepCarriesOrRefusesEveryLaunchSetting
// fails on a field missing here, and checks each claim against the code.
var foregroundDispositions = map[string]foregroundDisposition{
	"InstanceType":       {direct: whyDirect + " (RunInstances InstanceType)"},
	"Region":             {direct: whyDirect + " (the region it launches in)"},
	"AMI":                {direct: whyDirect + " (RunInstances ImageId)"},
	"KeyName":            {direct: whyDirect + " (RunInstances KeyName)", userData: true},
	"IamInstanceProfile": {direct: whyDirect + " (RunInstances IamInstanceProfile)"},
	"UserData":           {why: "overwritten with the rendered bootstrap (renderForegroundSweepUserData)"},
	"CustomUserData":     {userData: true},
	"ClientToken":        {direct: whyDirect + " (RunInstances ClientToken); no sweep sets one"},
	"EFAInterfaces":      {direct: whyDirect + " (the multi-card interfaces and their Elastic IP, #661)", prereq: func(c *aws.LaunchConfig) { c.EFAEnabled = true }},
	"EFASecondaryType": {direct: whyDirect + " (the type of the interfaces on cards after card 0, #1404)", prereq: func(c *aws.LaunchConfig) {
		c.EFAEnabled, c.EFAInterfaces = true, 2
	}},

	"AvailabilityZone":     {settings: true},
	"SecurityGroupIDs":     {settings: true},
	"SubnetID":             {settings: true},
	"Spot":                 {settings: true, tags: true},
	"SpotMaxPrice":         {settings: true},
	"ReservationID":        {settings: true},
	"CapacityBlock":        {settings: true},
	"Hibernate":            {settings: true},
	"PlacementGroup":       {settings: true},
	"EFAEnabled":           {settings: true},
	"NestedVirtualization": {settings: true},
	"RootVolumeSizeGiB":    {settings: true},
	"AttachVolumes":        {settings: true, userData: true},
	"EBSEncrypted":         {settings: true},
	"EBSKMSKeyID":          {settings: true},
	"IMDSv2Enforced":       {settings: true},
	"IMDSv2HopLimit":       {settings: true},
	"HibernateOnIdle":      {settings: true, tags: true},

	"VPCID":           {refused: true},
	"FSxLustreCreate": {refused: true},
	"FSxLustreRecall": {refused: true},

	"PlacementGroupPrefix": {why: whyNotSweep + " (an MPI job array's per-AZ groups)"},
	"DNSFunctionARN":       {why: whyNotSweep + " (the custom-DNS preflight; a sweep refuses --dns-function-arn)"},
	"FSxPending":           {tags: true},
	"FSxImportPath":        {why: "tagged only beside FSxPending, which only --fsx-create sets, and that is refused"},
	"FSxExportPath":        {why: "tagged only beside FSxPending, which only --fsx-create sets, and that is refused"},
	"FSxLifecycle":         {why: "valid only with --fsx-create, which is refused"},
	"FSxTTL":               {why: "valid only with --fsx-create, which is refused"},
	"FSxStorageCapacity":   {why: "read only by --fsx-create, which is refused"},
	"FSxS3Bucket":          {why: "read only by --fsx-create, which is refused"},

	"EFSID":         {tags: true, userData: true},
	"EFSMountPoint": {tags: true, userData: true, prereq: func(c *aws.LaunchConfig) { c.EFSID = "fs-1" }},
	"FSxLustreID":   {tags: true, userData: true},
	"FSxMountName":  {tags: true, prereq: func(c *aws.LaunchConfig) { c.FSxLustreID = "fs-2" }},
	"FSxMountPoint": {tags: true, userData: true, prereq: func(c *aws.LaunchConfig) { c.FSxLustreID = "fs-2" }},
	"TargetOS":      {tags: true, userData: true},

	"TTL": {tags: true}, "IdleTimeout": {tags: true}, "TerminateOnIdle": {tags: true}, "CostLimit": {tags: true},
	"DNSName": {tags: true}, "IdleCPUPercent": {tags: true}, "Observability": {tags: true},
	"DNSDomain":   {tags: true, prereq: func(c *aws.LaunchConfig) { c.DNSName = "box" }},
	"DNSEndpoint": {tags: true, prereq: func(c *aws.LaunchConfig) { c.DNSName = "box" }},
	"PreStop":     {tags: true}, "PreStopTimeout": {tags: true, prereq: func(c *aws.LaunchConfig) { c.PreStop = "sync" }},
	"SpotInterruptionWebhookURL": {tags: true},
	"WebhookCorrelation":         {tags: true, prereq: func(c *aws.LaunchConfig) { c.SpotInterruptionWebhookURL = "https://h.example.org/" }},
	"WebhookTimeout":             {tags: true, prereq: func(c *aws.LaunchConfig) { c.SpotInterruptionWebhookURL = "https://h.example.org/" }},
	"CompletionWebhookURL":       {tags: true}, "OnComplete": {tags: true},
	"CompletionFile":  {tags: true, userData: true, prereq: func(c *aws.LaunchConfig) { c.OnComplete = "terminate" }},
	"CompletionDelay": {tags: true}, "SessionTimeout": {tags: true}, "Username": {tags: true},
	"JobArrayID":    {tags: true},
	"JobArrayName":  {tags: true, prereq: func(c *aws.LaunchConfig) { c.JobArrayID = "ja" }},
	"JobArraySize":  {tags: true, prereq: func(c *aws.LaunchConfig) { c.JobArrayID = "ja" }},
	"JobArrayIndex": {tags: true, prereq: func(c *aws.LaunchConfig) { c.JobArrayID = "ja" }}, "JobArrayCommand": {tags: true},
	"SweepID": {tags: true}, "SweepName": {tags: true}, "SweepIndex": {tags: true}, "SweepSize": {tags: true},
	"Parameters": {tags: true}, "SlackWorkspaceID": {tags: true}, "NotifyURL": {tags: true}, "NotifyCommand": {tags: true},
	"NotifyPlatform": {tags: true}, "ActivePortsRaw": {tags: true}, "ActiveProcessesRaw": {tags: true},
	"DCVSessionID": {tags: true}, "AppName": {tags: true}, "AppMode": {tags: true}, "ReadyPort": {tags: true},
	"ReadyHealthPath": {tags: true}, "WebAuth": {tags: true}, "PricePerHour": {tags: true},
	"PriceSource": {tags: true, prereq: func(c *aws.LaunchConfig) { c.PricePerHour = 0.1 }}, "Name": {tags: true}, "Tags": {tags: true}, "SpawnVersion": {tags: true},
}

// TestForegroundSweepCarriesOrRefusesEveryLaunchSetting is #1366's discovery
// gate, the foreground twin of #1353's: every LaunchConfig field is found by
// reflection, so one added later fails here until a foreground member carries
// it (launch settings, tags, user data, or read by Client.Launch) or
// sweepMemberGuard refuses it.
//
// Each claim is checked against the code: a field said to be in the settings,
// the tags or the user data must change them, one said to be refused must be,
// and one claiming none of those must change none of them.
func TestForegroundSweepCarriesOrRefusesEveryLaunchSetting(t *testing.T) {
	plat := testSweepPlat(t)
	base := aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", SweepID: "sw", SweepName: "sw", TargetOS: "linux"}
	render := func(cfg aws.LaunchConfig) string {
		c := cfg
		if err := renderForegroundSweepUserData(context.Background(), plat, []*aws.LaunchConfig{&c},
			newSweepStorage(&fakeStorageLookups{}, io.Discard)); err != nil {
			t.Fatalf("render user data: %v", err)
		}
		return string(decodeUserDataOrPlain(t, c))
	}
	typ := reflect.TypeOf(aws.LaunchConfig{})
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		seen[name] = true
		d, ok := foregroundDispositions[name]
		if !ok {
			t.Errorf("LaunchConfig.%s has no foreground-sweep disposition. A foreground member must carry it "+
				"(aws.LaunchSettings, Client.Launch itself, the tags or the user data) or sweepMemberGuard must "+
				"refuse it; then record which in foregroundDispositions (#1366)", name)
			continue
		}
		if !(d.settings || d.tags || d.userData || d.refused) && d.direct == "" && d.why == "" {
			t.Errorf("LaunchConfig.%s: a disposition with nothing carried and no reason", name)
		}

		before := base
		if d.prereq != nil {
			d.prereq(&before)
		}
		after := before
		if !sampleField(reflect.ValueOf(&after).Elem().Field(i), name) {
			t.Fatalf("cannot sample LaunchConfig.%s (%s): teach sampleField its kind", name, typ.Field(i).Type)
		}
		if name == "KeyName" {
			after.KeyName = testSweepKeyName // the platform holds its key
		}
		if sweepMemberGuard(before) != nil {
			t.Fatalf("LaunchConfig.%s: the gate's base config is itself refused", name)
		}
		refused := sweepMemberGuard(after) != nil
		if d.refused != refused {
			t.Errorf("LaunchConfig.%s: disposition says refused=%t, but sweepMemberGuard refuses it: %t", name, d.refused, refused)
		}
		if refused {
			continue // a refused member renders nothing
		}
		settingsChange := !reflect.DeepEqual(aws.NewLaunchSettings(before, "", 0), aws.NewLaunchSettings(after, "", 0))
		tagsChange := !reflect.DeepEqual(gateTags(t, before), gateTags(t, after))
		userDataChange := name != "UserData" && render(before) != render(after)
		if d.settings != settingsChange {
			t.Errorf("LaunchConfig.%s: disposition says settings=%t, but setting it changes aws.LaunchSettings: %t", name, d.settings, settingsChange)
		}
		if d.tags != tagsChange {
			t.Errorf("LaunchConfig.%s: disposition says tags=%t, but setting it changes the member's tags: %t", name, d.tags, tagsChange)
		}
		if d.userData != userDataChange {
			t.Errorf("LaunchConfig.%s: disposition says userData=%t, but setting it changes the member's user data: %t", name, d.userData, userDataChange)
		}
	}
	for name := range foregroundDispositions {
		if !seen[name] {
			t.Errorf("foregroundDispositions names %s, which LaunchConfig no longer has", name)
		}
	}
}

// decodeUserDataOrPlain is a member's user-data script: gzip+base64 on Linux,
// plain base64 on Windows.
func decodeUserDataOrPlain(t *testing.T, cfg aws.LaunchConfig) []byte {
	t.Helper()
	if cfg.TargetOS == "windows" {
		return []byte(cfg.UserData)
	}
	return decodeUserDataOnce(t, cfg.UserData)
}
