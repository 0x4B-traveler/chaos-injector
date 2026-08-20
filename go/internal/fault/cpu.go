package fault

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

func init() {
	Register("cpu", func() Fault { return &CpuFault{} })
}

// CpuFault burns CPU: stress-ng on Linux when available, otherwise pure Go
// busy-loop goroutines (works on any platform, including Windows).
type CpuFault struct {
	LoadPercent int
	Cores       int

	proc   *exec.Cmd
	cancel context.CancelFunc
}

func (f *CpuFault) Name() string        { return "cpu" }
func (f *CpuFault) Description() string { return "CPU load / resource exhaustion" }

func (f *CpuFault) Describe() string {
	return fmt.Sprintf("cpu(load_percent=%d, cores=%d)", f.LoadPercent, f.Cores)
}

func (f *CpuFault) Check() error {
	if f.LoadPercent < 1 || f.LoadPercent > 100 {
		return Errf("load_percent must be in [1, 100]")
	}
	if f.Cores < 1 {
		return Errf("cores must be >= 1")
	}
	return nil
}

func (f *CpuFault) Inject() error {
	if err := f.Check(); err != nil {
		return err
	}
	// Prefer stress-ng on Linux; fall back to pure Go burners elsewhere.
	if runtime.GOOS == "linux" {
		if bin, err := exec.LookPath("stress-ng"); err == nil {
			cmd := exec.Command(
				bin, "--cpu", strconv.Itoa(f.Cores),
				"--cpu-load", strconv.Itoa(f.LoadPercent), "--quiet",
			)
			cmd.SysProcAttr = newSysProcAttr()
			if err := cmd.Start(); err == nil {
				f.proc = cmd
				return nil
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	for range f.Cores {
		go burnCPU(ctx, f.LoadPercent)
	}
	f.cancel = cancel
	return nil
}

func (f *CpuFault) Recover() error {
	if f.proc != nil {
		killProcessTree(f.proc)
		f.proc = nil
	}
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	return nil
}

// burnCPU busy-loops a single core with a duty cycle approximating load%.
func burnCPU(ctx context.Context, loadPercent int) {
	const window = 200 * time.Millisecond
	duty := time.Duration(loadPercent) * window / 100
	rest := window - duty
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		burnUntil := time.Now().Add(duty)
		for time.Now().Before(burnUntil) {
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(rest):
		}
	}
}
