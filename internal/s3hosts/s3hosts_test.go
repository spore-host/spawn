package s3hosts

import (
	"strings"
	"testing"
)

// The checker must tell the forms apart, or every gate built on it passes
// vacuously (CLAUDE.md, "Writing a gate").
func TestCheckTellsRegionalFromGlobal(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		bad        bool
	}{
		{"regional virtual-hosted, IMDS region", `S3_BASE_URL="https://spawn-binaries-${REGION}.s3.${REGION}.amazonaws.com"`, false},
		{"regional with a default", `for B in "https://spawn-binaries-${SPAWN_BS_REGION:-us-east-1}.s3.${SPAWN_BS_REGION:-us-east-1}.amazonaws.com"; do`, false},
		{"the us-east-1 fallback by its regional name", `FALLBACK_URL="https://spawn-binaries-us-east-1.s3.us-east-1.amazonaws.com"`, false},
		{"PowerShell braced region", `Invoke-WebRequest -Uri "https://amazon-ssm-${region}.s3.${region}.amazonaws.com/latest/x.exe"`, false},
		{"regional path-style", `curl https://s3.eu-west-2.amazonaws.com/bucket/key`, false},
		{"not S3", `curl https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip`, false},
		{"global virtual-hosted", `S3_BASE_URL="https://spawn-binaries-${REGION}.s3.amazonaws.com"`, true},
		{"global fallback", `FALLBACK_URL="https://spawn-binaries-us-east-1.s3.amazonaws.com"`, true},
		{"global path-style", `Invoke-WebRequest -Uri 'https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/x.exe'`, true},
		{"another region's regional name", `curl https://other-bucket.s3.us-west-2.amazonaws.com/k`, true},
		{"the fallback's name for another bucket", `curl https://someone.s3.us-east-1.amazonaws.com/k`, true},
		{"an unknown variable in the host", `curl "https://spawn-binaries-$R.s3.$R.amazonaws.com/k"`, true},
		{"aws s3 without --region", `    aws s3 cp "s3://${CERT_BUCKET}/k" /etc/x 2>/dev/null && \`, true},
		{"aws s3 with --region", `    aws s3 cp --region "$REGION" "s3://${CERT_BUCKET}/k" /etc/x && \`, false},
		{"aws with options before s3", `if aws "$AWS_REGION_ARG" s3api head-object --bucket b --key k; then`, false},
		{"PowerShell aws s3 with --region", `  & $aws s3 cp $src.u $cand --region $src.r 2>$null`, false},
		{"PowerShell aws s3 without --region", `  & $aws s3 cp $src.u $cand 2>$null`, true},
		{"a source with its own region", `  @{ u = 's3://spawn-binaries-us-east-1/spawn/x.exe'; r = 'us-east-1' }`, false},
		{"a source with another region", `  @{ u = 's3://spawn-binaries-us-east-1/spawn/x.exe'; r = 'eu-west-2' }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, probs := Check(tc.line, "eu-west-2")
			if len(refs) == 0 && tc.bad {
				t.Fatalf("found nothing in %q", tc.line)
			}
			if (len(probs) > 0) != tc.bad {
				t.Errorf("problems = %v, want any: %t", probs, tc.bad)
			}
		})
	}
	// Prose is not a call.
	if refs, _ := Check("# the old code ran aws s3 cp without --region\necho 'use aws s3 cp'", "eu-west-2"); len(refs) != 0 {
		t.Errorf("matched prose: %v", refs)
	}
	// $bucket counts as the launch region's only where the script says so.
	src := `  @{ u = "s3://$bucket/spawn/x.exe"; r = $region },`
	if _, p := Check(src, "eu-west-2"); len(p) == 0 {
		t.Error("accepted $bucket with no $bucket = \"spawn-binaries-$region\" in the script")
	}
	if _, p := Check("$bucket = \"spawn-binaries-$region\"\n"+src, "eu-west-2"); len(p) != 0 {
		t.Errorf("rejected the launch region's $bucket: %v", p)
	}
}

func TestGlobalHosts(t *testing.T) {
	text := strings.Join([]string{
		`a := "https://spawn-binaries-%s.s3.amazonaws.com"`,
		`b := "https://spawn-binaries-${REGION}.s3.${REGION}.amazonaws.com"`,
		`c := "https://spawn-binaries-us-east-1.s3.us-east-1.amazonaws.com/spawn/versions/%s/bootstrap.sh"`,
		`d := "https://s3.amazonaws.com/ec2-downloads-windows/x"`,
		`e := "http://s3.amazonaws.com/doc/2006-03-01/"`,
	}, "\n")
	got := GlobalHosts(text)
	var lines []int
	for _, r := range got {
		lines = append(lines, r.Line)
	}
	if len(lines) != 3 || lines[0] != 1 || lines[1] != 4 || lines[2] != 5 {
		t.Errorf("global hosts on lines %v, want [1 4 5]: %v", lines, got)
	}
}
