package taskproto

import "strings"

// S3 transfer tuning for the generated scripts (spawn#1256).
//
// Every stage-in and stage-out used to run `aws s3 cp` at the CLI's defaults:
// the classic Python transfer client, 10 concurrent requests and 8 MiB parts.
// Measured on Graviton4 boxes in-region that ran at 179-293 MB/s, and staging
// was most of the bill (96% of the window on a 215 GB Argo stage-in). The same
// copies with `preferred_transfer_client = crt`, `target_bandwidth` set to the
// box's peak NIC and 16 MiB parts ran at 1.53-2.68 GB/s, 5-15x faster, with
// identical outputs (A/B on the issue).
//
// How it is applied. The wrapper writes a per-run AWS CLI config with mktemp and
// points only its own `aws` invocations at it, through spawn_aws. Not `aws
// configure set`, which rewrites the user's ~/.aws/config, and not an exported
// AWS_CONFIG_FILE, which would reach the user's command: a host command's own
// `aws --profile x` would then find no such profile, and its `aws configure set`
// would write into a file this script deletes.
//
// When the user supplied a config (AWS_CONFIG_FILE, AWS_PROFILE or
// AWS_DEFAULT_PROFILE in the environment, which includes the spec's env, or an
// existing ~/.aws/config) the tuning is skipped and the CLI runs exactly as
// before. Merging is not attempted: a second [default] section is a parse error
// to the CLI's ini reader, and a named profile would need its own copy.
//
// Which client, by what the box can do:
//
//   - AWS CLI v2.14.0 or later (the release that made the CRT client and
//     target_bandwidth stable and named the old client `classic`) and at least
//     ~8 GiB of RAM: crt, with target_bandwidth from the instance type's NIC
//     class (nicSizeClasses). The CRT client reserves part buffers by target
//     throughput (2 GiB up to 25 Gb/s in aws-c-s3), which a small box cannot
//     spare, so those keep the classic client.
//   - Older CLIs, or a version that cannot be read: no preferred_transfer_client
//     line at all, since `classic` is not a valid value before 2.14.0 and an
//     invalid choice fails every copy. The classic settings below still apply.
//   - Classic: 32 concurrent requests and 16 MiB parts, the fallback the issue
//     first asked for.
//
// The terminal-flush hook always uses classic (crtAllowed=false). Its uploads
// rely on --cli-connect-timeout/--cli-read-timeout to fit its 45 s deadline,
// and the CRT client is built without them (awscli/s3transfer/crt.py passes no
// timeout to the CRT S3 client). Pinning classic also stops CLI 2.38.0+, whose
// `auto` resolves to crt on every host, from silently dropping those timeouts.
//
// Kept terse: this rides in the instance's user data (16 KiB after gzip).

// nicClass is one row of the instance-size to target_bandwidth table.
type nicClass struct {
	// Sizes are shell case patterns over the part of the instance type after
	// the first dot ("16xlarge", "metal-24xl").
	Sizes []string
	// Gbps is the target for a general-purpose family; NetOptGbps for a
	// network-optimized one (a family with an "n" after its generation digit:
	// c7gn, m6in, r6idn, c5n).
	Gbps, NetOptGbps int
}

// nicSizeClasses maps an instance size to the CRT target_bandwidth in Gb/s.
// Each value is at or a little above the peak network bandwidth AWS documents
// for that size across current families (r8g, r7i, m7i, c7g, r6in, c7gn), so
// a box is never asked for less than it can do: a target above the NIC costs
// some buffer memory, a target below it caps the transfer. Burstable sizes
// (4xlarge and smaller) get their burst peak, since a stage-in is a burst.
// First match wins, so metal-24xl precedes metal-*.
var nicSizeClasses = []nicClass{
	{Sizes: []string{"nano", "micro", "small", "medium", "large", "xlarge", "2xlarge", "3xlarge", "4xlarge"}, Gbps: 15, NetOptGbps: 50},
	{Sizes: []string{"6xlarge", "8xlarge", "9xlarge", "10xlarge"}, Gbps: 15, NetOptGbps: 100},
	{Sizes: []string{"12xlarge"}, Gbps: 25, NetOptGbps: 150},
	{Sizes: []string{"16xlarge", "18xlarge"}, Gbps: 30, NetOptGbps: 200},
	{Sizes: []string{"24xlarge", "metal-24xl"}, Gbps: 40, NetOptGbps: 200},
	{Sizes: []string{"32xlarge", "48xlarge", "metal", "metal-*"}, Gbps: 50, NetOptGbps: 200},
}

