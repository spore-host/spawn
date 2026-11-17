package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	rgt "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/libs/footprint"
	libtags "github.com/spore-host/libs/tags"

	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// `spawn footprint -o json` (spawn#1653): the footprint in the shared
// libs/footprint schema, with every resource's ownership, class, deletable
// verdict and dependencies, so that `spawn teardown --plan` can be driven from
// it. The rules are pkg/footprintspawn's; this file gathers the facts they
// need.

// FootprintInput is what FootprintReport builds on: the account, the regions
// scanned, and the rows `spawn footprint` already listed (the spawn:managed
// tag scan, the control plane, the global IAM and S3 rows).
type FootprintInput struct {
	Account      string
	Regions      []string
	Tagged       []ManagedResource
	ControlPlane []ControlPlaneResource
	Global       []ControlPlaneResource
	// Notes are the scans' own gaps; they become the report's warnings.
	Notes []string
	Now   time.Time
}

// FootprintReport turns in into the libs/footprint report, adding what the
// table never listed: resources tagged spore-host:managed-by (any tool), the
// managed policies under /spore-host/, CloudFormation stacks, spawn-pipeline-*
// SNS topics, and calque's queues, NFS security group and AMIs.
//
// Read-only and tolerant like the rest of the footprint: a call that fails
// becomes a warning, and the facts it would have given are taken in the safe
// direction (a resource whose use could not be checked is active, a bucket
// whose emptiness could not be checked holds user data).
func (c *Client) FootprintReport(ctx context.Context, in FootprintInput) *footprint.Report {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	b := &fpBuilder{c: c, account: in.Account, byARN: map[string]int{}, facts: map[string]*fs.Facts{}}
	b.warn(in.Notes...)

	for _, r := range in.Tagged {
		b.addManaged(r)
	}
	for _, region := range in.Regions {
		b.scanSporeTagged(ctx, region)
	}
	for _, r := range in.ControlPlane {
		b.addControlPlane(r)
	}
	for _, r := range in.Global {
		b.addControlPlane(r)
	}
	b.scanPolicies(ctx)
	for _, region := range in.Regions {
		b.scanStacks(ctx, region)
		b.scanTopics(ctx, region)
		b.scanCalque(ctx, region)
	}

	b.enrichEC2(ctx, in.Regions)
	b.enrichIAM(ctx)
	b.enrichBuckets(ctx)
	b.enrichControlPlaneTags(ctx)
	b.markInUse(ctx, in.Regions)

	rep := footprint.NewReport(fs.Tool, in.Account, now)
	for i := range b.res {
		r := &b.res[i]
		if r.Tags == nil {
			r.Tags = map[string]string{}
		}
		r.Scope = r.Tags[libtags.Scope]
		f := b.facts[r.ARN]
		f.Account = in.Account
		fs.Finish(r, *f)
	}
	sort.SliceStable(b.res, func(i, j int) bool {
		if b.res[i].Type != b.res[j].Type {
			return b.res[i].Type < b.res[j].Type
		}
		return b.res[i].ARN < b.res[j].ARN
	})
	rep.Resources = b.res
	rep.Warnings = b.warnings
	return rep
}

// GatherFootprint runs the scans FootprintReport builds on, for regions: the
// spawn:managed tag scan and the control plane in each region, and the
// account-wide IAM and S3 rows. A scan that fails becomes a note, as in
// `spawn footprint`, which calls this.
func (c *Client) GatherFootprint(ctx context.Context, regions []string) FootprintInput {
	in := FootprintInput{Regions: regions}
	for _, r := range regions {
		tagged, _, err := c.DiscoverManagedResources(ctx, DiscoverOptions{Region: r})
		if err != nil {
			in.Notes = append(in.Notes, fmt.Sprintf("%s: tagged scan: %v", r, err))
		} else {
			in.Tagged = append(in.Tagged, tagged...)
		}
		cp, notes := c.ScanControlPlane(ctx, r)
		in.ControlPlane = append(in.ControlPlane, cp...)
		for _, n := range notes {
			in.Notes = append(in.Notes, fmt.Sprintf("%s: %s", r, n))
		}
	}
	g, gnotes := c.ScanGlobalFootprint(ctx)
	in.Global = g
	in.Notes = append(in.Notes, gnotes...)
	return in
}

// AccountFootprint is `spawn footprint -o json` for regions of account: the
// same scans and the same rules, as the libs/footprint report. The reaper's
// account-idle sweep (spawn#1654) plans with it, so the plan it reports is the
// one a human would hand `spawn teardown --plan`.
func (c *Client) AccountFootprint(ctx context.Context, account string, regions []string, now time.Time) *footprint.Report {
	in := c.GatherFootprint(ctx, regions)
	in.Account, in.Now = account, now
	return c.FootprintReport(ctx, in)
}

