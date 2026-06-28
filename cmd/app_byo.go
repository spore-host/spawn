package cmd

import (
	"regexp"
	"strings"

	"github.com/spore-host/libs/catalog"
)

// BYO-image catalog support (spore-host#392). An app is shown/launchable for the
// current account only if its image resolves: public images always; private-ECR
// images only when owned by the caller's account (the common BYO case — your own
// private registry). Cross-account private grants are possible but not assumed
// for listing; launch surfaces the real pull error if a grant is missing.

// ecrAccountRe extracts the 12-digit account ID from a private-ECR image host:
//
//	<account>.dkr.ecr.<region>.amazonaws.com/<repo>[:tag]
var ecrAccountRe = regexp.MustCompile(`^(\d{12})\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/`)

// ecrImageAccount returns the AWS account that owns a private-ECR image, or ""
// if the image isn't a private-ECR ref. Pure.
func ecrImageAccount(image string) string {
	m := ecrAccountRe.FindStringSubmatch(image)
	if m == nil {
		return ""
	}
	return m[1]
}

// appResolvable reports whether an app's image is resolvable (pullable) for the
// given caller account, and a short reason when not. callerAccount may be "" if
// the account couldn't be determined (e.g. no creds) — in that case private
// images are treated as not-resolvable (we can't prove ownership). Pure, so the
// list/launch filter is unit-tested without AWS.
func appResolvable(e *catalog.AppEntry, callerAccount string) (bool, string) {
	if !e.Containerized() {
		// Legacy launch_command app (baked AMI) — resolvability is the AMI's
		// concern, not an image pull; treat as resolvable here.
		return e.LaunchCommand != "", "no image or launch command"
	}
	if e.ImageVisibility() == catalog.VisibilityPublic {
		return true, ""
	}
	// Private image: resolvable only if owned by the caller's account.
	owner := ecrImageAccount(e.Image)
	if owner == "" {
		// Private by declaration but not a recognized private-ECR ref — can't
		// verify ownership cheaply; assume launch will attempt auth.
		return callerAccount != "", "private image, no AWS account resolved"
	}
	if callerAccount == "" {
		return false, "private image, no AWS account resolved"
	}
	if owner != callerAccount {
		return false, "private image owned by another account (" + owner + ")"
	}
	return true, ""
}

// ecrRegistryHost returns the registry host (everything before the first '/') of
// an image ref — the argument `docker login` expects. Pure.
func ecrRegistryHost(image string) string {
	if i := strings.IndexByte(image, '/'); i >= 0 {
		return image[:i]
	}
	return image
}

// splitImageRef splits a container ref into (image, tag). The tag is the part
// after a ':' in the LAST path segment (so a registry "host:port/repo" is not
// mistaken for a tag). Returns ("", "") inputs unchanged; tag "" if none. Pure.
func splitImageRef(ref string) (image, tag string) {
	lastSlash := strings.LastIndexByte(ref, '/')
	seg := ref[lastSlash+1:]
	if i := strings.LastIndexByte(seg, ':'); i >= 0 {
		return ref[:lastSlash+1] + seg[:i], seg[i+1:]
	}
	return ref, ""
}

// App list status values (#392).
const (
	appStatusLaunchable = "launchable" // image resolves for this account, or legacy launch_command
	appStatusRecipe     = "recipe"     // buildable definition — public recipe, no bound image
)

// classifyForList decides how an app appears in `spawn app list` for the caller
// account (#392): show=false hides it (a private image owned by another account
// you can't pull); otherwise status is "launchable" or "recipe". Pure.
func classifyForList(e *catalog.AppEntry, callerAccount string) (show bool, status string) {
	if e.RecipeOnly() {
		// A buildable definition: shown as a recipe regardless of account (the
		// recipe itself is public). It becomes launchable once a cake is bound.
		return true, appStatusRecipe
	}
	if ok, _ := appResolvable(e, callerAccount); ok {
		return true, appStatusLaunchable
	}
	return false, "" // private image this account can't pull → hidden
}
