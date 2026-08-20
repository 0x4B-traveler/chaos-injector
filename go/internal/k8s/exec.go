package k8s

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// AgentRemotePath is where the agent lands inside the pod.
const AgentRemotePath = "/tmp/chaos-injector"

// AgentPath resolves the local Linux agent binary to copy into the pod:
// os.Executable when the orchestrator itself runs on Linux, otherwise the
// caller must pass an explicit path to a GOOS=linux build (a Windows build
// cannot run inside a pod).
func AgentPath(agent string) (string, error) {
	if agent != "" {
		return agent, nil
	}
	if runtime.GOOS == "linux" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return exe, nil
	}
	return "", fmt.Errorf(
		"orchestrator is not Linux: pass -agent with a GOOS=linux build of chaos-injector " +
			"(e.g. GOOS=linux go build -o chaos-injector-linux ./cmd/chaos-injector)",
	)
}

// timelineFlagValue extracts the "-timeline <path>" pair from agent args.
func timelineFlagValue(args []string) (string, bool) {
	for i, a := range args {
		if a == "-timeline" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// RunPodExperiment injects an existing fault atom inside a pod:
//
//  1. copy the agent binary into the pod (kubectl cp);
//  2. chmod +x it;
//  3. stream `chaos-injector <atom> <atomArgs>` inside the pod — the pod-side
//     agent runs the full check/inject/recover lifecycle with its own
//     auto-recovery timer, so rollback happens even if this orchestrator
//     dies;
//  4. if the agent wrote a timeline (-timeline /path), copy it back to the
//     same local path.
func RunPodExperiment(c *Client, namespace, pod, agentPath, atom string, atomArgs []string) error {
	if err := c.CopyToPod(namespace, pod, agentPath, AgentRemotePath); err != nil {
		return err
	}
	if _, err := c.Exec(namespace, pod, "chmod", "+x", AgentRemotePath); err != nil {
		return err
	}
	fmt.Printf("[k8s] agent copied to pod %s/%s, running: %s %s %s\n",
		namespace, pod, AgentRemotePath, atom, strings.Join(atomArgs, " "))
	if err := c.ExecStream(namespace, pod, append([]string{AgentRemotePath, atom}, atomArgs...)...); err != nil {
		return fmt.Errorf("agent inside pod failed: %w", err)
	}
	if tl, ok := timelineFlagValue(atomArgs); ok {
		if err := c.CopyFromPod(namespace, pod, tl, tl); err != nil {
			return err
		}
		fmt.Printf("[k8s] timeline copied back to %s\n", tl)
	}
	return nil
}
