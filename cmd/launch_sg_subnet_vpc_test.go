package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// The launch's security groups (MPI, Windows, --efa-group) must be created in
// the VPC of an explicit --subnet-id when --vpc is not given. They were created
// in the DEFAULT VPC, and RunInstances refused every member: "Security group
// sg-… and subnet subnet-… belong to different networks" (live, 2026-11-03, a
// --mpi --efa --no-public-ip launch into a subnet of a non-default VPC, #1515).

// vpcFakeEC2 knows subnet-x (in vpc-sub) and the region's default VPC.
type vpcFakeEC2 struct{ actions []string }

func (f *vpcFakeEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	a := r.Form.Get("Action")
	f.actions = append(f.actions, a)
	w.Header().Set("Content-Type", "text/xml")
	switch {
	case a == "DescribeSubnets" && r.Form.Get("SubnetId.1") == "subnet-x":
		// A fixed XML fixture for the SDK, not HTML served to a browser.
		// nosemgrep
		_, _ = w.Write([]byte(`<DescribeSubnetsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId><subnetSet><item><subnetId>subnet-x</subnetId><vpcId>vpc-sub</vpcId></item></subnetSet></DescribeSubnetsResponse>`))
	case a == "DescribeSubnets":
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidSubnetID.NotFound</Code><Message>no such subnet</Message></Error></Errors><RequestID>r</RequestID></Response>`))
	case a == "DescribeVpcs":
		// nosemgrep
		_, _ = w.Write([]byte(`<DescribeVpcsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId><vpcSet><item><vpcId>vpc-default</vpcId><isDefault>true</isDefault></item></vpcSet></DescribeVpcsResponse>`))
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidAction</Code><Message>not in this fake</Message></Error></Errors><RequestID>r</RequestID></Response>`))
	}
}

func TestLaunchSecurityGroupVPCFollowsTheSubnet(t *testing.T) {
	keepLaunchGlobals(t)
	dir := isolateAWSEnv(t)
	f := &vpcFakeEC2{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	client, err := aws.NewClientWithRegion(context.Background(), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, vpc, subnet, want, wantErr string
	}{
		{name: "explicit --vpc wins", vpc: "vpc-e", subnet: "subnet-x", want: "vpc-e"},
		{name: "the --subnet-id's VPC", subnet: "subnet-x", want: "vpc-sub"},
		{name: "neither: the default VPC", want: "vpc-default"},
		{name: "an unreadable subnet fails closed", subnet: "subnet-gone", wantErr: "cannot read the VPC of --subnet-id subnet-gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := client.ResolveLaunchVPC(context.Background(), "us-east-1", tc.vpc, tc.subnet)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ResolveLaunchVPC(%q, %q) = %q, %v; want %q", tc.vpc, tc.subnet, got, err, tc.want)
			}
		})
	}
}

// Every launch path that creates a security group must resolve its VPC with
// ResolveLaunchVPC, which knows about --subnet-id, never the subnet-blind
// ResolveVPC on config.VPCID alone.
func TestLaunchSecurityGroupCreatorsUseTheSubnetAwareVPC(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	creators := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "ResolveVPC(ctx, config.Region, config.VPCID)") {
			t.Errorf("%s resolves a launch's VPC with ResolveVPC on config.VPCID alone; use ResolveLaunchVPC(ctx, config.Region, config.VPCID, config.SubnetID)", file)
		}
		creators += strings.Count(src, "ResolveLaunchVPC(ctx, config.Region, config.VPCID, config.SubnetID)")
	}
	if creators < 3 {
		t.Errorf("found %d ResolveLaunchVPC calls in cmd; want the MPI, Windows and --efa-group security groups (3)", creators)
	}
}
