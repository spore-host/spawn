package cmd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// testMemberSettings is what aws.Client.ResolveLaunchSettings renders for cfg
// when its AMI and memory lookups find nothing (no AWS in tests).
func testMemberSettings(cfg aws.LaunchConfig) aws.LaunchSettings {
	return aws.NewLaunchSettings(cfg, "", 0)
}

// How a detached sweep member gets each LaunchConfig field, or why it does not.
type detachedDisposition struct {
	// settings: in aws.LaunchSettings, applied by the orchestrator to RunInstances.
	settings bool
	// tags: in the member's rendered instance tags (detachedTagsKey).
	tags bool
	// refused: detachedSettingsGuard refuses it at --detach (with prereq set).
	refused bool
	// why explains a field carried another way (its own param key, the user
	// data) or not at all. Required when none of the above is claimed.
	why string
	// prereq sets what the field needs to take effect (EFAInterfaces needs
	// EFAEnabled), before the sample is applied.
	prereq func(*aws.LaunchConfig)
}

const (
	whyParam    = "its own param key, read by the orchestrator (detachedParamKeys)"
	whyUserData = "the member's user data (detachedUserDataKey), rendered by the foreground's builder"
	whyNotSweep = "never set on a sweep member's config: only the single-instance or job-array launch path sets it"
)

// detachedDispositions is every LaunchConfig field's disposition on a detached
// sweep. TestDetachedSweepCarriesOrRefusesEveryLaunchSetting fails on a field
// missing here, and checks each claim against the code.
var detachedDispositions = map[string]detachedDisposition{
	"InstanceType":       {why: whyParam + " (instance_type, which may be a fallback pattern)"},
	"Region":             {why: "the sweep record's region (region groups), and the region param for the estimator"},
	"AMI":                {why: whyParam + " (ami)"},
	"KeyName":            {why: whyParam + " (key_name)"},
	"IamInstanceProfile": {why: whyParam + " (iam_role)"},
	"UserData":           {why: whyParam + " (detachedUserDataKey: the built bootstrap)"},
	"CustomUserData":     {why: whyUserData},
	"ClientToken":        {why: "no sweep path sets an idempotency token: cohort/taskcohort adapters only"},

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
	"AttachVolumes":        {settings: true},
	"EBSEncrypted":         {settings: true},
	"IMDSv2Enforced":       {settings: true},
	"IMDSv2HopLimit":       {settings: true},
	"HibernateOnIdle":      {settings: true, tags: true},

	"VPCID":           {refused: true},
	"EFAInterfaces":   {refused: true, prereq: func(c *aws.LaunchConfig) { c.EFAEnabled = true }},
	"EBSKMSKeyID":     {refused: true, settings: true},
	"FSxLustreCreate": {refused: true},
	"FSxLustreRecall": {refused: true},

	"PlacementGroupPrefix": {why: whyNotSweep + " (an MPI job array's per-AZ groups)"},
	"DNSFunctionARN":       {why: whyNotSweep + " (the custom-DNS preflight, which a sweep does not run on either path)"},
	"FSxPending":           {tags: true},
	"FSxImportPath":        {why: "tagged only beside FSxPending, which only --fsx-create sets, and that is refused"},
	"FSxExportPath":        {why: "tagged only beside FSxPending, which only --fsx-create sets, and that is refused"},
	"FSxLifecycle":         {why: "valid only with --fsx-create, which is refused"},
	"FSxTTL":               {why: "valid only with --fsx-create, which is refused"},
	"FSxStorageCapacity":   {why: "read only by --fsx-create, which is refused"},
	"FSxS3Bucket":          {why: "read only by --fsx-create, which is refused"},

	"TTL": {tags: true}, "IdleTimeout": {tags: true}, "TerminateOnIdle": {tags: true}, "CostLimit": {tags: true},
	"DNSName": {tags: true}, "IdleCPUPercent": {tags: true}, "Observability": {tags: true},
	"DNSDomain":   {tags: true, prereq: func(c *aws.LaunchConfig) { c.DNSName = "box" }},
	"DNSEndpoint": {tags: true, prereq: func(c *aws.LaunchConfig) { c.DNSName = "box" }},
	"PreStop":     {tags: true}, "PreStopTimeout": {tags: true, prereq: func(c *aws.LaunchConfig) { c.PreStop = "sync" }},
	"SpotInterruptionWebhookURL": {tags: true},
	"WebhookCorrelation":         {tags: true, prereq: func(c *aws.LaunchConfig) { c.SpotInterruptionWebhookURL = "https://h.example.org/" }},
	"WebhookTimeout":             {tags: true, prereq: func(c *aws.LaunchConfig) { c.SpotInterruptionWebhookURL = "https://h.example.org/" }},
	"CompletionWebhookURL":       {tags: true}, "OnComplete": {tags: true},
	"CompletionFile":  {tags: true, prereq: func(c *aws.LaunchConfig) { c.OnComplete = "terminate" }},
	"CompletionDelay": {tags: true}, "SessionTimeout": {tags: true}, "Username": {tags: true},
	"JobArrayID":    {tags: true},
	"JobArrayName":  {tags: true, prereq: func(c *aws.LaunchConfig) { c.JobArrayID = "ja" }},
	"JobArraySize":  {tags: true, prereq: func(c *aws.LaunchConfig) { c.JobArrayID = "ja" }},
	"JobArrayIndex": {tags: true, prereq: func(c *aws.LaunchConfig) { c.JobArrayID = "ja" }}, "JobArrayCommand": {tags: true}, "SweepID": {tags: true},
	"SweepName": {tags: true}, "SweepIndex": {tags: true}, "SweepSize": {tags: true}, "Parameters": {tags: true},
	"EFSID":            {tags: true},
	"EFSMountPoint":    {tags: true, prereq: func(c *aws.LaunchConfig) { c.EFSID = "fs-1" }},
	"FSxLustreID":      {tags: true},
	"FSxMountName":     {tags: true, prereq: func(c *aws.LaunchConfig) { c.FSxLustreID = "fs-2" }},
	"FSxMountPoint":    {tags: true, prereq: func(c *aws.LaunchConfig) { c.FSxLustreID = "fs-2" }},
	"SlackWorkspaceID": {tags: true}, "NotifyURL": {tags: true}, "NotifyCommand": {tags: true},
	"NotifyPlatform": {tags: true}, "ActivePortsRaw": {tags: true}, "ActiveProcessesRaw": {tags: true},
	"DCVSessionID": {tags: true}, "AppName": {tags: true}, "AppMode": {tags: true}, "ReadyPort": {tags: true},
	"ReadyHealthPath": {tags: true}, "WebAuth": {tags: true}, "PricePerHour": {tags: true},
	"PriceSource": {tags: true, prereq: func(c *aws.LaunchConfig) { c.PricePerHour = 0.1 }}, "Name": {tags: true}, "Tags": {tags: true}, "SpawnVersion": {tags: true},
	"TargetOS": {tags: true},
}