type fpBuilder struct {
	c        *Client
	account  string
	res      []footprint.Resource
	byARN    map[string]int
	facts    map[string]*fs.Facts
	warnings []string
	// What the IAM and bucket passes need beyond the resource itself.
	profileRoles map[string][]string // profile ARN -> role ARNs
	bucketRegion map[string]string
}

func (b *fpBuilder) warn(ws ...string) { b.warnings = append(b.warnings, ws...) }

// add records r unless its ARN is already in, in which case only the tags
// the first sighting lacked are taken. It returns the resource's Facts.
func (b *fpBuilder) add(r footprint.Resource) *fs.Facts {
	if r.ARN == "" {
		return &fs.Facts{}
	}
	if i, ok := b.byARN[r.ARN]; ok {
		cur := &b.res[i]
		for k, v := range r.Tags {
			if cur.Tags == nil {
				cur.Tags = map[string]string{}
			}
			if _, has := cur.Tags[k]; !has {
				cur.Tags[k] = v
			}
		}
		if cur.Name == "" || strings.HasPrefix(cur.Name, "sg-") || strings.HasPrefix(cur.Name, "key-") {
			if r.Name != "" {
				cur.Name = r.Name
			}
		}
		return b.facts[r.ARN]
	}
	b.byARN[r.ARN] = len(b.res)
	b.res = append(b.res, r)
	f := &fs.Facts{}
	b.facts[r.ARN] = f
	return f
}

// identify is a resource's footprint type and name from its ARN.
func identify(arn string) (typ, name string) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return "", ""
	}
	svc, tail := parts[2], parts[5]
	last := func(s string) string {
		if i := strings.LastIndex(s, "/"); i >= 0 {
			return s[i+1:]
		}
		return s
	}
	kind, rest, _ := strings.Cut(tail, "/")
	switch svc {
	case "s3":
		return fs.TypeBucket, tail
	case "sqs":
		return fs.TypeQueue, tail
	case "sns":
		return fs.TypeTopic, tail
	case "logs":
		return fs.TypeLogGroup, strings.TrimSuffix(strings.TrimPrefix(tail, "log-group:"), ":*")
	case "lambda":
		n := strings.TrimPrefix(tail, "function:")
		n, _, _ = strings.Cut(n, ":")
		return fs.TypeFunction, n
	case "cloudformation":
		// stack/NAME/UUID
		n, _, _ := strings.Cut(rest, "/")
		return fs.TypeStack, n
	}
	t := map[string]string{
		"ec2/instance": fs.TypeInstance, "ec2/volume": fs.TypeVolume,
		"ec2/security-group": fs.TypeSecurityGroup, "ec2/key-pair": fs.TypeKeyPair,
		"ec2/placement-group": fs.TypePlacementGroup, "ec2/elastic-ip": fs.TypeEIP,
		"ec2/image": fs.TypeImage, "ec2/snapshot": fs.TypeSnapshot,
		"ec2/spot-instances-request": fs.TypeSpotRequest,
		"iam/role":                   fs.TypeRole, "iam/instance-profile": fs.TypeInstanceProfile,
		"iam/policy": fs.TypeManagedPolicy, "dynamodb/table": fs.TypeTable,
		"events/rule": fs.TypeRule, "events/event-bus": fs.TypeEventBus,
		"scheduler/schedule": fs.TypeSchedule, "fsx/file-system": fs.TypeFileSystem,
	}[svc+"/"+kind]
	if t == "" {
		t = "AWS::" + strings.ToUpper(svc) + "::" + kind
	}
	return t, last(rest)
}

// regionOf is a footprint Region: "global" for IAM and an empty region.
func regionOf(typ, region string) string {
	if strings.HasPrefix(typ, "AWS::IAM::") || region == "" {
		return fs.RegionGlobal
	}
	return region
}

