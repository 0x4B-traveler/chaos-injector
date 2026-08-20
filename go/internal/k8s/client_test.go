package k8s

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var errFake = errors.New("fake kubectl failure")

// fakeRunner builds a Runner with scripted per-command responses.
func fakeRunner(respond func(args ...string) (string, error)) Runner {
	return respond
}

func TestGetPodNamesParsing(t *testing.T) {
	c := &Client{Run: fakeRunner(func(args ...string) (string, error) {
		return "pod-a pod-b pod-c", nil
	})}
	names, err := c.GetPodNames("default", "app=demo")
	if err != nil {
		t.Fatalf("GetPodNames: %v", err)
	}
	if len(names) != 3 || names[0] != "pod-a" || names[2] != "pod-c" {
		t.Fatalf("unexpected names: %v", names)
	}
}

func TestGetPodNamesEmpty(t *testing.T) {
	c := &Client{Run: fakeRunner(func(args ...string) (string, error) {
		return "", nil
	})}
	names, err := c.GetPodNames("default", "app=none")
	if err != nil {
		t.Fatalf("GetPodNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no pods, got %v", names)
	}
}

func TestGetPodNamesPropagatesError(t *testing.T) {
	c := &Client{Run: fakeRunner(func(args ...string) (string, error) {
		return "server timeout", errFake
	})}
	if _, err := c.GetPodNames("default", "app=demo"); err == nil {
		t.Fatal("expected error from kubectl failure")
	}
}

func TestPodExists(t *testing.T) {
	c := &Client{Run: fakeRunner(func(args ...string) (string, error) {
		return "pod/nginx-abc", nil
	})}
	ok, err := c.PodExists("default", "nginx-abc")
	if err != nil {
		t.Fatalf("PodExists: %v", err)
	}
	if !ok {
		t.Fatal("expected pod to exist")
	}
}

func TestDeletePodArgs(t *testing.T) {
	var got []string
	c := &Client{Run: fakeRunner(func(args ...string) (string, error) {
		got = append(got, args...)
		return "", nil
	})}
	if err := c.DeletePod("prod", "pod-1"); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	want := "delete pod -n prod pod-1 --wait=false"
	if strings.Join(got, " ") != want {
		t.Fatalf("delete args = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestWaitForReplacement(t *testing.T) {
	// First poll: deleted pod still listed and Ready (Terminating), new pod
	// not ready. Second poll: new pod becomes Ready.
	polls := 0
	c := &Client{Run: fakeRunner(func(args ...string) (string, error) {
		switch {
		case args[1] == "pods": // GetPodNames
			return "old-pod new-pod", nil
		case args[1] == "pod": // podReady
			name := args[4]
			if name == "old-pod" {
				return "False", nil // deleted pod never counts
			}
			if polls < 2 {
				return "False", nil
			}
			return "True", nil
		}
		return "", errFake
	})}
	// Advance polls after each GetPodNames call.
	base := c.Run
	c.Run = fakeRunner(func(args ...string) (string, error) {
		out, err := base(args...)
		if args[1] == "pods" {
			polls++
		}
		return out, err
	})

	start := time.Now()
	healed, err := c.WaitForReplacement("default", "app=demo", "old-pod", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForReplacement: %v", err)
	}
	if healed != "new-pod" {
		t.Fatalf("healed = %q, want new-pod", healed)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("expected at least one poll cycle, returned too fast")
	}
}

func TestWaitForReplacementTimeout(t *testing.T) {
	c := &Client{Run: fakeRunner(func(args ...string) (string, error) {
		if args[1] == "pods" {
			return "old-pod", nil // replacement never appears
		}
		return "False", nil
	})}
	_, err := c.WaitForReplacement("default", "app=demo", "old-pod", 1200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "no replacement pod") {
		t.Fatalf("unexpected error: %v", err)
	}
}
