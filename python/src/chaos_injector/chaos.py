"""Command-line interface for chaos-injector.

Subcommands map to fault atoms; ``random`` exercises the random-decision
layer (随机决策层) with safe parameter ranges and a load gate.

Safety rules
------------
- Real injection requires an explicit ``--confirm`` flag.
- ``--dry-run`` validates preconditions and prints the plan, injecting nothing.
- Every experiment auto-recovers after ``--duration`` seconds; Ctrl-C also
  triggers recovery via the context manager.
"""

from __future__ import annotations

import argparse
import os
import sys
import time
from pathlib import Path
from typing import Any

from . import k8s
from .faults import (
    LINUX,
    BaseFault,
    CpuFault,
    EnvProbe,
    FaultError,
    MemFault,
    MySQLFault,
    NetworkFault,
    PodKillFault,
    PortFault,
    ProcessFault,
)
from .scheduler import Experiment

MAX_CORES = 4

_MASK64 = (1 << 64) - 1


class SeededRng:
    """Deterministic PRNG shared with the Go implementation (xorshift64*).

    The same seed produces the same sequence in both languages, so
    ``random --seed N`` reproduces the exact same experiment on either
    implementation (稳定复现).
    """

    def __init__(self, seed: int) -> None:
        # splitmix64 maps any seed (including 0) to a non-zero state.
        z = (seed + 0x9E3779B97F4A7C15) & _MASK64
        z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & _MASK64
        z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & _MASK64
        self._state = (z ^ (z >> 31)) & _MASK64

    def _next(self) -> int:
        s = self._state
        s = (s ^ (s >> 12)) & _MASK64
        s = (s ^ (s << 25)) & _MASK64
        s = (s ^ (s >> 27)) & _MASK64
        self._state = s
        return (s * 0x2545F4914F6CDD1D) & _MASK64

    def randint(self, lo: int, hi: int) -> int:
        """Return a value in [lo, hi] inclusive, matching Go's IntRange."""
        return lo + self._next() % (hi - lo + 1)

    def choice(self, seq: list[Any]) -> Any:
        """Return one element of seq, matching Go's IntN-indexed choice."""
        return seq[self._next() % len(seq)]


def _auto_seed() -> int:
    """Generate a fresh seed when the user did not pass ``--seed``."""
    return int.from_bytes(os.urandom(8), "big") & 0x7FFFFFFFFFFFFFFF


def _print_event(event: dict) -> None:
    print(f"  [{event['at'][11:23]}] {event['phase']:<8} {event['detail']}")


def _common_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--duration",
        type=float,
        required=True,
        help="fault lifetime in seconds (auto-rollback after)",
    )
    parser.add_argument(
        "--confirm", action="store_true", help="acknowledge that a real fault will be injected"
    )
    parser.add_argument(
        "--dry-run", action="store_true", help="validate preconditions and print the plan only"
    )
    parser.add_argument(
        "--timeline", type=Path, help="write the experiment timeline as JSON evidence"
    )


def _ensure_confirm(args: argparse.Namespace, plan: str) -> None:
    if args.dry_run:
        print(f"[dry-run] would inject: {plan}")
        raise SystemExit(0)
    if not args.confirm:
        raise SystemExit("refusing to inject without --confirm")


def cmd_network(args: argparse.Namespace) -> int:
    fault = NetworkFault(interface=args.iface, delay_ms=args.delay, loss_pct=args.loss)
    plan = (
        f"NetworkFault(iface={args.iface}, delay={args.delay}ms, loss={args.loss}%, "
        f"{args.duration}s)"
    )
    _ensure_confirm(args, plan)
    return _run_experiment("network", fault, args)


def cmd_cpu(args: argparse.Namespace) -> int:
    fault = CpuFault(load_percent=args.load, cores=args.cores)
    plan = f"CpuFault(load={args.load}%, cores={args.cores}, {args.duration}s)"
    _ensure_confirm(args, plan)
    return _run_experiment("cpu", fault, args)