// addManaged takes one row of the spawn:managed tag scan.
func (b *fpBuilder) addManaged(m ManagedResource) {
	arn := m.ARN
	if arn == "" && m.ResourceType == "address" {
		arn = fmt.Sprintf("arn:aws:ec2:%s:%s:elastic-ip/%s", m.Region, b.account, m.ID)
	}
	typ, name := identify(arn)
	if typ == "" {
		return
	}
	f := b.add(footprint.Resource{ARN: arn, Type: typ, Region: regionOf(typ, m.Region), Name: name, Tags: cloneTags(m.Tags)})
	switch typ {
	case fs.TypeInstance:
		switch m.State {
		case "pending", "running", "shutting-down", "stopping", "stopped":
			f.InUse = "instance is " + m.State
		}
	case fs.TypeVolume:
		if m.State != "" && m.State != "available" && m.State != "deleted" {
			f.InUse = "volume is " + m.State
		}
	case fs.TypePlacementGroup:
		if m.State != "empty" {
			f.InUse = "an instance is in the group"
		}
	case fs.TypeEIP:
		switch {
		case !IsSpawnAllocatedAddress(m):
			f.UserData = "an Elastic IP spawn did not allocate is the user's (#262)"
		case m.State != "unassociated":
			f.InUse = "associated (" + m.State + ")"
		}
	}
}