// sampleValues are samples for fields whose value is validated before it is
// tagged; every other field gets a generic non-zero value of its kind.
var sampleValues = map[string]interface{}{
	"TTL": "2h", "IdleTimeout": "30m", "PreStopTimeout": "5m", "CompletionDelay": "30s",
	"SessionTimeout": "10m", "WebhookTimeout": "2s", "FSxTTL": "7d", "OnComplete": "stop",
	"DNSDomain": "example.org", "DNSEndpoint": "https://abc.lambda-url.us-east-1.on.aws/",
	"SpotInterruptionWebhookURL": "https://hooks.example.org/spot", "CompletionWebhookURL": "https://hooks.example.org/done",
	"NotifyURL": "https://notify.example.org/", "NotifyPlatform": "teams", "AppMode": "web", "WebAuth": "none",
	"TargetOS": "windows", "IdleCPUPercent": 12.5,
}

func sampleField(f reflect.Value, name string) bool {
	if v, ok := sampleValues[name]; ok {
		f.Set(reflect.ValueOf(v).Convert(f.Type()))
		return true
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString("sample-" + strings.ToLower(name))
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		f.SetInt(3)
	case reflect.Float64:
		f.SetFloat(1.5)
	case reflect.Slice:
		switch f.Type().Elem() {
		case reflect.TypeOf(""):
			f.Set(reflect.ValueOf([]string{"sample-" + strings.ToLower(name)}))
		case reflect.TypeOf(aws.AttachVolumeSpec{}):
			f.Set(reflect.ValueOf([]aws.AttachVolumeSpec{{SnapshotID: "snap-sample", MountPoint: "/data"}}))
		default:
			return false
		}
	case reflect.Map:
		f.Set(reflect.ValueOf(map[string]string{"sample": "value"}))
	case reflect.Struct:
		for i := 0; i < f.NumField(); i++ {
			if f.Field(i).Kind() == reflect.Bool {
				f.Field(i).SetBool(true)
				return true
			}
		}
		return false
	default:
		return false
	}
	return true
}

