//go:build !unix

package fault

import (
	"os/exec"
	"syscall"
)

// On non-unix platforms there is no euid concept; the platform gate in
// NetworkFault.Check already rejects injection before this is consulted.
func requireRoot() error { return nil }

func newSysProcAttr() *syscall.SysProcAttr { return nil }

func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// killPID is unreachable on non-unix: ProcessFault.Check rejects injection
// before this is consulted.
func killPID(pid int) error {
	return Errf("process kill is not supported on this platform")
}
