package fault

import (
	"strings"
	"testing"
	"time"

	"chaos-injector/internal/k8s"
)

// fakeClient builds a Client whose runner dispatches on the first argument.
func fakeClient(respond func(args ...string) (string, error)) *k8s.Client {
	return &k8s.Client{Run: respond}
}

func TestPodKillParameterValidation(t *testing.T) {
	// Both pod and selector set.
	f := &PodKillFault{Namespace: "default", Pod: "p1", Selector: "app=x"}
	if err := f.Check(); err == nil {
		t.Fatal("expected error when both pod and selector are set")
	}
	// Neither set.
	f = &PodKillFault{Namespace: "default"}
	if err := f.Check(); err == nil {
		t.Fatal("expected error when neither pod nor selector is set")
	}
	// Empty namespace.
	f = &PodKillFault{Pod: "p1"}
	if err := f.Check(); err == nil {
		t.Fatal("expected error on empty namespace")
	}
	// wait-ready requires a selector.
	f = &PodKillFault{Namespace: "default", Pod: "p1", WaitReady: time.Second}
	if err := f.Check(); err == nil {
		t.Fatal("expected error when wait-ready is used without a selector")
	}
	// Selector matches nothing.
	f = &PodKillFault{Namespace: "default", Selector: "app=none"}
	f.client = fakeClient(func(args ...string) (string, error) { return "", nil })
	if err := f.Check(); err == nil {
		t.Fatal("expected error when selector matches no pod")
	}
}

func TestPodKillCheckBySelectorCapsCount(t *testing.T) {
	f := &PodKillFault{Namespace: "default", Selector: "app=demo", Count: 2}
	f.client = fakeClient(func(args ...string) (string, error) {
		return "pod-a pod-b pod-c", nil
	})
	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(f.targets) != 2 || f.targets[0] != "pod-a" {
		t.Fatalf("targets = %v, want [pod-a pod-b]", f.targets)
	}
}

func TestPodKillCheckByPodName(t *testing.T) {
	f := &PodKillFault{Namespace: "default", Pod: "nginx-abc"}
	f.client = fakeClient(func(args ...string) (string, error) {
		return "pod/nginx-abc", nil
	})
	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(f.targets) != 1 || f.targets[0] != "nginx-abc" {
		t.Fatalf("targets = %v", f.targets)
	}
	// Pod does not exist.
	f.client = fakeClient(func(args ...string) (string, error) {
		return "pod/nginx-abc", nil
	})
	f.Pod = "nginx-xyz"
	f.targets = nil
	if err := f.Check(); err == nil {
		t.Fatal("expected error for missing pod")
	}
}

func TestPodKillInjectDeletesAndObservesHealing(t *testing.T) {
	var deleted []string
	f := &PodKillFault{
		Namespace: "default",
		Selector:  "app=demo",
		WaitReady: 5 * time.Second,
	}
	f.client = fakeClient(func(args ...string) (string, error) {
		switch args[0] {
		case "delete":
			deleted = append(deleted, args[4])
			return "", nil
		case "get":
			if args[1] == "pods" {
				return "old-pod new-pod", nil
			}
			return "True", nil // new-pod is Ready
		}
		return "", nil
	})
	if err := f.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "old-pod" {
		t.Fatalf("deleted = %v, want [old-pod]", deleted)
	}
	if f.Healed() != "new-pod" {
		t.Fatalf("Healed = %q, want new-pod", f.Healed())
	}
}

func TestPodKillRecoverIsNoOp(t *testing.T) {
	f := &PodKillFault{Namespace: "default", Selector: "app=demo"}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover must be a no-op: %v", err)
	}
}

func TestPodKillDescribe(t *testing.T) {
	f := &PodKillFault{Namespace: "default", Selector: "app=demo", Count: 1, WaitReady: 30 * time.Second}
	if !strings.Contains(f.Describe(), "selector=\"app=demo\"") {
		t.Fatalf("Describe missing selector: %s", f.Describe())
	}
}