// scanSporeTagged adds what carries spore-host:managed-by (any tool) in
// region, which the spawn:managed scan does not see when only the new tag is
// on it, and which is how another tool's resources are listed as foreign.
func (b *fpBuilder) scanSporeTagged(ctx context.Context, region string) {
	p := rgt.NewGetResourcesPaginator(rgt.NewFromConfig(b.c.regionalConfig(region)), &rgt.GetResourcesInput{
		TagFilters: []rgttypes.TagFilter{{Key: aws.String(libtags.ManagedBy)}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			b.warn(fmt.Sprintf("%s: %s tag scan: %v", region, libtags.ManagedBy, err))
			return
		}
		for _, m := range page.ResourceTagMappingList {
			arn := aws.ToString(m.ResourceARN)
			typ, name := identify(arn)
			if typ == "" || typ == fs.TypePlacementGroup {
				// Placement groups come keyed by id; the dedicated scan
				// (scanPlacementGroups) lists them by name (#713).
				continue
			}
			tags := map[string]string{}
			for _, t := range m.Tags {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			b.add(footprint.Resource{ARN: arn, Type: typ, Region: regionOf(typ, region), Name: name, Tags: tags})
		}
	}
}

// addControlPlane takes one row of the name-matched control-plane or global
// scan.
func (b *fpBuilder) addControlPlane(r ControlPlaneResource) {
	arn := r.ARN
	typ := map[string]string{
		"lambda/function": fs.TypeFunction, "logs/log-group": fs.TypeLogGroup,
		"events/event-bus": fs.TypeEventBus, "events/rule": fs.TypeRule,
		"scheduler/schedule": fs.TypeSchedule, "dynamodb/table": fs.TypeTable,
		"iam/role": fs.TypeRole, "iam/instance-profile": fs.TypeInstanceProfile,
		"s3/bucket": fs.TypeBucket,
	}[r.Service+"/"+r.Kind]
	if typ == "" {
		return
	}
	if arn == "" && typ == fs.TypeTable {
		arn = fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", r.Region, b.account, r.Name)
	}
	region := r.Region
	if typ == fs.TypeBucket {
		region = r.bucketRegion
	}
	f := b.add(footprint.Resource{ARN: arn, Type: typ, Region: regionOf(typ, region), Name: r.Name})
	switch typ {
	case fs.TypeInstanceProfile:
		if b.profileRoles == nil {
			b.profileRoles = map[string][]string{}
		}
		b.profileRoles[arn] = r.profileRoleARNs
	case fs.TypeBucket:
		if b.bucketRegion == nil {
			b.bucketRegion = map[string]string{}
		}
		b.bucketRegion[arn] = r.bucketRegion
	case fs.TypeSchedule:
		if r.scheduleState == "ENABLED" {
			f.InUse = "an enabled schedule"
		}
	case fs.TypeFunction, fs.TypeEventBus, fs.TypeRule, fs.TypeTable:
		f.Residue = "spore.host control-plane machinery"
	case fs.TypeLogGroup:
		if !strings.HasPrefix(r.Warn, "orphaned") {
			f.Residue = "spore.host control-plane machinery"
		}
	}
}

// scanPolicies adds the customer-managed policies under /spore-host/: the
// spored managed policies and the spawn-instance boundary.
func (b *fpBuilder) scanPolicies(ctx context.Context) {
	ic := iam.NewFromConfig(b.c.regionalConfig(""))
	p := iam.NewListPoliciesPaginator(ic, &iam.ListPoliciesInput{
		Scope: iamtypes.PolicyScopeTypeLocal, PathPrefix: aws.String(instancerole.SporedPolicyPath),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			b.warn(fmt.Sprintf("iam policies under %s: %v (listing them needs iam:ListPolicies)", instancerole.SporedPolicyPath, err))
			return
		}
		for _, pol := range page.Policies {
			arn := aws.ToString(pol.Arn)
			tags, err := listPolicyTags(ctx, ic, arn)
			if err != nil {
				b.warn(fmt.Sprintf("iam policy %s: tags: %v (reading them needs iam:ListPolicyTags)", arn, err))
			}
			f := b.add(footprint.Resource{ARN: arn, Type: fs.TypeManagedPolicy, Region: fs.RegionGlobal,
				Name: aws.ToString(pol.PolicyName), Tags: tags})
			if n := aws.ToInt32(pol.AttachmentCount) + aws.ToInt32(pol.PermissionsBoundaryUsageCount); n > 0 {
				f.InUse = fmt.Sprintf("attached to, or the boundary of, %d role(s)", n)
			}
		}
	}
}

func listPolicyTags(ctx context.Context, ic *iam.Client, arn string) (map[string]string, error) {
	tags := map[string]string{}
	p := iam.NewListPolicyTagsPaginator(ic, &iam.ListPolicyTagsInput{PolicyArn: aws.String(arn)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return tags, err
		}
		for _, t := range page.Tags {
			tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	return tags, nil
}

// failedStackStatus are the stack states that leave nothing running: a
// failed create rolled back, or a delete that did not finish.
var failedStackStatus = map[string]bool{
	"ROLLBACK_COMPLETE": true, "ROLLBACK_FAILED": true, "DELETE_FAILED": true, "CREATE_FAILED": true,
}

// scanStacks adds the CloudFormation stacks named like spore.host's (the
// bot, the cross-account reaper role, the member events, the scheduler
// role, calque's and lagotto's).
func (b *fpBuilder) scanStacks(ctx context.Context, region string) {
	p := cloudformation.NewDescribeStacksPaginator(cloudformation.NewFromConfig(b.c.regionalConfig(region)), &cloudformation.DescribeStacksInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			b.warn(fmt.Sprintf("%s: cloudformation stacks: %v (listing them needs cloudformation:DescribeStacks)", region, err))
			return
		}
		for _, st := range page.Stacks {
			name := aws.ToString(st.StackName)
			if !matchesFootprintName(name) {
				continue
			}
			tags := map[string]string{}
			for _, t := range st.Tags {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			f := b.add(footprint.Resource{ARN: aws.ToString(st.StackId), Type: fs.TypeStack, Region: region, Name: name, Tags: tags})
			status := string(st.StackStatus)
			switch {
			case strings.HasSuffix(status, "_IN_PROGRESS"):
				f.InUse = "stack is " + status
			case failedStackStatus[status]:
			default:
				f.Residue = "a deployed spore.host stack (" + status + ")"
			}
		}
	}
}

// PipelineTopicPrefix starts the SNS topics the pipeline orchestrator
// creates.
const PipelineTopicPrefix = "spawn-pipeline-"

// scanTopics adds the spawn-pipeline-* SNS topics.
func (b *fpBuilder) scanTopics(ctx context.Context, region string) {
	sc := sns.NewFromConfig(b.c.regionalConfig(region))
	p := sns.NewListTopicsPaginator(sc, &sns.ListTopicsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			b.warn(fmt.Sprintf("%s: sns topics: %v (listing them needs sns:ListTopics)", region, err))
			return
		}
		for _, t := range page.Topics {
			arn := aws.ToString(t.TopicArn)
			_, name := identify(arn)
			if !strings.HasPrefix(name, PipelineTopicPrefix) {
				continue
			}
			tags := map[string]string{}
			if out, err := sc.ListTagsForResource(ctx, &sns.ListTagsForResourceInput{ResourceArn: aws.String(arn)}); err != nil {
				b.warn(fmt.Sprintf("%s: sns topic %s: tags: %v (reading them needs sns:ListTagsForResource)", region, name, err))
			} else {
				for _, tg := range out.Tags {
					tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
				}
			}
			b.add(footprint.Resource{ARN: arn, Type: fs.TypeTopic, Region: region, Name: name, Tags: tags})
		}
	}
}

// CalqueNamePrefix and CalqueNFSGroup are calque's names (calque#227): spawn
// lists them so the account's whole spore.host footprint is in one report,
// as foreign, so spawn never deletes them.
const (
	CalqueNamePrefix = "calque-"
	CalqueNFSGroup   = "calque-nfs-sg"
)

// scanCalque adds calque's queues, its NFS security group and its AMIs in
// region. calque's roles and instance profiles come from the global scan,
// whose name prefixes include calque.
func (b *fpBuilder) scanCalque(ctx context.Context, region string) {
	cfg := b.c.regionalConfig(region)
	qc := sqs.NewFromConfig(cfg)
	qp := sqs.NewListQueuesPaginator(qc, &sqs.ListQueuesInput{QueueNamePrefix: aws.String(CalqueNamePrefix)})
	for qp.HasMorePages() {
		page, err := qp.NextPage(ctx)
		if err != nil {
			b.warn(fmt.Sprintf("%s: calque queues: %v (listing them needs sqs:ListQueues)", region, err))
			break
		}
		for _, url := range page.QueueUrls {
			name := url[strings.LastIndex(url, "/")+1:]
			tags := map[string]string{}
			if out, err := qc.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(url)}); err == nil {
				tags = out.Tags
			}
			b.add(footprint.Resource{ARN: fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, b.account, name),
				Type: fs.TypeQueue, Region: region, Name: name, Tags: tags})
		}
	}

	ec := ec2.NewFromConfig(cfg)
	sgs, err := ec.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-name"), Values: []string{CalqueNFSGroup}}},
	})
	if err != nil {
		b.warn(fmt.Sprintf("%s: calque security group: %v", region, err))
	} else {
		for _, g := range sgs.SecurityGroups {
			b.add(footprint.Resource{ARN: fmt.Sprintf("arn:aws:ec2:%s:%s:security-group/%s", region, b.account, aws.ToString(g.GroupId)),
				Type: fs.TypeSecurityGroup, Region: region, Name: aws.ToString(g.GroupName), Tags: ec2TagMap(g.Tags)})
		}
	}

	imgs, err := ec.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners:  []string{"self"},
		Filters: []ec2types.Filter{{Name: aws.String("name"), Values: []string{CalqueNamePrefix + "*"}}},
	})
	if err != nil {
		b.warn(fmt.Sprintf("%s: calque AMIs: %v", region, err))
		return
	}
	for _, im := range imgs.Images {
		b.add(footprint.Resource{ARN: fmt.Sprintf("arn:aws:ec2:%s::image/%s", region, aws.ToString(im.ImageId)),
			Type: fs.TypeImage, Region: region, Name: aws.ToString(im.Name), Tags: ec2TagMap(im.Tags)})
	}
}

