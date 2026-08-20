// Package k8s orchestrates pod-level fault injection by shelling out to
// kubectl (zero-dependency principle, same as the tc/stress-ng calls).
//
// Architecture: the orchestrator copies a single static agent binary (the Go
// build of chaos-injector) into the target pod, then runs an existing fault
// atom inside the pod's own namespaces via kubectl exec. The pod-side agent
// owns the auto-recovery timer, so rollback is guaranteed even if the
// orchestrator dies mid-experiment.
package k8s

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner executes a kubectl command line and returns captured output.
// Injectable so tests can fake the cluster.
type Runner func(args ...string) (string, error)

// Client wraps kubectl with an optional "minikube kubectl --" fallback for
// hosts that only have minikube on PATH.
type Client struct {
	Run Runner
}

// New builds a Client backed by the real kubectl.
func New() *Client {
	return &Client{Run: runKubectl}
}

// kubectlCommand resolves the base command: kubectl, or the minikube wrapper.
func kubectlCommand() ([]string, error) {
	if _, err := exec.LookPath("kubectl"); err == nil {
		return []string{"kubectl"}, nil
	}
	if _, err := exec.LookPath("minikube"); err == nil {
		return []string{"minikube", "kubectl", "--"}, nil
	}
	return nil, fmt.Errorf("kubectl not found in PATH (and no minikube fallback)")
}

func runKubectl(args ...string) (string, error) {
	base, err := kubectlCommand()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(base[0], append(base[1:], args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}

// Available checks that a kubectl executable can be resolved.
func (c *Client) Available() error {
	_, err := kubectlCommand()
	return err
}

// GetPodNames returns the names of pods matching a label selector.
func (c *Client) GetPodNames(namespace, selector string) ([]string, error) {
	out, err := c.Run(
		"get", "pods", "-n", namespace, "-l", selector,
		"-o", "jsonpath={.items[*].metadata.name}",
	)
	if err != nil {
		return nil, fmt.Errorf("kubectl get pods: %w (%s)", err, out)
	}
	if out == "" {
		return nil, nil
	}
	return strings.Fields(out), nil
}

// PodExists reports whether a named pod exists in the namespace.
func (c *Client) PodExists(namespace, name string) (bool, error) {
	out, err := c.Run("get", "pod", "-n", namespace, name, "-o", "name")
	if err != nil {
		return false, fmt.Errorf("kubectl get pod %s: %w (%s)", name, err, out)
	}
	return strings.Contains(out, "pod/"+name), nil
}

// DeletePod removes a pod without waiting for it to terminate.
func (c *Client) DeletePod(namespace, name string) error {
	out, err := c.Run("delete", "pod", "-n", namespace, name, "--wait=false")
	if err != nil {
		return fmt.Errorf("kubectl delete pod %s: %w (%s)", name, err, out)
	}
	return nil
}

// CopyToPod copies a local file into the pod (kubectl cp; requires tar in
// the pod image, present in busybox/nginx and most distro images).
func (c *Client) CopyToPod(namespace, pod, local, remote string) error {
	out, err := c.Run("cp", local, namespace+"/"+pod+":"+remote)
	if err != nil {
		return fmt.Errorf("kubectl cp %s -> pod: %w (%s)", local, err, out)
	}
	return nil
}

// CopyFromPod copies a file out of the pod to the local path.
func (c *Client) CopyFromPod(namespace, pod, remote, local string) error {
	out, err := c.Run("cp", namespace+"/"+pod+":"+remote, local)
	if err != nil {
		return fmt.Errorf("kubectl cp pod -> %s: %w (%s)", local, err, out)
	}
	return nil
}

// Exec runs a command inside the pod and returns captured output.
func (c *Client) Exec(namespace, pod string, args ...string) (string, error) {
	full := append([]string{"exec", "-n", namespace, pod, "--"}, args...)
	out, err := c.Run(full...)
	if err != nil {
		return out, fmt.Errorf("kubectl exec: %w (%s)", err, out)
	}
	return out, nil
}

// ExecStream runs a command inside the pod with output streamed to the
// orchestrator's stdout/stderr. Used for the fault agent itself so the
// pod-side timeline events are visible live.
func (c *Client) ExecStream(namespace, pod string, args ...string) error {
	base, err := kubectlCommand()
	if err != nil {
		return err
	}
	full := append(base[1:], "exec", "-n", namespace, pod, "--")
	full = append(full, args...)
	cmd := exec.Command(base[0], full...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl exec stream: %w", err)
	}
	return nil
}

// WaitForReplacement polls until a pod matching the selector is Ready and is
// not the deleted pod, or the timeout elapses. It turns pod-kill into a
// self-healing verification: the replacement is the system's own recovery.
func (c *Client) WaitForReplacement(namespace, selector, deleted string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		names, err := c.GetPodNames(namespace, selector)
		if err != nil {
			return "", err
		}
		for _, name := range names {
			if name == deleted {
				continue
			}
			ready, err := c.podReady(namespace, name)
			if err != nil {
				return "", err
			}
			if ready {
				return name, nil
			}
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("no replacement pod Ready within %s (selector %s)", timeout, selector)
}

func (c *Client) podReady(namespace, name string) (bool, error) {
	out, err := c.Run(
		"get", "pod", "-n", namespace, name,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
	)
	if err != nil {
		return false, fmt.Errorf("kubectl get pod %s: %w (%s)", name, err, out)
	}
	return out == "True", nil
}
