// Package launchererr holds launch-failure sentinel errors in a dependency-free
// leaf: it imports only the standard library, no AWS SDK. A downstream that only
// needs to classify a launch error — e.g. a capacity-retry loop keyed on whether
// a failure is post-launch (terminal) — can import this package without pulling
// the launcher's AWS SDK dependency tree (spawn#354).
//
// pkg/launcher aliases these so existing callers and errors.Is semantics are
// unchanged; the sentinel's identity lives here.
package launchererr

import "errors"

// ErrPostLaunch marks a failure that happened AFTER RunInstances already
// succeeded (e.g. ephemeral FSx setup). The instance was launched and has since
// been torn down by the launcher (#220). Callers that retry across AZs/regions
// should treat a post-launch failure as terminal: the launch itself worked, so
// retrying won't help and would just churn launch+terminate cycles. Test with
// errors.Is(err, ErrPostLaunch).
var ErrPostLaunch = errors.New("post-launch provisioning failure")