func ec2TagMap(ts []ec2types.Tag) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func cloneTags(t map[string]string) map[string]string {
	m := make(map[string]string, len(t))
	for k, v := range t {
		m[k] = v
	}
	return m
}

// ofType returns the indexes of the resources of typ in region ("" for any).
func (b *fpBuilder) ofType(typ, region string) []int {
	var out []int
	for i, r := range b.res {
		if r.Type == typ && (region == "" || r.Region == region) {
			out = append(out, i)
		}
	}
	return out
}

func idOf(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// liveInstanceStates are the states in which an instance holds what it uses.
var liveInstanceStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

// enrichEC2 reads, per region, what the tag scans do not carry: security
// group and key pair names, which security groups a network interface
// carries, what each instance depends on, and which FSx filesystems a live
// instance references.
func (b *fpBuilder) enrichEC2(ctx context.Context, regions []string) {
	for _, region := range regions {
		ec := ec2.NewFromConfig(b.c.regionalConfig(region))

		// Security groups: name, and whether a network interface carries it.
		sgIdx := map[string]int{}
		var sgIDs []string
		for _, i := range b.ofType(fs.TypeSecurityGroup, region) {
			id := idOf(b.res[i].ARN)
			sgIdx[id] = i
			sgIDs = append(sgIDs, id)
		}
		for _, chunk := range chunks(sgIDs, 200) {
			out, err := ec.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
				Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: chunk}},
			})
			if err != nil {
				b.warn(fmt.Sprintf("%s: security group names: %v", region, err))
				break
			}
			for _, g := range out.SecurityGroups {
				if i, ok := sgIdx[aws.ToString(g.GroupId)]; ok {
					b.res[i].Name = aws.ToString(g.GroupName)
				}
			}
		}
		for _, chunk := range chunks(sgIDs, 200) {
			used := map[string]int{}
			p := ec2.NewDescribeNetworkInterfacesPaginator(ec, &ec2.DescribeNetworkInterfacesInput{
				Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: chunk}},
			})
			var err error
			for p.HasMorePages() {
				var page *ec2.DescribeNetworkInterfacesOutput
				if page, err = p.NextPage(ctx); err != nil {
					break
				}
				for _, ni := range page.NetworkInterfaces {
					for _, g := range ni.Groups {
						used[aws.ToString(g.GroupId)]++
					}
				}
			}
			for _, id := range chunk {
				f := b.facts[b.res[sgIdx[id]].ARN]
				switch {
				case err != nil:
					f.InUse = "its network interfaces could not be read: " + err.Error()
				case used[id] > 0:
					f.InUse = fmt.Sprintf("on %d network interface(s)", used[id])
				}
			}
			if err != nil {
				b.warn(fmt.Sprintf("%s: network interfaces: %v (reading them needs ec2:DescribeNetworkInterfaces)", region, err))
			}
		}

		// Key pairs: name, and a workstation key is the user's.
		kpIdx := map[string]int{}
		var kpIDs []string
		for _, i := range b.ofType(fs.TypeKeyPair, region) {
			id := idOf(b.res[i].ARN)
			kpIdx[id] = i
			kpIDs = append(kpIDs, id)
			if b.res[i].Tags["spawn:purpose"] == "ssh-key" {
				b.facts[b.res[i].ARN].UserData = "the user's workstation key pair"
			}
		}
		if len(kpIDs) > 0 {
			out, err := ec.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
				Filters: []ec2types.Filter{{Name: aws.String("key-pair-id"), Values: kpIDs}},
			})
			if err != nil {
				b.warn(fmt.Sprintf("%s: key pair names: %v", region, err))
			} else {
				for _, k := range out.KeyPairs {
					if i, ok := kpIdx[aws.ToString(k.KeyPairId)]; ok {
						b.res[i].Name = aws.ToString(k.KeyName)
					}
				}
			}
		}

		// AMIs and snapshots hold what the user built.
		for _, t := range []string{fs.TypeImage, fs.TypeSnapshot} {
			for _, i := range b.ofType(t, region) {
				b.facts[b.res[i].ARN].UserData = "an image or snapshot the user built"
			}
		}

		// Instances: what each depends on.
		instIdx := map[string]int{}
		var instIDs []string
		for _, i := range b.ofType(fs.TypeInstance, region) {
			id := idOf(b.res[i].ARN)
			instIdx[id] = i
			instIDs = append(instIDs, id)
		}
		for _, chunk := range chunks(instIDs, 200) {
			p := ec2.NewDescribeInstancesPaginator(ec, &ec2.DescribeInstancesInput{
				Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: chunk}},
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					b.warn(fmt.Sprintf("%s: instance dependencies: %v", region, err))
					break
				}
				for _, rv := range page.Reservations {
					for _, inst := range rv.Instances {
						i, ok := instIdx[aws.ToString(inst.InstanceId)]
						if !ok {
							continue
						}
						for _, g := range inst.SecurityGroups {
							b.res[i].DependsOn = append(b.res[i].DependsOn,
								fmt.Sprintf("arn:aws:ec2:%s:%s:security-group/%s", region, b.account, aws.ToString(g.GroupId)))
						}
						if inst.IamInstanceProfile != nil {
							b.res[i].DependsOn = append(b.res[i].DependsOn, aws.ToString(inst.IamInstanceProfile.Arn))
						}
					}
				}
			}
		}

		// FSx: referenced by a live instance's lease tag.
		if fsx := b.ofType(fs.TypeFileSystem, region); len(fsx) > 0 {
			refs := map[string]string{}
			p := ec2.NewDescribeInstancesPaginator(ec, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
				{Name: aws.String("tag-key"), Values: []string{"spawn:fsx-id", "spawn:fsx-pending"}},
				{Name: aws.String("instance-state-name"), Values: liveInstanceStates},
			}})
			var err error
			for p.HasMorePages() {
				var page *ec2.DescribeInstancesOutput
				if page, err = p.NextPage(ctx); err != nil {
					break
				}
				for _, rv := range page.Reservations {
					for _, inst := range rv.Instances {
						t := ec2TagMap(inst.Tags)
						for _, k := range []string{"spawn:fsx-id", "spawn:fsx-pending"} {
							if v := t[k]; v != "" {
								refs[v] = aws.ToString(inst.InstanceId)
							}
						}
					}
				}
			}
			for _, i := range fsx {
				f := b.facts[b.res[i].ARN]
				switch id := idOf(b.res[i].ARN); {
				case err != nil:
					f.InUse = "the instances that may use it could not be read: " + err.Error()
				case refs[id] != "":
					f.InUse = "referenced by " + refs[id]
				}
			}
		}
	}
}

