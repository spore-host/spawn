package userdata

import (
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// The EFA installer that --efa runs as root (spawn#1505): a pinned version and
// the sha256 of its tarball, the one Go source of truth for both the MPI user
// data and the release and bump tooling (scripts/efapin reads and rewrites these
// two constants with go/parser).
//
// Until #1505 the user data ran whatever https://efa-installer.amazonaws.com/
// aws-efa-installer-latest.tar.gz served at boot, unchecked. Now it fetches
// aws-efa-installer-<efaInstallerVersion>.tar.gz and refuses to unpack bytes
// whose sha256 is not efaInstallerSHA256, failing the member closed (#1483).
//
// The signature is checked at pin time, not on the instance, exactly as for the
// AWS CLI (#1495): scripts/efa-pin.sh verifies the tarball's detached .sig with
// gpg against AWS's EFA key, committed as scripts/efapin/aws-efa-installer.key
// (fingerprint 4E9091BCBB97A96B26B15E59A05480B1DD2D3CCC, as
// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-start.html gives it),
// before it pins a sha256. The weekly .github/workflows/efa-pin.yml moves the
// pin by PR, and the release workflow re-verifies the pinned tarball's
// signature and sha256 before it builds anything.
//
// Amazon Linux 2 (spawn#1504): the installer dropped AL2 in 1.50.0, so this pin
// cannot install there, and the user data refuses --efa on AL2 with a message
// rather than pinning a second, older installer (1.49.0) for one distribution.
const (
	efaInstallerVersion = "1.51.0"
	efaInstallerSHA256  = "83993044fa4df3dc2f8b5c2f7051d61b9689441be023a2c92ea51d3f53f2465c"
)

// efaInstallerOrigin is where AWS publishes the installer.
const efaInstallerOrigin = "https://efa-installer.amazonaws.com/"

// efaInstallerMirrorPrefix is where the pinned tarball is mirrored, under its
// own file name, in each spawn-binaries-<region> bucket. The user data tries
// the launch region's copy first, so a --no-public-ip member, which reaches S3
// through the endpoint that flag requires but not efa-installer.amazonaws.com,
// can still install it; the sha256 check applies to either source.
const efaInstallerMirrorPrefix = "spawn/efa/"

// efaInstallerFile is the pinned tarball's file name, the same at AWS and in
// the mirror.
func efaInstallerFile() string { return "aws-efa-installer-" + efaInstallerVersion + ".tar.gz" }

// efaInstallerSources are the URLs the user data tries, in order: the launch
// region's mirror, when that region has a spawn-binaries bucket, then AWS.
// Each is shell-quoted for the script.
func efaInstallerSources(region string) []string {
	var urls []string
	if sporedinstall.Published(region) {
		urls = append(urls, "https://spawn-binaries-"+region+".s3."+region+".amazonaws.com/"+efaInstallerMirrorPrefix+efaInstallerFile())
	}
	urls = append(urls, efaInstallerOrigin+efaInstallerFile())
	for i, u := range urls {
		urls[i] = security.ShellQuote(u)
	}
	return urls
}