// crtOptimizedTypes are the instance types the AWS CLI's documentation lists as
// ones the CRT client is optimized for, and so detects the bandwidth of itself
// (https://docs.aws.amazon.com/cli/latest/topic/s3-config.html,
// preferred_transfer_client). For these no target_bandwidth is written: the
// CLI's own figure is far above anything in nicSizeClasses.
var crtOptimizedTypes = []string{
	"p4d.24xlarge", "p4de.24xlarge", "p5.48xlarge", "p5e.48xlarge", "p5en.48xlarge",
	"p6-b200.48xlarge", "p6-b300.48xlarge", "trn1.32xlarge", "trn1n.32xlarge",
}

// crtMinMemKiB is the MemTotal at or above which the CRT client is used. An
// "8 GiB" instance reports a little under 8 GiB, hence 7 GiB.
const crtMinMemKiB = 7 * 1024 * 1024

// meminfoPath is read for MemTotal. A generate-time value so tests can supply
// one; macOS has no /proc.
var meminfoPath = "/proc/meminfo"

// writeNICTarget emits the shell that sets G to the target_bandwidth in Gb/s
// for the instance type in $IT, or to nothing when the CLI should choose
// (a CRT-optimized type, an unknown size, or no type from IMDS).
func writeNICTarget(p func(string, ...interface{})) {
	p("G=''; case \"$IT\" in %s) ;; *)\n", strings.Join(crtOptimizedTypes, "|"))
	p("  case \"${IT%%%%.*}\" in *[0-9]*n*) X=1 ;; *) X=0 ;; esac\n")
	p("  case \"${IT#*.}\" in\n")
	for _, c := range nicSizeClasses {
		p("    %s) G=$((X?%d:%d)) ;;\n", strings.Join(c.Sizes, "|"), c.NetOptGbps, c.Gbps)
	}
	p("  esac ;; esac\n")
}

// writeS3Config emits the per-run AWS CLI config into $S3CFG (” when skipped).
// crtAllowed is true for the wrapper, which also gets spawn_aws, the function
// every one of its S3 copies goes through; false for the flush hook, which
// exports the file instead (see GenerateFlushScript). The wrapper removes its
// file before exit; the hook removes its own on EXIT.
func writeS3Config(p func(string, ...interface{}), crtAllowed bool) {
	p("# ---- S3 transfer settings (#1256) ----\n")
	p("S3CFG=''\n")
	if crtAllowed {
		p("spawn_aws() { if [ -n \"$S3CFG\" ]; then AWS_CONFIG_FILE=\"$S3CFG\" aws \"$@\"; else aws \"$@\"; fi; }\n")
	}
	p("if [ -n \"${AWS_CONFIG_FILE:-}${AWS_PROFILE:-}${AWS_DEFAULT_PROFILE:-}\" ] || [ -e \"${HOME:-/nonexistent}/.aws/config\" ]; then\n")
	p("  echo 'spawn: using your AWS CLI config as given; S3 transfer tuning skipped'\n")
	p("elif S3CFG=\"$(mktemp 2>/dev/null)\"; then\n")
	// "aws-cli/2.15.30 Python/..." -> M=2 N=15. Anything unreadable is 0.0.
	p("  V=\"$(aws --version 2>&1)\"; V=\"${V#aws-cli/}\"; M=\"${V%%%%.*}\"; N=\"${V#*.}\"; N=\"${N%%%%.*}\"\n")
	p("  case \"$M.$N\" in *[!0-9.]*|.*|*.) M=0 N=0 ;; esac\n")
	p("  T=''; G=''\n")
	p("  if [ \"$M\" -gt 2 ] || { [ \"$M\" -eq 2 ] && [ \"$N\" -ge 14 ]; }; then\n")
	p("    T=classic\n")
	if crtAllowed {
		p("    if [ \"$(awk '/^MemTotal:/{print $2}' %s 2>/dev/null)\" -ge %d ] 2>/dev/null; then\n", shQuote(meminfoPath), crtMinMemKiB)
		p("      T=crt\n")
		writeNICTarget(func(f string, a ...interface{}) { p("      "+f, a...) })
		p("    fi\n")
	}
	p("  fi\n")
	p("  { echo '[default]'; echo 's3 ='\n")
	p("    [ -z \"$T\" ] || echo \"  preferred_transfer_client = $T\"\n")
	p("    if [ \"$T\" = crt ]; then [ -z \"$G\" ] || echo \"  target_bandwidth = ${G}Gb/s\"; else echo '  max_concurrent_requests = 32'; fi\n")
	p("    echo '  multipart_chunksize = 16MB'; } > \"$S3CFG\" || { rm -f \"$S3CFG\"; S3CFG=''; }\n")
	p("  echo \"spawn: S3 transfer client ${T:-default} ${G:+target ${G}Gb/s}\"\n")
	p("fi\n\n")
}
