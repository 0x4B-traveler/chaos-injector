"""Unit tests for chaos-injector: fault atoms, scheduler safety, CLI gates."""

from __future__ import annotations

import dataclasses
import json
import socket
import sys
import time

import pytest

from chaos_injector import chaos as cli
from chaos_injector import faults, k8s
from chaos_injector.faults import (
    FAULTS,
    BaseFault,
    CpuFault,
    FaultError,
    MemFault,
    MySQLFault,
    NetworkFault,
    PodKillFault,
    PortFault,
    ProcessFault,
)
from chaos_injector.scheduler import Experiment


@dataclasses.dataclass
class FakeFault(BaseFault):
    """Recorded-lifecycle fault for scheduler tests."""

    name: str = "fake"
    description: str = "fake fault for tests"
    injected: bool = False
    recovered: bool = False

    def check(self) -> None:
        pass

    def inject(self) -> None:
        self.injected = True

    def recover(self) -> None:
        self.recovered = True


# -- fault registry -------------------------------------------------------


def test_registry_contains_core_atoms():
    assert set(FAULTS) == {"network", "cpu", "mem", "port", "process", "pod-kill", "mysql"}
    assert issubclass(FAULTS["network"], NetworkFault)
    assert issubclass(FAULTS["cpu"], CpuFault)
    assert issubclass(FAULTS["mem"], MemFault)
    assert issubclass(FAULTS["port"], PortFault)
    assert issubclass(FAULTS["process"], ProcessFault)
    assert issubclass(FAULTS["pod-kill"], PodKillFault)
    assert issubclass(FAULTS["mysql"], MySQLFault)


def test_network_fault_parameter_validation():
    with pytest.raises(FaultError):
        NetworkFault(delay_ms=-1)
    with pytest.raises(FaultError):
        NetworkFault(loss_pct=101)
    with pytest.raises(FaultError):
        NetworkFault(delay_ms=0, loss_pct=0)


def test_cpu_fault_parameter_validation():
    with pytest.raises(FaultError):
        CpuFault(load_percent=0)
    with pytest.raises(FaultError):
        CpuFault(load_percent=101)
    with pytest.raises(FaultError):
        CpuFault(cores=0)


def test_recover_without_inject_is_safe():
    # recover() must never raise, even if inject() was never called.
    CpuFault(load_percent=50, cores=1).recover()
    MemFault(size_mb=8).recover()
    PortFault(port=18080).recover()
    ProcessFault(pattern="x").recover()


# -- new atoms: mem / port / process --------------------------------------


def test_mem_fault_parameter_validation():
    with pytest.raises(FaultError):
        MemFault(size_mb=0)


def test_mem_fault_inject_and_rollback():
    fault = MemFault(size_mb=1)
    fault.inject()
    assert fault._buf is not None and len(fault._buf) == 1024 * 1024
    fault.recover()
    assert fault._buf is None and fault._toucher is None


def test_port_fault_parameter_validation():
    with pytest.raises(FaultError):
        PortFault(port=0)
    with pytest.raises(FaultError):
        PortFault(port=65536)


def test_port_fault_inject_and_rollback():
    fault = PortFault(port=18080)
    fault.inject()
    assert fault._sock is not None
    # The port is now occupied: a second bind must fail.
    probe = socket.socket()
    with pytest.raises(OSError):
        probe.bind(("", 18080))
    probe.close()
    fault.recover()
    assert fault._sock is None
    # The port must be reusable right after recovery.
    fault.inject()
    fault.recover()


@pytest.mark.skipif(sys.platform.startswith("linux"), reason="platform gate only rejects non-Linux")
def test_process_fault_rejected_on_non_linux():
    fault = ProcessFault(pattern="sleep")
    try:
        fault.check()
    except FaultError as exc:
        assert "Linux" in str(exc)
        return
    assert False, "ProcessFault.check should reject non-Linux platforms"


def test_process_fault_empty_pattern_rejected():
    with pytest.raises(FaultError):
        ProcessFault(pattern="  ").check()


def test_cli_mem_port_subcommands_require_confirm(capsys):
    rc = cli.main(["mem", "--size-mb", "64", "--duration", "1"])
    assert rc == 2
    assert "refusing" in capsys.readouterr().err
    rc = cli.main(["port", "--port", "18080", "--duration", "1"])
    assert rc == 2
    assert "refusing" in capsys.readouterr().err


