package fault

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

func init() {
	Register("process", func() Fault { return &ProcessFault{} })
}

// ProcessFault kills processes whose name or command line contains Pattern
// (chaosblade "process kill" equivalent). It is an instantaneous fault: the
// killed processes cannot be brought back, so Recover is a no-op.
//
// Safety boundary: the current process and its ancestors are never matched,
// so the injector cannot kill itself or its parent shell.
type ProcessFault struct {
	Pattern string

	targets []int
}

func (f *ProcessFault) Name() string        { return "process" }
func (f *ProcessFault) Description() string { return "terminate processes matching a pattern" }

func (f *ProcessFault) Describe() string {
	return fmt.Sprintf("process(pattern=%q)", f.Pattern)
}

func (f *ProcessFault) Check() error {
	if runtime.GOOS != "linux" {
		return Errf("ProcessFault requires Linux (/proc scan); not available on %s", runtime.GOOS)
	}
	if strings.TrimSpace(f.Pattern) == "" {
		return Errf("pattern must not be empty")
	}
	targets, err := matchProcesses(f.Pattern)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return Errf("no process matches pattern %q", f.Pattern)
	}
	f.targets = targets
	return nil
}

func (f *ProcessFault) Inject() error {
	if err := f.Check(); err != nil {
		return err
	}
	killed := 0
	for _, pid := range f.targets {
		if err := killPID(pid); err == nil {
			killed++
		}
	}
	if killed == 0 {
		return Errf("no matching process could be killed (already gone?)")
	}
	return nil
}

// Recover is idempotent by design: an instantaneous kill has nothing to undo.
func (f *ProcessFault) Recover() error { return nil }

// matchProcesses scans /proc for pids whose comm or cmdline contains pattern,
// excluding the current process and its ancestor chain.
func matchProcesses(pattern string) ([]int, error) {
	self := protectedPIDs()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, Errf("cannot scan /proc: %v", err)
	}
	var targets []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if self[pid] {
			continue
		}
		if processMatches(pid, pattern) {
			targets = append(targets, pid)
		}
	}
	return targets, nil
}

// protectedPIDs returns the set of pids that must never be targeted: this
// process and its ancestors (walked via /proc/<pid>/status PPid).
func protectedPIDs() map[int]bool {
	prot := map[int]bool{}
	pid := os.Getpid()
	for pid > 1 {
		prot[pid] = true
		ppid := parentPID(pid)
		if ppid == pid || ppid < 1 {
			break
		}
		pid = ppid
	}
	return prot
}

func parentPID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				if ppid, err := strconv.Atoi(fields[1]); err == nil {
					return ppid
				}
			}
			return -1
		}
	}
	return -1
}

func processMatches(pid int, pattern string) bool {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err == nil && strings.Contains(strings.TrimSpace(string(comm)), pattern) {
		return true
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err == nil && strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), pattern) {
		return true
	}
	return false
}
