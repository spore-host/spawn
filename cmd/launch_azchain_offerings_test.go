package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// zonesEC2 answers the three zone lookups a job array's AZ chain could make:
// DescribeAvailabilityZones with every zone, DescribeInstanceTypeOfferings
// with offered, DescribeSubnets with a default subnet in each of defaults.
type zonesEC2 struct {
	all, offered, defaults []string
}

func (f zonesEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	var b bytes.Buffer
	switch form.Get("Action") {
	case "DescribeAvailabilityZones":
		fmt.Fprintf(&b, `<DescribeAvailabilityZonesResponse %s><requestId>r</requestId><availabilityZoneInfo>`, ns)
		for _, z := range f.all {
			fmt.Fprintf(&b, `<item><zoneName>%s</zoneName><zoneState>available</zoneState></item>`, z)
		}
		b.WriteString(`</availabilityZoneInfo></DescribeAvailabilityZonesResponse>`)
	case "DescribeInstanceTypeOfferings":
		fmt.Fprintf(&b, `<DescribeInstanceTypeOfferingsResponse %s><requestId>r</requestId><instanceTypeOfferingSet>`, ns)
		for _, z := range f.offered {
			fmt.Fprintf(&b, `<item><instanceType>%s</instanceType><locationType>availability-zone</locationType><location>%s</location></item>`, form.Get("Filter.1.Value.1"), z)
		}
		b.WriteString(`</instanceTypeOfferingSet></DescribeInstanceTypeOfferingsResponse>`)
	case "DescribeSubnets":
		fmt.Fprintf(&b, `<DescribeSubnetsResponse %s><requestId>r</requestId><subnetSet>`, ns)
		for _, z := range f.defaults {
			fmt.Fprintf(&b, `<item><subnetId>subnet-%s</subnetId><availabilityZone>%s</availabilityZone><defaultForAz>true</defaultForAz></item>`, z, z)
		}
		b.WriteString(`</subnetSet></DescribeSubnetsResponse>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		b.WriteString(`<Response><Errors><Error><Code>InvalidAction</Code><Message>not in this fake</Message></Error></Errors><RequestID>r</RequestID></Response>`)
	}
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write(b.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
}

func zonesClient(t *testing.T, f zonesEC2) *aws.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return aws.NewClientFromConfig(awssdk.Config{
		Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: awssdk.String(srv.URL), HTTPClient: srv.Client(),
	})
}

func chainZones(chain []cohort.Rung) []string {
	var out []string
	for _, r := range chain {
		out = append(out, r.AvailZone)
	}
	return out
}

// A job array's AZ chain comes from the zones that offer the type and have a
// default subnet, the list a single launch falls back across (#1458), not
// every zone in the region: us-east-1b does not offer the type, us-east-1e
// has no default subnet. A rung in either fails as terminal and ends the
// chain there.
func TestBuildAZChainUsesTheZonesThatOfferTheType(t *testing.T) {
	c := zonesClient(t, zonesEC2{
		all:      []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d", "us-east-1e"},
		offered:  []string{"us-east-1a", "us-east-1c", "us-east-1d", "us-east-1e"},
		defaults: []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d"},
	})
	base := &aws.LaunchConfig{Region: "us-east-1", InstanceType: "g7e.8xlarge"}
	_, chain := buildAZChain(context.Background(), c, base, cohort.CapacityOnDemand)
	if got, want := chainZones(chain), []string{"us-east-1a", "us-east-1c", "us-east-1d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("chain zones %q, want %q", got, want)
	}

	// A zone the caller named stays first, even one the lookup leaves out:
	// the chain does not move a cohort out of its zone before trying it.
	base.AvailabilityZone = "us-east-1b"
	_, chain = buildAZChain(context.Background(), c, base, cohort.CapacityOnDemand)
	if got := chainZones(chain); len(got) == 0 || got[0] != "us-east-1b" {
		t.Errorf("chain zones %q, want the named us-east-1b first", got)
	}
}