def cmd_mem(args: argparse.Namespace) -> int:
    fault = MemFault(size_mb=args.size_mb)
    plan = f"MemFault(size={args.size_mb}MB, {args.duration}s)"
    _ensure_confirm(args, plan)
    return _run_experiment("mem", fault, args)


def cmd_process(args: argparse.Namespace) -> int:
    fault = ProcessFault(pattern=args.pattern)
    plan = f"ProcessFault(pattern={args.pattern!r}, {args.duration}s)"
    _ensure_confirm(args, plan)
    return _run_experiment("process", fault, args)


def cmd_port(args: argparse.Namespace) -> int:
    fault = PortFault(port=args.port)
    plan = f"PortFault(port={args.port}, {args.duration}s)"
    _ensure_confirm(args, plan)
    return _run_experiment("port", fault, args)


def cmd_mysql(args: argparse.Namespace) -> int:
    fault = MySQLFault(
        host=args.host,
        port=args.port,
        user=args.user,
        password=args.password,
        database=args.database,
        mode=args.mode,
        connections=args.connections,
        table=args.table or "",
        session_user=args.session_user or "",
        session_db=args.session_db or "",
        session_command=args.session_command or "",
        session_pattern=args.session_pattern or "",
        duration=args.duration,
    )
    # The plan must never contain the password (it goes via MYSQL_PWD only).
    plan = (
        f"MySQLFault(mode={args.mode}, host={args.host}:{args.port}, user={args.user}, "
        f"database={args.database or '-'}, {args.duration}s)"
    )
    _ensure_confirm(args, plan)
    return _run_experiment("mysql", fault, args)


def cmd_k8s_podkill(args: argparse.Namespace) -> int:
    fault = PodKillFault(
        namespace=args.namespace,
        pod=args.pod or "",
        selector=args.selector or "",
        count=args.count,
        wait_ready=args.wait_ready,
    )
    plan = (
        f"PodKillFault(ns={args.namespace}, pod={args.pod!r}, selector={args.selector!r}, "
        f"count={args.count})"
    )
    _ensure_confirm(args, plan)
    # Instantaneous fault: 1s observation window, then no-op recover
    # (the recovery is Kubernetes self-healing, verified by wait-ready).
    exp = Experiment("k8s-pod-kill", fault, 1.0, on_event=_print_event)
    with exp:
        try:
            exp.start()
            time.sleep(1.0)
        except KeyboardInterrupt:
            print("\n[interrupt] Ctrl-C received, rolling back...")
    if healed := fault.healed():
        print(f"[k8s] replacement ready: {healed} (self-healing observed)")
    if args.timeline:
        exp.write_timeline(args.timeline)
        print(f"[timeline] written to {args.timeline}")
    return 0


def cmd_k8s_exec(args: argparse.Namespace) -> int:
    agent = k8s.agent_path(args.agent)
    if args.dry_run:
        print(
            f"[dry-run] would copy {agent} to pod {args.namespace}/{args.pod} and run: "
            f"{k8s.AGENT_REMOTE_PATH} {args.atom} {' '.join(args.atom_args)}"
        )
        return 0
    if not args.confirm:
        raise SystemExit("refusing to inject without --confirm")
    k8s.run_pod_experiment(args.namespace, args.pod, agent, args.atom, args.atom_args)
    return 0