# -- scheduler safety -----------------------------------------------------


def test_experiment_auto_recovers_after_duration():
    fault = FakeFault()
    exp = Experiment("t", fault, duration=0.5)
    timeline = exp.run()
    time.sleep(0.3)  # let the timer callback land

    assert fault.injected
    assert fault.recovered
    phases = [event["phase"] for event in timeline]
    assert phases[:4] == ["start", "check", "inject", "armed"]
    assert "recover" in phases
    assert "done" in phases
    assert "auto" in phases


def test_context_manager_recovers_on_exception():
    fault = FakeFault()
    with pytest.raises(RuntimeError), Experiment("t", fault, duration=60) as exp:
        exp.start()
        raise RuntimeError("boom")
    assert fault.injected
    assert fault.recovered


def test_recover_is_idempotent():
    fault = FakeFault()
    exp = Experiment("t", fault, duration=60)
    exp.start()
    exp.recover()
    exp.recover()  # second call must be a no-op
    assert fault.recovered
    assert sum(1 for e in exp.timeline if e["phase"] == "recover") == 1


def test_timeline_written_to_nested_dir(tmp_path):
    fault = FakeFault()
    exp = Experiment("t", fault, duration=60)
    exp.start()
    exp.recover()
    target = tmp_path / "nested" / "dir" / "exp.json"
    exp.write_timeline(target)  # parent dirs must be created automatically
    assert target.exists()
    payload = json.loads(target.read_text(encoding="utf-8"))
    assert payload["experiment"] == "t"
    assert payload["recovered"] is True


def test_cpu_fault_real_inject_and_rollback():
    # Real end-to-end exercise: burners start and are cleaned up.
    fault = CpuFault(load_percent=30, cores=1)
    exp = Experiment("real-cpu", fault, duration=1.0)
    exp.start()
    assert len(fault._burners) == 1
    burner = fault._burners[0][0]
    assert burner.is_alive()
    exp.recover()
    assert not burner.is_alive()
    assert fault._burners == []


# -- CLI gates ------------------------------------------------------------


def test_cli_refuses_without_confirm(capsys):
    rc = cli.main(["cpu", "--load", "50", "--duration", "1"])
    assert rc == 2
    assert "refusing to inject without --confirm" in capsys.readouterr().err


def test_cli_dry_run_injects_nothing(capsys):
    with pytest.raises(SystemExit) as exc:
        cli.main(["cpu", "--load", "50", "--duration", "1", "--dry-run"])
    assert exc.value.code == 0
    assert "[dry-run]" in capsys.readouterr().out


def test_cli_list_faults(capsys):
    rc = cli.main(["--list"])
    assert rc == 0
    out = capsys.readouterr().out
    assert (
        "network" in out
        and "cpu" in out
        and "mem" in out
        and "port" in out
        and "process" in out
        and "pod-kill" in out
    )


@pytest.mark.skipif(sys.platform.startswith("linux"), reason="platform gate only rejects non-Linux")
def test_cli_network_rejected_on_non_linux():
    fault = NetworkFault(interface="eth0", delay_ms=200)
    try:
        fault.check()
    except FaultError as exc:
        assert "Linux" in str(exc)
        return
    assert False, "NetworkFault.check should reject non-Linux platforms"


def test_network_recover_is_safe_on_non_linux():
    # recover() must not crash on platforms where tc never existed.
    NetworkFault(interface="eth0", delay_ms=200).recover()


# -- Kubernetes orchestration (k8s module + PodKillFault) -----------------


def test_podkill_parameter_validation(monkeypatch):
    with pytest.raises(FaultError):
        PodKillFault(pod="p1", selector="app=x").check()  # both set
    with pytest.raises(FaultError):
        PodKillFault().check()  # neither set
    with pytest.raises(FaultError):
        PodKillFault(namespace="", pod="p1").check()  # empty namespace
    with pytest.raises(FaultError):
        PodKillFault(pod="p1", wait_ready=10).check()  # wait-ready needs selector
    monkeypatch.setattr(k8s, "available", lambda: False)
    with pytest.raises(FaultError):
        PodKillFault(selector="app=none").check()  # kubectl unavailable


