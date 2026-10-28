package main

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	smithy "github.com/aws/smithy-go"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// A legacy member's missing spored-instance-profile (#1369).
//
// A legacy member (legacy.go, #1368) with no iam_role launches with
// spored-instance-profile. A v0.146.0-or-older CLI never ensured that profile
// on the detached path, so an account that has only run detached sweeps, or
// only schedules, may not have it. RunInstances then rejects the member with
//
//	InvalidParameterValue: Value (spored-instance-profile) for parameter
//	iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name
//
// the shape pkg/aws already recognises for a just-created profile that has
// not reached EC2 yet (isTransientIAMInstanceProfilePropagation, #572). The
// same message, with "ARN", is EC2's answer for a profile named by ARN.
//
// Every other legacy member with no iam_role names the same profile in the
// same account, so each would fail the same way. The sweep stops at the first:
// its members not yet attempted are not attempted, and the sweep is FAILED
// with an error that names the remedy. It is not a capacity error, so the
// member's other instance types are not tried either.
//
// The remedy is a current spawn, with the user's own credentials, because
// this Lambda will not create IAM in a member account and its cross-account
// role cannot: re-running the sweep (launchSweepDetached) or re-creating the
// schedule (seedScheduleParams) each calls ensureDetachedInstanceProfile,
// which creates spored-instance-profile when it is missing, before anything is
// uploaded. Neither launches anything the user did not ask for: `spawn
// schedule create` launches nothing itself. `spawn resume --sweep-id --detach`
// is not named: it ensures no profile, and this Lambda does not resume a FAILED
// sweep. Nor is spawnd-role a fallback: it cannot write spored's heartbeat, so
// #1313 would terminate the member five minutes after launch.
//
// A member whose params a current CLI rendered keeps the ordinary non-capacity
// behaviour: its row fails with EC2's error and the sweep's other members are
// still attempted. That CLI ensured the profile moments before it uploaded
// them, so the error there is propagation or a deletion since, not this gap,
// and this remedy (re-run with a current spawn) is what the user just did.

// sporedProfileFixedIn is the first spawn release whose detached sweeps and
// schedules ensure spored-instance-profile (#1323).
const sporedProfileFixedIn = "v0.147.0"

// legacyProfileError is a legacy member that RunInstances refused because
// spored-instance-profile does not exist in the member account, or is not yet
// usable there.
type legacyProfileError struct {
	paramIndex int
	remedy     string
	err        error
}

func (e *legacyProfileError) Error() string {
	return fmt.Sprintf("param set %d: this account has no usable %s, which a member from an older spawn's params "+
		"launches with when it names no iam_role (#1369): %s. EC2 said: %v",
		e.paramIndex, spawnaws.SporedInstanceProfileName, e.remedy, e.err)
}

func (e *legacyProfileError) Unwrap() error { return e.err }

// isInvalidInstanceProfile reports whether err is RunInstances rejecting the
// request's instance profile: InvalidParameterValue whose message says the IAM
// instance profile name (or ARN) is invalid. Narrow on purpose: any other
// InvalidParameterValue (an instance type, an AMI) is not this.
func isInvalidInstanceProfile(err error) bool {
	const wantMsg = "Invalid IAM Instance Profile"
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidParameterValue" &&
			(strings.Contains(apiErr.ErrorMessage(), wantMsg) || strings.Contains(err.Error(), wantMsg))
	}
	return false
}

// isLegacyParams reports whether a member's params, as uploaded, are legacy
// params: no bootstrap and no tags (withMemberBootstrap).
func isLegacyParams(config map[string]interface{}) bool {
	return getStringParam(config, userDataParam, "") == "" && len(getMapParam(config, tagsParam)) == 0
}

// classifyLaunchError returns a RunInstances error as the member's error: a
// *legacyProfileError for a legacy member refused for spored-instance-profile,
// and err unchanged otherwise.
func classifyLaunchError(state *SweepRecord, config map[string]interface{}, input *ec2.RunInstancesInput, paramIndex int, err error) error {
	if !isLegacyParams(config) || input.IamInstanceProfile == nil ||
		aws.ToString(input.IamInstanceProfile.Name) != spawnaws.SporedInstanceProfileName ||
		!isInvalidInstanceProfile(err) {
		return err
	}
	return &legacyProfileError{paramIndex: paramIndex, remedy: legacyProfileRemedy(state), err: err}
}

// legacyProfileRemedy is what the user runs: a current spawn ensures the
// profile with their credentials, as it re-runs the sweep or re-creates the
// schedule.
func legacyProfileRemedy(state *SweepRecord) string {
	ensures := "it creates " + spawnaws.SporedInstanceProfileName + " with your credentials if it is missing, before it uploads anything"
	if state.Source == "scheduled" && state.ScheduleID != "" {
		return fmt.Sprintf("re-create the schedule with spawn %s or later: `spawn schedule cancel %s`, then "+
			"`spawn schedule create <params-file>` with its --cron or --at; %s", sporedProfileFixedIn, state.ScheduleID, ensures)
	}
	return fmt.Sprintf("re-run the sweep with spawn %s or later: `spawn launch --param-file <params-file>`; %s",
		sporedProfileFixedIn, ensures)
}

// abortForLegacyProfile stops a sweep at a legacy member's profile error: the
// members not yet attempted are not attempted, and the sweep's error names the
// remedy. notAttempted is how many members that leaves unlaunched. It reports
// whether err was such an error; the caller drains its own queue.
func abortForLegacyProfile(state *SweepRecord, err error, notAttempted int) bool {
	var lpe *legacyProfileError
	if !errors.As(err, &lpe) {
		return false
	}
	state.ErrorMessage = fmt.Sprintf("Stopped at param set %d: this account has no usable %s, which members from an "+
		"older spawn's params launch with when they name no iam_role, so every such member would fail the same way "+
		"and the %d not yet attempted were not attempted (#1369). To fix it, %s",
		lpe.paramIndex, spawnaws.SporedInstanceProfileName, notAttempted, lpe.remedy)
	log.Printf("Sweep %s: %s", state.SweepID, state.ErrorMessage)
	return true
}

// abortedStatus is a finished sweep's status: FAILED if it was stopped by
// abortForLegacyProfile, whatever it launched before, and completionStatus's
// decision otherwise.
func abortedStatus(state *SweepRecord, status string) string {
	if strings.HasPrefix(state.ErrorMessage, "Stopped at param set ") {
		return "FAILED"
	}
	return status
}
