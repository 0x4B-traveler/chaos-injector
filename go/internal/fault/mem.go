package fault

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

func init() {
	Register("mem", func() Fault { return &MemFault{} })
}

// MemFault occupies memory (chaosblade "mem load" equivalent): allocates a
// buffer of SizeMB and keeps touching pages so they stay resident. Recovery
// releases the buffer and lets the GC reclaim it.
type MemFault struct {
	SizeMB int

	buf    []byte
	cancel context.CancelFunc
}

func (f *MemFault) Name() string        { return "mem" }
func (f *MemFault) Description() string { return "memory occupancy / resource exhaustion" }

func (f *MemFault) Describe() string {
	return fmt.Sprintf("mem(size_mb=%d)", f.SizeMB)
}

func (f *MemFault) Check() error {
	if f.SizeMB < 1 {
		return Errf("size_mb must be >= 1")
	}
	// Safety gate (Linux only): never allocate beyond what the host can
	// spare, or the kernel OOM killer may pick this very process.
	if runtime.GOOS == "linux" {
		avail, err := MemAvailableMB()
		if err != nil {
			return err
		}
		if int64(f.SizeMB) > avail {
			return Errf("size_mb=%d exceeds available memory (%d MB)", f.SizeMB, avail)
		}
	}
	return nil
}

func (f *MemFault) Inject() error {
	if err := f.Check(); err != nil {
		return err
	}
	f.buf = make([]byte, f.SizeMB*1024*1024)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go touchPages(ctx, f.buf)
	return nil
}

func (f *MemFault) Recover() error {
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	f.buf = nil // release for the GC
	return nil
}

// touchPages writes one byte per page so the kernel actually backs the
// allocation with physical memory (dirty pages stay resident).
func touchPages(ctx context.Context, buf []byte) {
	const pageSize = 4096
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		for i := 0; i < len(buf); i += pageSize {
			buf[i] = byte(i)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
