package service

import (
	"os"
	"os/exec"
)

// Windows has no POSIX process groups. These exist only so the package's tests
// compile for GOOS=windows (#925): fakeSSHExec, their one caller, skips on
// Windows before it can reach them.

func ownProcessGroup(*exec.Cmd) {}

func killProcessGroup(p *os.Process) { _ = p.Kill() }