def cmd_random(args: argparse.Namespace) -> int:
    """Random-decision layer: pick a fault and safe parameters, gate on load.

    ``--seed`` makes the whole decision reproducible: the same seed always
    picks the same fault with the same parameters (and the same sequence in
    the Go implementation). Without ``--seed`` a fresh seed is generated and
    still recorded, so every experiment can be replayed later.
    """
    probe = EnvProbe()
    ok, detail = probe.load_ok()
    print(f"[env] {detail}")
    if not ok:
        raise SystemExit(f"refusing to inject: host already under load ({detail})")

    seed = args.seed if args.seed is not None else _auto_seed()
    rng = SeededRng(seed)
    print(f"[random] seed={seed}")

    kinds = ["cpu", "mem", "port"]
    if args.iface and LINUX:
        kinds.append("network")
    kind = rng.choice(kinds)
    if kind == "network":
        fault = NetworkFault(
            interface=args.iface,
            delay_ms=rng.choice([50, 100, 200, 300, 500]),
            loss_pct=rng.choice([0.0, 1.0, 3.0, 5.0, 10.0]),
        )
    elif kind == "mem":
        # Deterministic range; OOM protection is enforced by MemFault.check().
        fault = MemFault(size_mb=rng.randint(64, 256))
    elif kind == "port":
        fault = PortFault(port=rng.randint(8000, 9999))
    else:
        fault = CpuFault(
            load_percent=rng.randint(30, 90),
            cores=rng.randint(1, MAX_CORES),
        )
    _ensure_confirm(args, f"randomly chosen {fault.describe()} for {args.duration}s")
    return _run_experiment("random", fault, args, seed=seed)


