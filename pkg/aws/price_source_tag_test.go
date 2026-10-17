package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// #1076: a cost computed later from spawn:price-per-hour must be able to say
// whether the rate was live or from truffle's static table, so a static figure
// can be labelled static. The source travels from the resolver to the tag.
func TestPriceSourceReachesTheInstanceTags(t *testing.T) {
	for _, src := range []truffleaws.PriceSource{truffleaws.PriceSourceLive, truffleaws.PriceSourceStatic} {
		cfg := &LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", Name: "x"}
		if err := resolvePricePerHour(context.Background(), fakePricer{price: 0.1, source: src}, cfg, false); err != nil {
			t.Fatal(err)
		}
		got := ""
		tags, err := buildTags(*cfg, "123456789012", "arn:aws:iam::123456789012:user/u", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, tg := range tags {
			if aws.ToString(tg.Key) == PriceSourceTag {
				got = aws.ToString(tg.Value)
			}
		}
		if got != string(src) {
			t.Errorf("source %q: %s tag = %q", src, PriceSourceTag, got)
		}
	}
	// A caller-supplied rate has no recorded source, and none is invented.
	tags, err := buildTags(LaunchConfig{InstanceType: "m7i.large", PricePerHour: 0.1, Name: "x"}, "123456789012", "arn", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range tags {
		if aws.ToString(tg.Key) == PriceSourceTag {
			t.Errorf("a source was invented for a caller-supplied rate: %q", aws.ToString(tg.Value))
		}
	}
}
