package elasticip

import "testing"

// The tag must equal what IAM reports as ec2:SourceInstanceARN, partition
// included, or the instance role's release grant never matches.
func TestInstanceARN(t *testing.T) {
	for region, want := range map[string]string{
		"us-west-2":     "arn:aws:ec2:us-west-2:111122223333:instance/i-1",
		"cn-north-1":    "arn:aws-cn:ec2:cn-north-1:111122223333:instance/i-1",
		"us-gov-west-1": "arn:aws-us-gov:ec2:us-gov-west-1:111122223333:instance/i-1",
	} {
		if got := InstanceARN(region, "111122223333", "i-1"); got != want {
			t.Errorf("InstanceARN(%s) = %s, want %s", region, got, want)
		}
	}
}