func chunks(xs []string, n int) [][]string {
	var out [][]string
	for len(xs) > 0 {
		k := min(n, len(xs))
		out = append(out, xs[:k])
		xs = xs[k:]
	}
	return out
}

// enrichIAM reads the tags of the roles and instance profiles the name scan
// found (ListRoles and ListInstanceProfiles return none), and their
// dependencies: a profile depends on its roles, a role on its attached
// policies.
func (b *fpBuilder) enrichIAM(ctx context.Context) {
	ic := iam.NewFromConfig(b.c.regionalConfig(""))
	for _, i := range b.ofType(fs.TypeRole, "") {
		r := &b.res[i]
		if len(r.Tags) == 0 {
			tags := map[string]string{}
			p := iam.NewListRoleTagsPaginator(ic, &iam.ListRoleTagsInput{RoleName: aws.String(r.Name)})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					b.warn(fmt.Sprintf("iam role %s: tags: %v (reading them needs iam:ListRoleTags)", r.Name, err))
					break
				}
				for _, t := range page.Tags {
					tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}
			}
			r.Tags = tags
		}
		p := iam.NewListAttachedRolePoliciesPaginator(ic, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(r.Name)})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				b.warn(fmt.Sprintf("iam role %s: attached policies: %v", r.Name, err))
				break
			}
			for _, ap := range page.AttachedPolicies {
				r.DependsOn = append(r.DependsOn, aws.ToString(ap.PolicyArn))
			}
		}
	}
	for _, i := range b.ofType(fs.TypeInstanceProfile, "") {
		r := &b.res[i]
		if len(r.Tags) == 0 {
			tags := map[string]string{}
			p := iam.NewListInstanceProfileTagsPaginator(ic, &iam.ListInstanceProfileTagsInput{InstanceProfileName: aws.String(r.Name)})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					b.warn(fmt.Sprintf("iam instance profile %s: tags: %v (reading them needs iam:ListInstanceProfileTags)", r.Name, err))
					break
				}
				for _, t := range page.Tags {
					tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}
			}
			r.Tags = tags
		}
		r.DependsOn = append(r.DependsOn, b.profileRoles[r.ARN]...)
		if r.Name == instancerole.SharedProfileName {
			b.facts[r.ARN].Residue = "spawn's shared spored instance profile"
		}
	}
	for _, i := range b.ofType(fs.TypeRole, "") {
		if b.res[i].Name == instancerole.SharedRoleName {
			b.facts[b.res[i].ARN].Residue = "spawn's shared spored role"
		}
	}
}

