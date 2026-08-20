package fault

import (
	"os"
	"testing"
)

func TestRegistryContainsCoreAtoms(t *testing.T) {
	for _, name := range []string{"network", "cpu", "mem", "process", "port", "pod-kill", "mysql"} {
		if _, ok := Registry[name]; !ok {
			t.Fatalf("registry missing %q", name)
		}
	}
}

func TestNetworkFaultParameterValidation(t *testing.T) {
	cases := []*NetworkFault{
		{DelayMS: -1},
		{LossPct: 101},
		{DelayMS: 0, LossPct: 0},
	}
	for i, f := range cases {
		if err := f.Check(); err == nil {
			t.Errorf("case %d (%+v): expected validation error", i, f)
		}
	}
}

func TestNetworkRecoverIsSafeOnNonLinux(t *testing.T) {
	// recover() must not fail on platforms where tc never existed.
	f := &NetworkFault{Interface: "eth0", DelayMS: 200}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover on non-Linux must not fail: %v", err)
	}
}

func TestCpuFaultParameterValidation(t *testing.T) {
	cases := []*CpuFault{{LoadPercent: 0}, {LoadPercent: 101}, {Cores: 0}}
	for i, f := range cases {
		if err := f.Check(); err == nil {
			t.Errorf("case %d (%+v): expected validation error", i, f)
		}
	}
}

func TestCpuFaultInjectAndRollback(t *testing.T) {
	f := &CpuFault{LoadPercent: 30, Cores: 1}
	if err := f.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if f.cancel == nil && f.proc == nil {
		t.Fatal("no burner started")
	}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if f.cancel != nil || f.proc != nil {
		t.Fatal("burner handles not cleaned up after Recover")
	}
}

func TestCpuFaultRecoverWithoutInjectIsSafe(t *testing.T) {
	f := &CpuFault{LoadPercent: 50, Cores: 1}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover without Inject must not fail: %v", err)
	}
}

func TestMemFaultParameterValidation(t *testing.T) {
	f := &MemFault{SizeMB: 0}
	if err := f.Check(); err == nil {
		t.Fatal("expected validation error for size_mb=0")
	}
}

func TestMemFaultInjectAndRollback(t *testing.T) {
	f := &MemFault{SizeMB: 1}
	if err := f.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if f.buf == nil || len(f.buf) != 1024*1024 {
		t.Fatalf("buffer not allocated (len=%d)", len(f.buf))
	}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if f.buf != nil || f.cancel != nil {
		t.Fatal("buffer/cancel not cleaned up after Recover")
	}
}

func TestMemFaultRecoverWithoutInjectIsSafe(t *testing.T) {
	f := &MemFault{SizeMB: 8}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover without Inject must not fail: %v", err)
	}
}

func TestPortFaultParameterValidation(t *testing.T) {
	for _, port := range []int{0, 65536} {
		f := &PortFault{Port: port}
		if err := f.Check(); err == nil {
			t.Errorf("port %d: expected validation error", port)
		}
	}
}

func TestPortFaultInjectAndRollback(t *testing.T) {
	f := &PortFault{Port: 18080}
	if err := f.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if f.ln == nil {
		t.Fatal("listener not created")
	}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if f.ln != nil {
		t.Fatal("listener not closed after Recover")
	}
	// The port must be reusable right after recovery.
	if err := f.Inject(); err != nil {
		t.Fatalf("re-inject after Recover must work: %v", err)
	}
	_ = f.Recover()
}

func TestPortFaultRecoverWithoutInjectIsSafe(t *testing.T) {
	f := &PortFault{Port: 18081}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover without Inject must not fail: %v", err)
	}
}

func TestProcessFaultPlatformGate(t *testing.T) {
	// On non-Linux the fault must be rejected at Check time; on Linux an
	// empty pattern must still fail validation.
	f := &ProcessFault{Pattern: ""}
	if err := f.Check(); err == nil {
		t.Fatal("expected error for empty pattern")
	}
	f = &ProcessFault{Pattern: "definitely-no-such-process-name-xyz"}
	if err := f.Check(); err == nil {
		t.Fatal("expected error when no process matches")
	}
}

func TestProtectedPIDsIncludesSelf(t *testing.T) {
	prot := protectedPIDs()
	if !prot[os.Getpid()] {
		t.Fatal("own pid must be protected from process kill")
	}
}
