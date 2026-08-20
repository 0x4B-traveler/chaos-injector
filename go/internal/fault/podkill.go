package fault

import (
	"fmt"
	"time"

	"chaos-injector/internal/k8s"
)

func init() {
	Register("pod-kill", func() Fault { return &PodKillFault{} })
}

// PodKillFault deletes a Kubernetes pod (chaosblade "k8s pod-kill"
// equivalent). Deleting a pod is not reversible, so Recover is a no-op: the
// intended rollback is Kubernetes self-healing (the Deployment recreates the
// pod). With WaitReady set, Inject blocks until a replacement pod is Ready,
// turning the experiment into a self-healing verification.
type PodKillFault struct {
	Namespace string
	Pod       string        // explicit pod name (mutually exclusive with Selector)
	Selector  string        // label selector of pods to delete
	Count     int           // max number of matching pods to delete
	WaitReady time.Duration // wait for a replacement pod after delete (0 = skip)

	client  *k8s.Client
	targets []string
	healed  string
}

func (f *PodKillFault) Name() string        { return "pod-kill" }
func (f *PodKillFault) Description() string { return "delete a Kubernetes pod (self-healing chaos)" }

func (f *PodKillFault) Describe() string {
	return fmt.Sprintf(
		"pod-kill(namespace=%s pod=%q selector=%q count=%d wait-ready=%s)",
		f.Namespace, f.Pod, f.Selector, f.Count, f.WaitReady,
	)
}

// Healed returns the name of the replacement pod observed after deletion
// (empty when WaitReady was disabled or healing was not observed).
func (f *PodKillFault) Healed() string { return f.healed }

func (f *PodKillFault) clientOrNew() *k8s.Client {
	if f.client == nil {
		f.client = k8s.New()
	}
	return f.client
}

func (f *PodKillFault) Check() error {
	if f.Namespace == "" {
		return Errf("namespace must not be empty")
	}
	if (f.Pod == "") == (f.Selector == "") {
		return Errf("exactly one of pod or selector must be set")
	}
	if f.Count < 1 {
		f.Count = 1
	}
	if f.WaitReady > 0 && f.Selector == "" {
		return Errf("wait-ready requires a label selector (a replacement pod cannot be matched by name)")
	}
	c := f.clientOrNew()
	if err := c.Available(); err != nil {
		return Errf("kubectl unavailable: %v", err)
	}
	if f.Pod != "" {
		ok, err := c.PodExists(f.Namespace, f.Pod)
		if err != nil {
			return err
		}
		if !ok {
			return Errf("pod %s/%s not found", f.Namespace, f.Pod)
		}
		f.targets = []string{f.Pod}
		return nil
	}
	names, err := c.GetPodNames(f.Namespace, f.Selector)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return Errf("no pod matches selector %q in namespace %s", f.Selector, f.Namespace)
	}
	if len(names) > f.Count {
		names = names[:f.Count]
	}
	f.targets = names
	return nil
}

func (f *PodKillFault) Inject() error {
	if err := f.Check(); err != nil {
		return err
	}
	c := f.clientOrNew()
	for _, name := range f.targets {
		if err := c.DeletePod(f.Namespace, name); err != nil {
			return err
		}
	}
	if f.WaitReady > 0 {
		healed, err := c.WaitForReplacement(f.Namespace, f.Selector, f.targets[0], f.WaitReady)
		if err != nil {
			return err
		}
		f.healed = healed
	}
	return nil
}

func (f *PodKillFault) Recover() error {
	// No-op by design: a deleted pod cannot be restored; the recovery is
	// Kubernetes self-healing, which is what the experiment verifies.
	return nil
}