// enrichBuckets reads each bucket's tags, and whether a results bucket is
// empty. Only an empty staging or results bucket is litter; anything else
// may hold the user's data.
func (b *fpBuilder) enrichBuckets(ctx context.Context) {
	for _, i := range b.ofType(fs.TypeBucket, "") {
		r := &b.res[i]
		f := b.facts[r.ARN]
		region := b.bucketRegion[r.ARN]
		if region == "" && r.Region != fs.RegionGlobal {
			region = r.Region
		}
		if region == "" {
			f.UserData = "its region is unknown, so whether it is empty could not be checked"
			continue
		}
		r.Region = region
		sc := s3.NewFromConfig(b.c.regionalConfig(region))
		if len(r.Tags) == 0 {
			if out, err := sc.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
				Bucket: aws.String(r.Name), ExpectedBucketOwner: aws.String(b.account),
			}); err == nil {
				r.Tags = map[string]string{}
				for _, t := range out.TagSet {
					r.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}
			} else if !strings.Contains(err.Error(), "NoSuchTagSet") {
				b.warn(fmt.Sprintf("s3 bucket %s: tags: %v", r.Name, err))
			}
		}
		switch {
		case strings.HasPrefix(r.Name, taskproto.StagingBucketPrefix):
			// Never listed (#1274): its objects expire after a day, and S3
			// refuses to delete it while it holds anything.
		case strings.HasPrefix(r.Name, "spawn-results-"):
			empty, err := bucketEmpty(ctx, sc, r.Name, b.account)
			switch {
			case err != nil:
				f.UserData = "whether it is empty could not be checked: " + err.Error()
			case !empty:
				f.UserData = "holds results"
			}
		default:
			f.UserData = "may hold the user's data"
		}
	}
}

// bucketEmpty reports whether a bucket holds no object version, delete
// marker or multipart upload.
func bucketEmpty(ctx context.Context, sc *s3.Client, bucket, owner string) (bool, error) {
	v, err := sc.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket), ExpectedBucketOwner: aws.String(owner), MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, err
	}
	if len(v.Versions) > 0 || len(v.DeleteMarkers) > 0 {
		return false, nil
	}
	u, err := sc.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(bucket), ExpectedBucketOwner: aws.String(owner), MaxUploads: aws.Int32(1),
	})
	if err != nil {
		return false, err
	}
	return len(u.Uploads) == 0, nil
}