def test_podkill_check_by_selector_caps_count(monkeypatch):
    monkeypatch.setattr(k8s, "available", lambda: True)
    monkeypatch.setattr(k8s, "get_pod_names", lambda ns, sel: ["pod-a", "pod-b", "pod-c"])
    fault = PodKillFault(selector="app=demo", count=2)
    fault.check()
    assert fault._targets == ["pod-a", "pod-b"]


def test_podkill_check_by_pod_name(monkeypatch):
    monkeypatch.setattr(k8s, "available", lambda: True)
    monkeypatch.setattr(k8s, "pod_exists", lambda ns, name: True)
    fault = PodKillFault(pod="nginx-abc", wait_ready=0)
    fault.check()
    assert fault._targets == ["nginx-abc"]


def test_podkill_check_rejects_missing_pod(monkeypatch):
    monkeypatch.setattr(k8s, "available", lambda: True)
    monkeypatch.setattr(k8s, "pod_exists", lambda ns, name: False)
    with pytest.raises(FaultError):
        PodKillFault(pod="nginx-xyz", wait_ready=0).check()


def test_podkill_check_rejects_empty_selector(monkeypatch):
    monkeypatch.setattr(k8s, "available", lambda: True)
    monkeypatch.setattr(k8s, "get_pod_names", lambda ns, sel: [])
    with pytest.raises(FaultError):
        PodKillFault(selector="app=none").check()


def test_podkill_inject_deletes_and_observes_healing(monkeypatch):
    deleted: list[str] = []
    monkeypatch.setattr(k8s, "available", lambda: True)
    monkeypatch.setattr(k8s, "get_pod_names", lambda ns, sel: ["old-pod", "new-pod"])
    monkeypatch.setattr(k8s, "delete_pod", lambda ns, name: deleted.append(name))
    monkeypatch.setattr(k8s, "wait_for_replacement", lambda ns, sel, d, t: "new-pod")
    fault = PodKillFault(selector="app=demo", wait_ready=5)
    fault.inject()
    assert deleted == ["old-pod"]
    assert fault.healed() == "new-pod"


def test_podkill_recover_is_no_op():
    assert PodKillFault(selector="app=demo").recover() is None


def test_k8s_get_pod_names_parsing(monkeypatch):
    class FakeResult:
        stdout = "pod-a pod-b"

    monkeypatch.setattr(k8s, "run_kubectl", lambda args: FakeResult())
    assert k8s.get_pod_names("default", "app=demo") == ["pod-a", "pod-b"]


def test_k8s_run_kubectl_raises_on_error(monkeypatch):
    import subprocess

    def fake_run(cmd, capture_output, text, check):
        return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="boom")

    monkeypatch.setattr(subprocess, "run", fake_run)
    with pytest.raises(k8s.K8sError):
        k8s.run_kubectl(["get", "pods"])


