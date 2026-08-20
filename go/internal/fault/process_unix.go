//go:build unix

package fault

import (
	"os/exec"
	"syscall"
)

// newSysProcAttr places stress-ng in its own process group so recovery can
// kill the whole tree, not just the parent.
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	_ = cmd.Wait()
}

// killPID sends SIGKILL to a single process (ProcessFault).
func killPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
