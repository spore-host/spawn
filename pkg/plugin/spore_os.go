package plugin

import (
	"errors"
	"strings"
)

// ErrWindowsSporeUnsupported is returned when a plugin would be installed on a
// Windows spore. Plugins are Linux-only (#1001):
//
//   - a "run" step is a POSIX shell script (spored runs it with sh -c, or
//     su - <user> -c for as_user) and the registry's scripts use systemctl,
//     apt/dnf and curl | sh;
//   - "extract" is tar, and "background" is nohup;
//   - as_user means the instance's local matching user, which a Windows spore
//     does not have (you log in as Administrator; see spore-plugins CONCEPTS.md);
//   - every plugin in spore-host/spore-plugins declares
//     conditions.remote: [{type: platform, os: linux}].
//
// A PowerShell runner would need every plugin rewritten for Windows, so the
// runtime refuses rather than failing at the first step with "sh not found".
var ErrWindowsSporeUnsupported = errors.New("plugins are not supported on Windows spores: " +
	"plugin steps are POSIX shell (sh, su, tar) and every registry plugin requires a Linux instance (#1001)")

// CheckSporeOS returns ErrWindowsSporeUnsupported when goos (a GOOS value or a
// spawn:os tag value; "" means Linux) names Windows, and nil otherwise.
func CheckSporeOS(goos string) error {
	if strings.EqualFold(strings.TrimSpace(goos), "windows") {
		return ErrWindowsSporeUnsupported
	}
	return nil
}
