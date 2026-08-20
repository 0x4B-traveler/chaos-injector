package fault

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// LoadGate implements the environment awareness layer (环境感知层):
// refuse to inject into a host that is already under load. Linux reads
// /proc/loadavg; on other platforms the gate is skipped.
func LoadGate(limit float64) (ok bool, detail string) {
	if runtime.GOOS != "linux" {
		return true, "load gate skipped on this platform"
	}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return true, "load check unavailable"
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return true, "load check unavailable"
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return true, "load check unavailable"
	}
	return load1 < limit, fmt.Sprintf("loadavg(1m)=%.2f limit=%.1f", load1, limit)
}

// Snapshot is a lightweight system-state capture used as reproducibility
// evidence: recorded before injection and after rollback, so two runs of the
// same experiment can be compared (same seed + same environment = stable
// reproduction). Fields unavailable on the current platform are omitted.
// Field names mirror the Python implementation's snapshot_env().
type Snapshot struct {
	Ts             string    `json:"ts"`
	Hostname       string    `json:"hostname"`
	Platform       string    `json:"platform"`
	CPUCount       int       `json:"cpu_count"`
	LoadAvg        []float64 `json:"loadavg,omitempty"`
	MemAvailableMB int64     `json:"mem_available_mb,omitempty"`
}

// Capture records the current system state (Linux adds loadavg and
// available memory; other platforms get the basic fields only).
func Capture() Snapshot {
	s := Snapshot{
		Ts:       time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Platform: runtime.GOOS,
		CPUCount: runtime.NumCPU(),
	}
	if h, err := os.Hostname(); err == nil {
		s.Hostname = h
	}
	if runtime.GOOS == "linux" {
		if avg, ok := loadAvg(); ok {
			s.LoadAvg = avg
		}
		if avail, err := MemAvailableMB(); err == nil {
			s.MemAvailableMB = avail
		}
	}
	return s
}

// loadAvg reads the 1/5/15-minute load averages on Linux.
func loadAvg() ([]float64, bool) {
	if runtime.GOOS != "linux" {
		return nil, false
	}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return nil, false
	}
	out := make([]float64, 3)
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil, false
		}
		out[i] = math.Round(v*100) / 100
	}
	return out, true
}

// MemAvailableMB returns free memory in MB from /proc/meminfo (Linux only),
// used by the random-decision layer to size memory faults safely.
func MemAvailableMB() (int64, error) {
	if runtime.GOOS != "linux" {
		return 0, Errf("meminfo is only readable on Linux")
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb / 1024, nil
		}
	}
	return 0, Errf("MemAvailable not found in /proc/meminfo")
}