def test_k8s_available_false_without_kubectl(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    assert k8s.available() is False


def test_k8s_agent_path_explicit(tmp_path):
    agent = tmp_path / "agent"
    agent.write_text("x")
    assert k8s.agent_path(str(agent)) == str(agent)


def test_k8s_agent_path_searches_candidates(monkeypatch, tmp_path):
    (tmp_path / "chaos-injector-linux-amd64").write_text("x")
    monkeypatch.chdir(tmp_path)
    assert k8s.agent_path("") == str((tmp_path / "chaos-injector-linux-amd64").resolve())


def test_k8s_agent_path_missing(monkeypatch, tmp_path):
    monkeypatch.chdir(tmp_path)
    with pytest.raises(k8s.K8sError):
        k8s.agent_path("")


def test_k8s_run_pod_experiment_flow(monkeypatch):
    calls: list[tuple] = []
    monkeypatch.setattr(
        k8s, "copy_to_pod", lambda ns, pod, l, r: calls.append(("copy_to", l, r))
    )
    monkeypatch.setattr(k8s, "exec_in_pod", lambda ns, pod, args: calls.append(("exec", args)))
    monkeypatch.setattr(
        k8s, "exec_stream", lambda ns, pod, args: calls.append(("stream", args)) or 0
    )
    monkeypatch.setattr(
        k8s, "copy_from_pod", lambda ns, pod, r, l: calls.append(("copy_from", r, l))
    )
    k8s.run_pod_experiment(
        "default", "busybox", "/tmp/agent", "cpu", ["-duration", "2", "-timeline", "/tmp/tl.json"]
    )
    assert calls[0] == ("copy_to", "/tmp/agent", "/tmp/chaos-injector")
    assert calls[1] == ("exec", ["chmod", "+x", "/tmp/chaos-injector"])
    assert calls[2] == (
        "stream",
        ["/tmp/chaos-injector", "cpu", "-duration", "2", "-timeline", "/tmp/tl.json"],
    )
    assert calls[3] == ("copy_from", "/tmp/tl.json", "/tmp/tl.json")


def test_k8s_run_pod_experiment_agent_failure(monkeypatch):
    monkeypatch.setattr(k8s, "copy_to_pod", lambda *a: None)
    monkeypatch.setattr(k8s, "exec_in_pod", lambda *a: None)
    monkeypatch.setattr(k8s, "exec_stream", lambda *a: 1)
    with pytest.raises(k8s.K8sError):
        k8s.run_pod_experiment("default", "busybox", "/tmp/agent", "cpu", ["-duration", "2"])


def test_k8s_cli_podkill_dry_run(capsys):
    with pytest.raises(SystemExit) as exc:
        cli.main(["k8s", "pod-kill", "--selector", "app=demo", "--dry-run"])
    assert exc.value.code == 0
    assert "[dry-run]" in capsys.readouterr().out


def test_k8s_cli_podkill_refuses_without_confirm(capsys):
    rc = cli.main(["k8s", "pod-kill", "--selector", "app=demo"])
    assert rc == 2
    assert "refusing to inject without --confirm" in capsys.readouterr().err


def test_k8s_cli_exec_requires_agent(capsys, monkeypatch):
    def no_agent(_agent: str) -> str:
        raise k8s.K8sError("in-pod agent not found: pass --agent")

    monkeypatch.setattr(k8s, "agent_path", no_agent)
    rc = cli.main(["k8s", "exec", "--pod", "busybox", "cpu", "-duration", "2"])
    assert rc == 2
    assert "agent" in capsys.readouterr().err


def test_k8s_cli_exec_dry_run(capsys, monkeypatch):
    monkeypatch.setattr(k8s, "agent_path", lambda agent: "/tmp/agent")
    rc = cli.main(["k8s", "exec", "--pod", "busybox", "--dry-run", "cpu", "-duration", "2"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "[dry-run]" in out and "/tmp/chaos-injector cpu -duration 2" in out


# -- reproducibility: seeded PRNG + timeline snapshots --------------------


# These constants are pinned in the Go test suite (rng_test.go): the same
# seed must produce the identical sequence on both implementations.
def test_seeded_rng_canonical_sequence():
    rng = cli.SeededRng(42)
    assert [rng._next() for _ in range(6)] == [
        3580622183945639842,
        10378725325292465923,
        8967075514996744559,
        5001014893397904463,
        14825054885549601002,
        10736401887688096443,
    ]


def test_seeded_rng_deterministic_and_bounds():
    # Same seed -> same sequence, including seed 0 (splitmix64 non-zero state).
    for seed in [42, 0, 1, 123456789]:
        a = cli.SeededRng(seed)
        b = cli.SeededRng(seed)
        assert [a._next() for _ in range(20)] == [b._next() for _ in range(20)]
    # Canonical ranges agree with the Go implementation (seed 42).
    rng = cli.SeededRng(42)
    assert [rng.randint(64, 256) for _ in range(6)] == [79, 97, 110, 222, 89, 230]
    rng = cli.SeededRng(42)
    assert [rng.choice(["cpu", "mem", "port"]) for _ in range(6)] == [
        "mem",
        "mem",
        "port",
        "mem",
        "port",
        "cpu",
    ]
    # Bounds sanity across many draws.
    rng = cli.SeededRng(7)
    for _ in range(100):
        assert 30 <= rng.randint(30, 90) <= 90
        assert 0 <= rng.choice([0, 1, 2, 3, 4]) <= 4
    assert cli.SeededRng(7).randint(5, 5) == 5


def test_cli_random_same_seed_same_plan(capsys):
    # Two dry-runs with the same seed must pick the exact same fault; the
    # printed seed lets a user replay an experiment without --timeline.
    with pytest.raises(SystemExit) as first:
        cli.main(["random", "--seed", "42", "--duration", "1", "--dry-run"])
    out1 = capsys.readouterr().out
    assert first.value.code == 0
    assert "seed=42" in out1 and "[dry-run]" in out1

    with pytest.raises(SystemExit) as second:
        cli.main(["random", "--seed", "42", "--duration", "1", "--dry-run"])
    out2 = capsys.readouterr().out
    assert second.value.code == 0
    assert out1 == out2


def test_experiment_timeline_records_seed_and_snapshot(tmp_path):
    fault = FakeFault()
    exp = Experiment("random-mem", fault, duration=60, seed=42)
    exp.start()
    exp.recover()
    target = tmp_path / "exp.json"
    exp.write_timeline(target)
    payload = json.loads(target.read_text(encoding="utf-8"))
    assert payload["seed"] == 42
    snap = payload["snapshot"]
    for state in (snap["before"], snap["after"]):
        assert state["hostname"]
        assert state["platform"]
        assert state["cpu_count"]


def test_experiment_timeline_omits_seed_when_unset(tmp_path):
    fault = FakeFault()
    exp = Experiment("cpu", fault, duration=60)
    exp.start()
    exp.recover()
    target = tmp_path / "exp.json"
    exp.write_timeline(target)
    payload = json.loads(target.read_text(encoding="utf-8"))
    assert "seed" not in payload
    assert "snapshot" in payload


# -- mysql atom (mocked mysql CLI, zero password leakage) ------------------


@dataclasses.dataclass
class FakeMySQLResult:
    """CompletedProcess stand-in returned by the mocked _run_mysql."""

    returncode: int = 0
    stdout: str = ""
    stderr: str = ""


class FakeMySQLProc:
    """Popen stand-in returned by the mocked _spawn_mysql."""

    def __init__(self) -> None:
        self.killed = False

    def poll(self):
        return None

    def kill(self):
        self.killed = True


def test_mysql_fault_parameter_validation(monkeypatch):
    monkeypatch.setattr(faults.shutil, "which", lambda _name: "/usr/bin/mysql")
    cases = [
        MySQLFault(mode="drop", user="u", password="p"),
        MySQLFault(mode="connection", port=70000, user="u", password="p"),
        MySQLFault(mode="connection", user="u"),
        MySQLFault(mode="connection", user="u", password="p", connections=0, duration=1),
        MySQLFault(mode="connection", user="u", password="p", connections=5),
        MySQLFault(mode="lock", user="u", password="p", database="app", duration=1),
        MySQLFault(mode="lock", user="u", password="p", database="app", table="x; DROP TABLE t", duration=1),
        MySQLFault(mode="session", user="u", password="p"),
    ]
    for f in cases:
        with pytest.raises(FaultError):
            f.check()


def test_mysql_check_requires_cli(monkeypatch):
    monkeypatch.setattr(faults.shutil, "which", lambda _name: None)
    f = MySQLFault(mode="connection", user="u", password="p", connections=5, duration=1)
    with pytest.raises(FaultError, match="mysql CLI not found"):
        f.check()


def test_mysql_connection_check_caps_max(monkeypatch):
    monkeypatch.setattr(faults.shutil, "which", lambda _name: "/usr/bin/mysql")
    seen: list[str] = []

    def fake_run(fault, sql, timeout=10.0):
        seen.append(sql)
        return FakeMySQLResult(stdout="@@max_connections\n8\n")

    monkeypatch.setattr(faults, "_run_mysql", fake_run)
    with pytest.raises(FaultError, match="max_connections=8"):
        MySQLFault(mode="connection", user="u", password="p", connections=20, duration=1).check()
    assert any("max_connections" in sql for sql in seen)
    MySQLFault(mode="connection", user="u", password="p", connections=5, duration=1).check()


def test_mysql_lock_check_rejects_missing_table(monkeypatch):
    monkeypatch.setattr(faults.shutil, "which", lambda _name: "/usr/bin/mysql")
    monkeypatch.setattr(
        faults, "_run_mysql", lambda fault, sql, timeout=10.0: FakeMySQLResult(stdout="COUNT(*)\n0\n")
    )
    f = MySQLFault(mode="lock", user="u", password="p", database="app", table="orders", duration=1)
    with pytest.raises(FaultError, match="not found"):
        f.check()


def test_mysql_connection_inject_and_rollback(monkeypatch):
    spawned: list[str] = []

    def fake_spawn(fault, sql):
        spawned.append(sql)
        assert "secret" not in sql, f"password leaked into mysql sql: {sql}"
        return FakeMySQLProc()

    monkeypatch.setattr(faults, "_spawn_mysql", fake_spawn)
    f = MySQLFault(mode="connection", user="u", password="secret", connections=3, duration=5)
    f.inject()
    assert len(spawned) == 3
    assert all("SELECT SLEEP(5)" in sql for sql in spawned)
    procs = list(f._clients)
    f.recover()
    assert all(p.killed for p in procs)
    assert f._clients == []
    f.recover()  # idempotent


def test_mysql_lock_inject_holds_lock_session(monkeypatch):
    spawned: list[str] = []
    monkeypatch.setattr(faults, "_spawn_mysql", lambda fault, sql: spawned.append(sql) or FakeMySQLProc())
    f = MySQLFault(mode="lock", user="u", password="p", database="app", table="orders", duration=5)
    f.inject()
    assert len(spawned) == 1
    assert "LOCK TABLES `orders` WRITE" in spawned[0]
    assert "SLEEP(5)" in spawned[0]


def test_mysql_session_kill(monkeypatch):
    rows = (
        "ID\tUSER\tHOST\tDB\tCOMMAND\tINFO\n"
        "1\troot\tlocalhost\tapp\tQuery\tSELECT SLEEP(5)\n"
        "2\troot\tlocalhost\tapp\tSleep\tNULL\n"
        "3\tapp\t10.0.0.1\tapp\tQuery\tSELECT * FROM users\n"
    )
    seen: list[str] = []

    def fake_run(fault, sql, timeout=10.0):
        seen.append(sql)
        if "PROCESSLIST" in sql:
            assert "CONNECTION_ID()" in sql, "must exclude own connection"
            return FakeMySQLResult(stdout=rows)
        return FakeMySQLResult()

    monkeypatch.setattr(faults, "_run_mysql", fake_run)
    f = MySQLFault(mode="session", user="u", password="p", session_pattern="SLEEP")
    f.inject()
    assert f.killed() == 1
    kill_sql = seen[-1]
    assert "KILL 1" in kill_sql
    assert "KILL 3" not in kill_sql


def test_mysql_session_no_match_rejected(monkeypatch):
    rows = "ID\tUSER\tHOST\tDB\tCOMMAND\tINFO\n1\troot\tlocalhost\tapp\tQuery\tSELECT 1\n"
    monkeypatch.setattr(
        faults, "_run_mysql", lambda fault, sql, timeout=10.0: FakeMySQLResult(stdout=rows)
    )
    f = MySQLFault(mode="session", user="u", password="p", session_pattern="zzz")
    with pytest.raises(FaultError, match="no session matches"):
        f.inject()


def test_mysql_password_never_leaks():
    f = MySQLFault(mode="connection", user="root", password="s3cret", database="app")
    # repr (timeline/logging) and describe() must omit the password.
    assert "s3cret" not in repr(f)
    assert "s3cret" not in f.describe()
    # The command line must not carry it either; only MYSQL_PWD does.
    assert "s3cret" not in " ".join(faults._mysql_base_args(f))
    assert faults._mysql_env(f)["MYSQL_PWD"] == "s3cret"


def test_cli_mysql_dry_run_omits_password(capsys):
    with pytest.raises(SystemExit) as exc:
        cli.main(
            [
                "mysql",
                "--password",
                "s3cret",
                "--mode",
                "connection",
                "--connections",
                "5",
                "--duration",
                "1",
                "--dry-run",
            ]
        )
    out = capsys.readouterr().out
    assert exc.value.code == 0
    assert "[dry-run]" in out
    assert "s3cret" not in out