func gateTags(t *testing.T, cfg aws.LaunchConfig) map[string]string {
	t.Helper()
	tags, err := aws.BuildLaunchTags(cfg, "111122223333", "arn:aws:iam::111122223333:user/u", "")
	if err != nil {
		t.Fatalf("BuildLaunchTags: %v", err)
	}
	return tagsAsMap(tags)
}

// TestDetachedSweepCarriesOrRefusesEveryLaunchSetting is #1353's discovery
// gate. Every LaunchConfig field is found by reflection, so a field added later
// fails here until it is carried to a detached member or refused at --detach;
// it cannot be silently dropped, which is what happened to volume_size,
// security groups, subnet, AZ, Spot max price, hibernation, EFA and placement.
//
// Each claim is checked against the code, not trusted: a field said to be in the
// settings must change aws.LaunchSettings, one said to be tagged must change the
// tags a foreground launch writes, one said to be refused must be refused, and
// one said to reach the member some other way, or not at all, must change none
// of those, so a field that starts to matter has to be reclassified.
func TestDetachedSweepCarriesOrRefusesEveryLaunchSetting(t *testing.T) {
	base := aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-1", SweepID: "sw", SweepName: "sw"}
	typ := reflect.TypeOf(aws.LaunchConfig{})
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		seen[name] = true
		d, ok := detachedDispositions[name]
		if !ok {
			t.Errorf("LaunchConfig.%s has no detached-sweep disposition. A detached member must either carry it "+
				"(aws.LaunchSettings for a RunInstances setting, the tags or user data otherwise) or "+
				"detachedSettingsGuard must refuse it at --detach; then record which in detachedDispositions (#1353)", name)
			continue
		}
		claimed := d.settings || d.tags || d.refused
		if !claimed && d.why == "" {
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

		settingsChange := !reflect.DeepEqual(aws.NewLaunchSettings(before, "", 0), aws.NewLaunchSettings(after, "", 0))
		tagsChange := !reflect.DeepEqual(gateTags(t, before), gateTags(t, after))
		refusedBefore := detachedSettingsGuard(before) != nil
		refused := detachedSettingsGuard(after) != nil
		if refusedBefore {
			t.Fatalf("LaunchConfig.%s: the gate's base config is itself refused", name)
		}

		if d.settings != settingsChange {
			t.Errorf("LaunchConfig.%s: disposition says settings=%t, but setting it changes aws.LaunchSettings: %t",
				name, d.settings, settingsChange)
		}
		if d.tags != tagsChange {
			t.Errorf("LaunchConfig.%s: disposition says tags=%t, but setting it changes the member's tags: %t",
				name, d.tags, tagsChange)
		}
		if d.refused != refused {
			t.Errorf("LaunchConfig.%s: disposition says refused=%t, but detachedSettingsGuard refuses it: %t",
				name, d.refused, refused)
		}
	}
	for name := range detachedDispositions {
		if !seen[name] {
			t.Errorf("detachedDispositions names %s, which LaunchConfig no longer has", name)
		}
	}
}