def _run_experiment(
    name: str, fault: BaseFault, args: argparse.Namespace, seed: int | None = None
) -> int:
    exp = Experiment(f"{name}-{fault.name}", fault, args.duration, on_event=_print_event, seed=seed)
    with exp:
        try:
            exp.start()
            time.sleep(args.duration)
        except KeyboardInterrupt:
            print("\n[interrupt] Ctrl-C received, rolling back...")
    if args.timeline:
        exp.write_timeline(args.timeline)
        print(f"[timeline] written to {args.timeline}")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="chaos-injector",
        description="Lightweight chaos experiment engine (four-layer: environment awareness -> "
        "random decision -> fault execution -> recovery & monitoring)",
    )
    parser.add_argument("--list", action="store_true", help="list supported fault atoms")
    sub = parser.add_subparsers(dest="command")

    net = sub.add_parser("network", help="inject network delay / packet loss (Linux tc netem)")
    net.add_argument("--iface", required=True, help="network interface to fault (e.g. eth0)")
    net.add_argument("--delay", type=int, default=200, help="one-way delay in ms")
    net.add_argument("--loss", type=float, default=0.0, help="packet loss percent [0-100]")
    _common_args(net)
    net.set_defaults(func=cmd_network)

    cpu = sub.add_parser(
        "cpu", help="inject CPU load (stress-ng on Linux, stdlib burner elsewhere)"
    )
    cpu.add_argument("--load", type=int, default=80, help="target load percent [1-100]")
    cpu.add_argument("--cores", type=int, default=1, help="number of cores to burn")
    _common_args(cpu)
    cpu.set_defaults(func=cmd_cpu)

    mem = sub.add_parser(
        "mem", help="inject memory occupancy (chaosblade mem load equivalent)"
    )
    mem.add_argument("--size-mb", type=int, default=256, help="memory to occupy in MB")
    _common_args(mem)
    mem.set_defaults(func=cmd_mem)

    proc = sub.add_parser(
        "process", help="terminate processes matching a pattern (chaosblade process kill)"
    )
    proc.add_argument("--pattern", required=True, help="kill processes whose name/cmdline contains it")
    _common_args(proc)
    proc.set_defaults(func=cmd_process)

    port = sub.add_parser(
        "port", help="occupy a TCP port (chaosblade network port occupy equivalent)"
    )
    port.add_argument("--port", type=int, default=8080, help="TCP port to occupy")
    _common_args(port)
    port.set_defaults(func=cmd_port)

    mysql_p = sub.add_parser(
        "mysql", help="database instance faults via mysql client (connection/lock/session)"
    )
    mysql_p.add_argument("--host", default="127.0.0.1", help="mysql host")
    mysql_p.add_argument("--port", type=int, default=3306, help="mysql port")
    mysql_p.add_argument("--user", default="root", help="mysql user")
    mysql_p.add_argument(
        "--password",
        default="",
        help="mysql password (passed via MYSQL_PWD env, never logged)",
    )
    mysql_p.add_argument("--database", default="", help="database to fault against")
    mysql_p.add_argument(
        "--mode", choices=["connection", "lock", "session"], default="connection"
    )
    mysql_p.add_argument(
        "--connections", type=int, default=20, help="connections to occupy (connection mode)"
    )
    mysql_p.add_argument("--table", help="table to lock (lock mode)")
    mysql_p.add_argument("--session-user", help="kill sessions of this user (session mode)")
    mysql_p.add_argument("--session-db", help="kill sessions using this database")
    mysql_p.add_argument(
        "--session-command", help="kill sessions running this command (e.g. Query)"
    )
    mysql_p.add_argument(
        "--session-pattern", help="kill sessions whose SQL contains this pattern"
    )
    _common_args(mysql_p)
    mysql_p.set_defaults(func=cmd_mysql)

    rnd = sub.add_parser(
        "random", help="random fault orchestration within safe ranges (load-gated)"
    )
    rnd.add_argument("--iface", help="required to enable network faults in the random pool")
    rnd.add_argument(
        "--seed",
        type=int,
        default=None,
        help="PRNG seed for reproducible experiments (same seed -> same fault, "
        "identical to the Go implementation)",
    )
    _common_args(rnd)
    rnd.set_defaults(func=cmd_random)

    k8s_p = sub.add_parser(
        "k8s", help="Kubernetes pod-level fault injection (shells out to kubectl)"
    )
    k8s_sub = k8s_p.add_subparsers(dest="k8s_action", required=True)

    pk = k8s_sub.add_parser(
        "pod-kill", help="delete a Kubernetes pod (self-healing chaos)"
    )
    pk.add_argument("--namespace", default="default", help="kubernetes namespace")
    pk.add_argument("--pod", help="pod name to delete (mutually exclusive with --selector)")
    pk.add_argument("--selector", help="label selector of pods to delete")
    pk.add_argument("--count", type=int, default=1, help="max number of matching pods to delete")
    pk.add_argument(
        "--wait-ready",
        type=float,
        default=60.0,
        help="seconds to wait for a replacement pod (0 = skip)",
    )
    pk.add_argument("--confirm", action="store_true", help="acknowledge that a real fault will be injected")
    pk.add_argument("--dry-run", action="store_true", help="validate preconditions and print the plan only")
    pk.add_argument("--timeline", type=Path, help="write the experiment timeline as JSON evidence")
    pk.set_defaults(func=cmd_k8s_podkill)

    ex = k8s_sub.add_parser(
        "exec", help="inject an existing fault atom inside a pod via kubectl exec"
    )
    ex.add_argument("--namespace", default="default", help="kubernetes namespace")
    ex.add_argument("--pod", required=True, help="pod to inject into")
    ex.add_argument(
        "--agent",
        default="",
        help="path to a Linux chaos-injector binary to copy into the pod",
    )
    ex.add_argument("--confirm", action="store_true", help="acknowledge that a real fault will be injected")
    ex.add_argument("--dry-run", action="store_true", help="validate preconditions and print the plan only")
    ex.add_argument(
        "atom", choices=["network", "cpu", "mem", "process", "port", "mysql"], help="fault atom to run in the pod"
    )
    ex.add_argument(
        "atom_args",
        nargs=argparse.REMAINDER,
        help="agent flags after the atom (Go style: -duration/-confirm/-timeline)",
    )
    ex.set_defaults(func=cmd_k8s_exec)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)

    if args.list:
        from .faults import FAULTS

        for kind, cls in FAULTS.items():
            print(f"{kind:<10} {cls.description}")
        return 0

    if args.command is None:
        parser.print_help(sys.stderr)
        return 2

    try:
        return args.func(args)
    except (FaultError, k8s.K8sError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    except SystemExit as exc:
        if isinstance(exc.code, str):
            print(f"error: {exc.code}", file=sys.stderr)
            return 2
        raise


if __name__ == "__main__":
    raise SystemExit(main())
