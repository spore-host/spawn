//go:build !windows

package service

import (
	"os"
	"os/exec"
	"syscall"
)

// ownProcessGroup puts cmd in a new process group, so killProcessGroup can
// reach everything it backgrounds. See fakeSSHExec for why that matters.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs p's whole process group: a negative pid names the
// group, i.e. the shell and every descendant it backgrounded.
func killProcessGroup(p *os.Process) {
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}
