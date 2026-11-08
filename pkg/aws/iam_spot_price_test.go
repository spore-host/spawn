package aws

import (
	"encoding/json"
	"testing"
)

// spored reads its own Spot price so --cost-limit charges a Spot instance at
// what it pays (#1497); spawn provisions the grant, in BOTH policies a spored
// role carries: the baseline every spawn-instance-* role gets, and the
// spored-instance-role / DCV role policy. A read-only Describe on "*" (the API
// has no resource-level scoping) with no condition and no policy variable,
// since PutRolePolicy rejects one in a Resource (#1069).
func TestSporedPoliciesGrantDescribeSpotPriceHistory(t *testing.T) {
	docs := map[string]string{
		"spored-baseline-policy":   (&Client{}).sporedBaselinePolicyDoc(),
		"spored-instance-role/DCV": sporedDCVRolePolicy,
	}
	for name, doc := range docs {
		var pol struct {
			Statement []struct {
				Effect    string
				Action    any
				Resource  any
				Condition any
			}
		}
		if err := json.Unmarshal([]byte(doc), &pol); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, st := range pol.Statement {
			actions, _ := st.Action.([]any)
			if s, ok := st.Action.(string); ok {
				actions = []any{s}
			}
			for _, a := range actions {
				if a != "ec2:DescribeSpotPriceHistory" {
					continue
				}
				found = true
				if st.Effect != "Allow" || st.Resource != "*" || st.Condition != nil {
					t.Errorf("%s: ec2:DescribeSpotPriceHistory granted as %+v, want Allow on \"*\" with no condition", name, st)
				}
			}
		}
		if !found {
			t.Errorf("%s does not grant ec2:DescribeSpotPriceHistory: spored would charge every Spot instance at on-demand", name)
		}
	}
}