// shippedSettings decodes the settings member i's row carries, through JSON as
// S3 delivers them and the decoder the orchestrator uses.
func shippedSettings(t *testing.T, pf *ParamFileFormat, i int) aws.LaunchSettings {
	t.Helper()
	v, ok := pf.Params[i][detachedLaunchSettingsKey]
	if !ok {
		t.Fatalf("member %d carries no %s: the orchestrator refuses it", i, detachedLaunchSettingsKey)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	s, err := aws.DecodeLaunchSettings(raw)
	if err != nil {
		t.Fatalf("member %d: the orchestrator cannot decode its settings: %v", i, err)
	}
	return s
}

// TestDetachedMemberSettingsAreTheForegroundMembers is #1353's CLI half: the
// settings each member's row ships are those of the config the foreground path
// launches for the same member, CLI flags and row keys alike. (The pkg/aws gate
// proves applying them composes Client.Launch's RunInstances for that config;
// the Lambda's, that buildRunInstancesInput applies them.)
func TestDetachedMemberSettingsAreTheForegroundMembers(t *testing.T) {
	base := aws.LaunchConfig{
		InstanceType: "m7i.large", Region: "us-east-1", SubnetID: "subnet-1", SecurityGroupIDs: []string{"sg-1"},
		PlacementGroup: "pg-1", EFAEnabled: true, NestedVirtualization: true, RootVolumeSizeGiB: 40,
		ReservationID: "cr-1", AttachVolumes: []aws.AttachVolumeSpec{{SnapshotID: "snap-1", MountPoint: "/db"}},
		TTL: "2h",
	}
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "m7i.large", "ami": "ami-1", "region": "us-east-1"},
		Params: []map[string]interface{}{
			{"trial": "a"},
			{"trial": "b", "volume_size": 300, "az": "us-east-1c", "spot": true, "spot_max_price": "0.30"},
			{"trial": "c", "hibernate": true, "volume_size": 0},
		},
	}
	foreground := copyParamFile(pf)
	if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
		t.Fatalf("seedDetachedBootstrap: %v", err)
	}
	for i := range pf.Params {
		cfg, err := buildLaunchConfigFromParams(base, foreground.Defaults, foreground.Params[i], "sw-1", "sw", i, len(pf.Params))
		if err != nil {
			t.Fatal(err)
		}
		defaultSweepMember(&cfg, base, "sw", i)
		if got, want := shippedSettings(t, pf, i), testMemberSettings(cfg); !reflect.DeepEqual(got, want) {
			t.Errorf("member %d ships settings\n %+v\nbut the foreground member's are\n %+v", i, got, want)
		}
	}
	// And they are the member's own, not the CLI defaults alone.
	if s := shippedSettings(t, pf, 1); s.RootVolumeGiB != 300 || s.AvailabilityZone != "us-east-1c" ||
		!s.Spot || s.SpotMaxPrice != "0.30" || s.SubnetID != "subnet-1" || s.PlacementGroup != "pg-1" {
		t.Errorf("member 1's settings %+v lost its row's volume_size/az/spot/spot_max_price or the CLI's subnet/placement", s)
	}
	if s := shippedSettings(t, pf, 2); !s.Hibernate || s.RootVolumeGiB == 40 {
		t.Errorf("member 2's settings %+v lost its hibernation, or its RAM-sized root", s)
	}
	// A user's value under the key never survives: it is spawn's.
	file := &ParamFileFormat{Params: []map[string]interface{}{{detachedLaunchSettingsKey: map[string]interface{}{"schema": 1}}}}
	if err := validateSweepParamKeys(file); err == nil {
		t.Errorf("a param file may set %s", detachedLaunchSettingsKey)
	}
}

// TestDetachedSweepRefusesWhatTheOrchestratorCannotLaunch: a setting the
// orchestrator cannot honour fails the detached launch before anything is
// uploaded, naming the setting and --no-detach, rather than launching members
// without it.
func TestDetachedSweepRefusesWhatTheOrchestratorCannotLaunch(t *testing.T) {
	cases := map[string]struct {
		base aws.LaunchConfig
		row  map[string]interface{}
		want string
	}{
		"fsx-create":          {base: aws.LaunchConfig{FSxLustreCreate: true, FSxLifecycle: "ephemeral", FSxS3Bucket: "b"}, want: "--fsx-create"},
		"fsx-recall":          {base: aws.LaunchConfig{FSxLustreRecall: "stack"}, want: "--fsx-recall"},
		"multi-card EFA":      {base: aws.LaunchConfig{EFAEnabled: true, EFAInterfaces: 4}, want: "--efa-interfaces 4"},
		"customer KMS key":    {base: aws.LaunchConfig{EBSKMSKeyID: "arn:aws:kms:us-east-1:1:key/k"}, want: "KMS key"},
		"vpc without subnet":  {base: aws.LaunchConfig{VPCID: "vpc-1"}, want: "--vpc without --subnet"},
		"hibernation pattern": {row: map[string]interface{}{"instance_type": "m7i.large|m7i.xlarge", "hibernate": true}, want: "instance type pattern"},
	}
	for name, tc := range cases {
		row := map[string]interface{}{"trial": "a"}
		for k, v := range tc.row {
			row[k] = v
		}
		pf := &ParamFileFormat{
			Defaults: map[string]interface{}{"instance_type": "m7i.large", "ami": "ami-1", "region": "us-east-1", "ttl": "1h"},
			Params:   []map[string]interface{}{row},
		}
		err := seedDetachedBootstrap(pf, tc.base, "sw-1", "sw", testDetachedBootstrap(t))
		if err == nil {
			t.Errorf("%s: a detached sweep was seeded; its members would launch without the setting", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--no-detach") {
			t.Errorf("%s: error %q does not name %q and --no-detach", name, err, tc.want)
		}
		if _, shipped := pf.Params[0][detachedLaunchSettingsKey]; shipped {
			t.Errorf("%s: settings were seeded for a refused member", name)
		}
	}
}
