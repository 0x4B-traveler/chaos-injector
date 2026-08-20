"""Kubernetes orchestration (云原生故障注入编排层).

Shells out to kubectl (zero-dependency principle, same as the tc/stress-ng
calls). Pod-level injection reuses the Go single-binary agent inside the pod:
arbitrary pods cannot be assumed to ship a python3 interpreter, so the
orchestrator copies the Go build (a static binary) into the target pod and
runs the existing fault atoms there. The pod-side agent owns the
auto-recovery timer, so rollback is guaranteed even if the orchestrator dies
mid-experiment.
"""

from __future__ import annotations

import subprocess
import sys
import time
from pathlib import Path

LINUX = sys.platform.startswith("linux")

AGENT_REMOTE_PATH = "/tmp/chaos-injector"


class K8sError(RuntimeError):
    """Raised when a kubectl call fails or a k8s precondition is not met."""


def _kubectl_command() -> list[str]:
    """Resolve the base command: kubectl, or the minikube wrapper."""
    import shutil

    if shutil.which("kubectl"):
        return ["kubectl"]
    if shutil.which("minikube"):
        return ["minikube", "kubectl", "--"]
    raise K8sError("kubectl not found in PATH (and no minikube fallback)")


def available() -> bool:
    try:
        _kubectl_command()
        return True
    except K8sError:
        return False


def run_kubectl(args: list[str]) -> subprocess.CompletedProcess[str]:
    """Run kubectl, raising K8sError on non-zero exit."""
    cmd = _kubectl_command() + args
    result = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        stderr = (result.stderr or result.stdout).strip()
        raise K8sError(f"kubectl {' '.join(args)} failed (rc={result.returncode}): {stderr}")
    return result


def get_pod_names(namespace: str, selector: str) -> list[str]:
    result = run_kubectl(
        [
            "get",
            "pods",
            "-n",
            namespace,
            "-l",
            selector,
            "-o",
            "jsonpath={.items[*].metadata.name}",
        ]
    )
    return result.stdout.split() if result.stdout.strip() else []


def pod_exists(namespace: str, name: str) -> bool:
    result = run_kubectl(["get", "pod", "-n", namespace, name, "-o", "name"])
    return f"pod/{name}" in result.stdout


def delete_pod(namespace: str, name: str) -> None:
    run_kubectl(["delete", "pod", "-n", namespace, name, "--wait=false"])


def copy_to_pod(namespace: str, pod: str, local: str, remote: str) -> None:
    run_kubectl(["cp", local, f"{namespace}/{pod}:{remote}"])


def copy_from_pod(namespace: str, pod: str, remote: str, local: str) -> None:
    run_kubectl(["cp", f"{namespace}/{pod}:{remote}", local])


def exec_in_pod(namespace: str, pod: str, args: list[str]) -> subprocess.CompletedProcess[str]:
    return run_kubectl(["exec", "-n", namespace, pod, "--", *args])


def exec_stream(namespace: str, pod: str, args: list[str]) -> int:
    """Run a command inside the pod with output streamed (the fault agent)."""
    cmd = _kubectl_command() + ["exec", "-n", namespace, pod, "--", *args]
    result = subprocess.run(cmd, check=False)
    return result.returncode


def _pod_ready(namespace: str, name: str) -> bool:
    result = run_kubectl(
        [
            "get",
            "pod",
            "-n",
            namespace,
            name,
            "-o",
            'jsonpath={.status.conditions[?(@.type=="Ready")].status}',
        ]
    )
    return result.stdout.strip() == "True"


def wait_for_replacement(namespace: str, selector: str, deleted: str, timeout: float) -> str:
    """Poll until a pod matching the selector is Ready and is not the deleted
    pod. Turns pod-kill into a self-healing verification."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for name in get_pod_names(namespace, selector):
            if name != deleted and _pod_ready(namespace, name):
                return name
        time.sleep(1.0)
    raise K8sError(f"no replacement pod Ready within {timeout}s (selector {selector})")


def agent_path(agent: str) -> str:
    """Local path of the Linux agent binary to copy into the pod.

    Arbitrary pods cannot be assumed to ship a python3 interpreter, so the
    in-pod agent is always the Go single binary. A prebuilt binary is
    searched for when ``agent`` is not given.
    """
    if agent:
        return agent
    for candidate in (
        "chaos-injector-linux",
        "chaos-injector-linux-amd64",
        "chaos-injector",
        "go/chaos-injector",
        "artifacts/chaos-injector-linux-amd64",
        "../go/chaos-injector",
    ):
        if Path(candidate).exists():
            return str(Path(candidate).resolve())
    raise K8sError(
        "in-pod agent not found: pass --agent with a GOOS=linux build of chaos-injector "
        "(e.g. GOOS=linux go build -o chaos-injector-linux ./cmd/chaos-injector)"
    )


def _timeline_flag_value(args: list[str]) -> str | None:
    for i, arg in enumerate(args):
        if arg == "-timeline" and i + 1 < len(args):
            return args[i + 1]
    return None


def run_pod_experiment(
    namespace: str, pod: str, agent: str, atom: str, atom_args: list[str]
) -> None:
    """Copy the agent into the pod, run <atom> inside it (streaming), and
    copy the agent timeline (if any) back to the same local path."""
    copy_to_pod(namespace, pod, agent, AGENT_REMOTE_PATH)
    exec_in_pod(namespace, pod, ["chmod", "+x", AGENT_REMOTE_PATH])
    print(
        f"[k8s] agent copied to pod {namespace}/{pod}, running: "
        f"{AGENT_REMOTE_PATH} {atom} {' '.join(atom_args)}"
    )
    rc = exec_stream(namespace, pod, [AGENT_REMOTE_PATH, atom, *atom_args])
    if rc != 0:
        raise K8sError(f"agent inside pod exited with rc={rc}")
    timeline = _timeline_flag_value(atom_args)
    if timeline:
        copy_from_pod(namespace, pod, timeline, timeline)
        print(f"[k8s] timeline copied back to {timeline}")
