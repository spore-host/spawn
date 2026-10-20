package cmd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spore-host/libs/catalog"
	"github.com/spore-host/spawn/pkg/ecrref"
)

// BYO-image catalog support (spore-host#392). An app is shown/launchable for the
// current account only if its image resolves: public images always; private-ECR
// images only when owned by the caller's account (the common BYO case — your own
// private registry). A private-ECR image owned by another account is hidden
// and refused at launch even if that account granted pull access: ownership is
// the one thing checkable without AWS calls, and a launch whose pull fails
// would leave a doomed, billing instance (#592). docs/catalog-schema.md's
// visibility row says the same (#1153).

// ecrImageAccount returns the AWS account that owns a private-ECR image, or ""
// if the image isn't a private-ECR ref. Pure. Thin alias over pkg/ecrref, kept
// so callers in this file read the same as before.
func ecrImageAccount(image string) string { return ecrref.Account(image) }

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
// an image ref — the argument `docker login` expects. Pure. Thin alias over
// pkg/ecrref.
func ecrRegistryHost(image string) string { return ecrref.RegistryHost(image) }

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

// byoImageTag splits an --image ref into the image and the tag to launch: the
// ref's own tag, else --app-version, else "latest" — what `docker pull` of a
// bare ref means (#1153).
func byoImageTag(ref, version string) (image, tag string) {
	image, tag = splitImageRef(ref)
	switch {
	case tag != "":
	case version != "":
		tag = version
	default:
		tag = "latest"
	}
	return image, tag
}

// adhocAppName is what an app name not in the catalog must look like: it
// becomes the start of the session's DNS name.
var adhocAppName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// adhocWebEntry is the catalog entry for `spawn app launch <name> --image <ref>
// --web-port <n>` when <name> is not in the catalog: a web app with no catalog
// hardware spec, so --instance-type is required rather than guessed. The
// bring-your-own-app guide and the README document this form as needing no
// catalog entry; it used to fail "not found in catalog" (#1153).
func adhocWebEntry(name, image string, webPort int, instanceType string) (*catalog.AppEntry, error) {
	if image == "" || webPort <= 0 {
		return nil, fmt.Errorf("application %q not found in catalog — run 'spawn app list' to see available apps, "+
			"or launch your own web app image with --image <ref> --web-port <port> --instance-type <type>", name)
	}
	if !adhocAppName.MatchString(name) {
		return nil, fmt.Errorf("app name %q must be lowercase letters, digits and hyphens (at most 40), since it names the session's DNS record", name)
	}
	if instanceType == "" {
		return nil, fmt.Errorf("%q is not in the catalog, so it has no default hardware: pass --instance-type (e.g. m7i.large)", name)
	}
	return &catalog.AppEntry{
		Name:        name,
		Description: "your web app (" + image + ")",
	}, nil
}

// App list status values (#392).
const (
	appStatusLaunchable = "launchable" // image resolves for this account (or a desktop)
	appStatusRecipe     = "recipe"     // buildable definition — public recipe, no bound image
)

// classifyForList decides how an app appears in `spawn app list` for the caller
// account (#392): show=false hides it (a private image owned by another account
// you can't pull); otherwise status is "launchable" or "recipe". Pure.
func classifyForList(e *catalog.AppEntry, callerAccount string) (show bool, status string) {
	if e.Kind() == catalog.KindDesktop {
		// A bare desktop needs no image — always launchable (#591).
		return true, appStatusLaunchable
	}
	if e.RecipeOnly() {
		// A buildable definition: shown as a recipe regardless of account (the
		// recipe itself is public). It becomes launchable once a cake is bound.
		return true, appStatusRecipe
	}
	if e.Image == "" {
		// No container image (desktop handled above). A bare legacy launch_command
		// relied on the retired baked-AMI model (#389) and can't launch now — it's a
		// definition, not launchable. Bind an image via overlay or --image to launch.
		return true, appStatusRecipe
	}
	if ok, _ := appResolvable(e, callerAccount); ok {
		return true, appStatusLaunchable
	}
	return false, "" // private image this account can't pull → hidden
}
