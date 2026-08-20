package fault

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func init() {
	Register("network", func() Fault { return &NetworkFault{} })
}

// NetworkFault injects network delay / packet loss via Linux tc netem.
type NetworkFault struct {
	Interface string
	DelayMS   int
	LossPct   float64
}

func (f *NetworkFault) Name() string        { return "network" }
func (f *NetworkFault) Description() string { return "network delay / packet loss (Linux tc netem)" }

func (f *NetworkFault) Describe() string {
	return fmt.Sprintf("network(interface=%s, delay_ms=%d, loss_pct=%.1f)", f.Interface, f.DelayMS, f.LossPct)
}

func (f *NetworkFault) Check() error {
	if runtime.GOOS != "linux" {
		return Errf("NetworkFault requires Linux + tc; not available on %s", runtime.GOOS)
	}
	if _, err := exec.LookPath("tc"); err != nil {
		return Errf("tc not found in PATH (iproute2 required)")
	}
	if err := requireRoot(); err != nil {
		return err
	}
	if f.DelayMS < 0 {
		return Errf("delay_ms must be >= 0")
	}
	if f.LossPct < 0 || f.LossPct > 100 {
		return Errf("loss_pct must be in [0, 100]")
	}
	if f.DelayMS == 0 && f.LossPct == 0 {
		return Errf("at least one of delay/loss must be set")
	}
	return nil
}

func (f *NetworkFault) Inject() error {
	if err := f.Check(); err != nil {
		return err
	}
	args := []string{"qdisc", "replace", "dev", f.Interface, "root", "handle", "1:", "netem"}
	if f.DelayMS > 0 {
		args = append(args, "delay", fmt.Sprintf("%dms", f.DelayMS))
	}
	if f.LossPct > 0 {
		args = append(args, "loss", fmt.Sprintf("%g%%", f.LossPct))
	}
	out, err := exec.Command("tc", args...).CombinedOutput()
	if err != nil {
		return Errf("inject network fault failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (f *NetworkFault) Recover() error {
	if runtime.GOOS != "linux" {
		return nil // injection could not have happened here; nothing to roll back
	}
	if _, err := exec.LookPath("tc"); err != nil {
		return nil
	}
	out, err := exec.Command("tc", "qdisc", "del", "dev", f.Interface, "root").CombinedOutput()
	if err != nil {
		// rc=2 means the qdisc was already gone: treat as success (idempotent).
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
			return nil
		}
		return Errf("recover network fault failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