// enrichControlPlaneTags reads the tags of the name-matched Lambda
// functions, log groups, tables, buses and rules, so their ownership is
// read from their tags rather than assumed from their names. A failed read
// leaves the resource untagged: legacy-name, which is never deleted.
func (b *fpBuilder) enrichControlPlaneTags(ctx context.Context) {
	for i := range b.res {
		r := &b.res[i]
		if len(r.Tags) > 0 {
			continue
		}
		cfg := b.c.regionalConfig(r.Region)
		tags := map[string]string{}
		var err error
		switch r.Type {
		case fs.TypeFunction:
			var out *lambda.ListTagsOutput
			if out, err = lambda.NewFromConfig(cfg).ListTags(ctx, &lambda.ListTagsInput{Resource: aws.String(r.ARN)}); err == nil {
				tags = out.Tags
			}
		case fs.TypeLogGroup:
			var out *cwlogs.ListTagsForResourceOutput
			if out, err = cwlogs.NewFromConfig(cfg).ListTagsForResource(ctx, &cwlogs.ListTagsForResourceInput{ResourceArn: aws.String(r.ARN)}); err == nil {
				tags = out.Tags
			}
		case fs.TypeTable:
			var out *dynamodb.ListTagsOfResourceOutput
			if out, err = dynamodb.NewFromConfig(cfg).ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{ResourceArn: aws.String(r.ARN)}); err == nil {
				for _, t := range out.Tags {
					tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}
			}
		case fs.TypeEventBus, fs.TypeRule:
			var out *eventbridge.ListTagsForResourceOutput
			if out, err = eventbridge.NewFromConfig(cfg).ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{ResourceARN: aws.String(r.ARN)}); err == nil {
				for _, t := range out.Tags {
					tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}
			}
		default:
			continue
		}
		if err != nil {
			b.warn(fmt.Sprintf("%s %s: tags: %v", r.Type, r.Name, err))
		}
		if tags == nil {
			tags = map[string]string{}
		}
		r.Tags = tags
	}
}

// markInUse marks the instance profiles on live instances, their roles, and
// the pool queues with workers, active. IAM is global, so every enabled
// region is checked whatever was scanned; if any cannot be, every role,
// profile and pool queue is active, since "unused" could not be shown
// (poolqueue.WorkersInUse, the check `spawn cleanup --pool-queues` and the
// reaper use; its Profiles are every live instance's, a superset of
// instancerole.ProfilesInUse's per-task ones).
func (b *fpBuilder) markInUse(ctx context.Context, scanned []string) {
	profiles := b.ofType(fs.TypeInstanceProfile, "")
	roles := b.ofType(fs.TypeRole, "")
	var queues []int
	for _, i := range b.ofType(fs.TypeQueue, "") {
		if _, ok := poolqueue.RunID(b.res[i].Name); ok {
			queues = append(queues, i)
		}
	}
	if len(profiles)+len(roles)+len(queues) == 0 {
		return
	}
	cfg := b.c.regionalConfig("")
	regions, err := instancerole.EnabledRegions(ctx, ec2.NewFromConfig(cfg), scanned...)
	var u poolqueue.InUse
	if err == nil {
		ec2s := make(map[string]instancerole.EC2Describer, len(regions))
		for _, r := range regions {
			ec2s[r] = ec2.NewFromConfig(b.c.regionalConfig(r))
		}
		u, err = poolqueue.WorkersInUse(ctx, ec2s)
	}
	if err != nil {
		why := "the instances that may use it could not all be read: " + err.Error()
		b.warn("instances in every enabled region: " + err.Error())
		for _, i := range append(append(profiles, roles...), queues...) {
			if f := b.facts[b.res[i].ARN]; f.InUse == "" {
				f.InUse = why
			}
		}
		return
	}
	usedRoles := map[string]string{}
	for _, i := range profiles {
		r := b.res[i]
		if u.Profiles[r.Name] {
			b.facts[r.ARN].InUse = "attached to a live instance"
			for _, role := range b.profileRoles[r.ARN] {
				usedRoles[role] = r.Name
			}
		}
	}
	for _, i := range roles {
		r := b.res[i]
		switch {
		case usedRoles[r.ARN] != "":
			b.facts[r.ARN].InUse = "its instance profile " + usedRoles[r.ARN] + " is attached to a live instance"
		case u.Profiles[r.Name]:
			// The same-name profile, when the profile itself was not listed.
			b.facts[r.ARN].InUse = "its instance profile is attached to a live instance"
		}
	}
	for _, i := range queues {
		r := b.res[i]
		if id, _ := poolqueue.RunID(r.Name); u.Holds(id) {
			b.facts[r.ARN].InUse = "the pool has live workers"
		}
	}
}
