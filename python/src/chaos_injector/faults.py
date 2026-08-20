"""Fault atoms (故障执行层).

Every fault atom implements the same lifecycle so the scheduler can treat
them uniformly:

    check()    -> environment awareness: platform/tool/load gates
    inject()   -> execute the fault
    recover()  -> idempotent rollback, must succeed even after partial failure

Design notes
------------
- NetworkFault shells out to Linux ``tc netem`` (the Go rewrite will talk to
  netlink directly, removing the shell dependency).
- CpuFault prefers ``stress-ng`` on Linux and falls back to a stdlib-only
  CPU burner, which keeps the tool runnable on Windows development machines.
- Recovery is idempotent: it never raises if the fault is already gone.
"""

from __future__ import annotations

import abc
import dataclasses
import multiprocessing
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, ClassVar

from . import k8s

LINUX = sys.platform.startswith("linux")


class FaultError(RuntimeError):
    """Raised when a fault cannot be injected or recovered safely."""


class EnvProbe:
    """Environment awareness layer (环境感知层).

    A deliberately small gate: refuse to inject into a host that is already
    under load. ``psutil`` is optional; without it the gate relies on
    ``os.getloadavg()`` on Linux and is skipped elsewhere.
    """

    def __init__(self, max_load: float = 4.0) -> None:
        self.max_load = max_load

    def load_ok(self) -> tuple[bool, str]:
        if LINUX:
            try:
                load1, _, _ = os.getloadavg()
            except OSError as exc:
                return True, f"load check unavailable ({exc})"
            ok = load1 < self.max_load
            return ok, f"loadavg(1m)={load1:.2f} limit={self.max_load}"
        return True, "load gate skipped on this platform"

    def summary(self) -> str:
        _, detail = self.load_ok()
        return detail


def snapshot_env() -> dict[str, Any]:
    """Capture a lightweight system-state snapshot as reproducibility evidence.

    Recorded before injection and after rollback, so two runs of the same
    experiment can be compared (same seed + same environment = stable
    reproduction). Fields unavailable on the current platform are omitted.
    """

    snap: dict[str, Any] = {
        "ts": datetime.now(timezone.utc).isoformat(timespec="milliseconds"),
        "hostname": socket.gethostname(),
        "platform": sys.platform,
        "cpu_count": os.cpu_count(),
    }
    if LINUX:
        try:
            load1, load5, load15 = os.getloadavg()
            snap["loadavg"] = [round(load1, 2), round(load5, 2), round(load15, 2)]
        except OSError:
            pass
        try:
            with open("/proc/meminfo", encoding="utf-8") as fh:
                for line in fh:
                    if line.startswith("MemAvailable:"):
                        snap["mem_available_mb"] = int(line.split()[1]) // 1024
                        break
        except OSError:
            pass
    return snap


@dataclasses.dataclass
class BaseFault(abc.ABC):
    """Common contract for all fault atoms."""

    name: ClassVar[str] = "base"
    description: ClassVar[str] = ""

    @abc.abstractmethod
    def check(self) -> None:
        """Validate that the fault can run here; raise FaultError otherwise."""

    @abc.abstractmethod
    def inject(self) -> None:
        """Execute the fault. Must be safe to call once per experiment."""

    @abc.abstractmethod
    def recover(self) -> None:
        """Roll the fault back. MUST be idempotent and never raise."""

    def describe(self) -> str:
        params = ", ".join(
            f"{field.name}={getattr(self, field.name)}"
            for field in dataclasses.fields(self)
            if field.repr
        )
        return f"{self.name}({params})"


@dataclasses.dataclass
class NetworkFault(BaseFault):
    """Network delay / packet loss via Linux ``tc netem``."""

    name: ClassVar[str] = "network"
    description: ClassVar[str] = "network delay / packet loss (Linux tc netem)"

    interface: str = "eth0"
    delay_ms: int = 200
    loss_pct: float = 0.0

    def __post_init__(self) -> None:
        if self.delay_ms < 0:
            raise FaultError("delay_ms must be >= 0")
        if not 0 <= self.loss_pct <= 100:
            raise FaultError("loss_pct must be in [0, 100]")
        if self.delay_ms == 0 and self.loss_pct == 0:
            raise FaultError("at least one of delay/loss must be set")

    def check(self) -> None:
        if not LINUX:
            raise FaultError("NetworkFault requires Linux + tc; not available on this platform")
        if not shutil.which("tc"):
            raise FaultError("tc not found in PATH (iproute2 required)")
        if os.geteuid() != 0:
            raise FaultError("NetworkFault requires root (tc netem)")

    def _netem(self) -> list[str]:
        opts = ["netem"]
        if self.delay_ms:
            opts += ["delay", f"{self.delay_ms}ms"]
        if self.loss_pct:
            opts += ["loss", f"{self.loss_pct}%"]
        return opts

    def inject(self) -> None:
        self.check()
        _run(
            [
                "tc", "qdisc", "replace", "dev", self.interface, "root", "handle", "1:",
                *self._netem(),
            ],
            "inject network fault",
        )

    def recover(self) -> None:
        if not LINUX or not shutil.which("tc"):
            return  # injection could not have happened here; nothing to roll back
        # rc=2 means the qdisc was already gone: treat as success (idempotent).
        _run(
            ["tc", "qdisc", "del", "dev", self.interface, "root"],
            "recover network fault",
            allowed_codes=(0, 2),
        )


@dataclasses.dataclass
class CpuFault(BaseFault):
    """CPU resource exhaustion (资源类故障原子).

    Linux: ``stress-ng --cpu N --cpu-load P`` when available.
    Fallback: stdlib CPU burners, one process per core, so the tool also
    runs on Windows development machines.
    """

    name: ClassVar[str] = "cpu"
    description: ClassVar[str] = "CPU load / resource exhaustion"

    load_percent: int = 80
    cores: int = 1
    _proc: subprocess.Popen[str] | None = dataclasses.field(default=None, init=False, repr=False)
    _stop: threading.Event = dataclasses.field(
        default_factory=threading.Event, init=False, repr=False
    )
    _burners: list[tuple[multiprocessing.Process, multiprocessing.Event]] = dataclasses.field(
        default_factory=list, init=False, repr=False
    )

    def __post_init__(self) -> None:
        if not 1 <= self.load_percent <= 100:
            raise FaultError("load_percent must be in [1, 100]")
        if self.cores < 1:
            raise FaultError("cores must be >= 1")

    def check(self) -> None:
        if not 1 <= self.load_percent <= 100:
            raise FaultError("load_percent must be in [1, 100]")
        self._stress_bin = shutil.which("stress-ng")
        # stress-ng is preferred, not required: the burner fallback always works.

    def inject(self) -> None:
        self.check()

        if LINUX and self._stress_bin:
            self._proc = subprocess.Popen(
                [
                    self._stress_bin,
                    "--cpu",
                    str(self.cores),
                    "--cpu-load",
                    str(self.load_percent),
                    "--quiet",
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            return

        for _ in range(self.cores):
            stop = multiprocessing.Event()
            proc = multiprocessing.Process(
                target=_burn_cpu, args=(self.load_percent, stop), daemon=True
            )
            proc.start()
            self._burners.append((proc, stop))

    def recover(self) -> None:
        if self._proc is not None:
            _terminate_process_tree(self._proc)
            self._proc = None
        for proc, stop in self._burners:
            stop.set()
            proc.join(timeout=2)
        self._burners = []


def _burn_cpu(load_percent: int, stop: threading.Event) -> None:
    """Busy-burn one core with a duty cycle approximating ``load_percent``."""
    window = 0.2
    duty = load_percent / 100.0
    while not stop.is_set():
        burn_until = time.monotonic() + window * duty
        while time.monotonic() < burn_until:
            pass
        time.sleep(window * (1 - duty))


def _run(command: list[str], label: str, allowed_codes: tuple[int, ...] = (0,)) -> None:
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if result.returncode not in allowed_codes:
        stderr = result.stderr.strip()
        raise FaultError(f"{label} failed (rc={result.returncode}): {stderr or command}")


def _terminate_process_tree(proc: subprocess.Popen[str]) -> None:
    if LINUX:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
            proc.wait(timeout=3)
            return
        except (ProcessLookupError, subprocess.TimeoutExpired):
            pass
    proc.terminate()
    try:
        proc.wait(timeout=3)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=3)


@dataclasses.dataclass
class MemFault(BaseFault):
    """Memory occupancy (chaosblade ``mem load`` equivalent).

    Allocates a buffer of ``size_mb`` MiB and keeps touching pages so they
    stay resident; recovery releases the buffer for the GC.
    """

    name: ClassVar[str] = "mem"
    description: ClassVar[str] = "memory occupancy / resource exhaustion"

    size_mb: int = 256
    _buf: bytearray | None = dataclasses.field(default=None, init=False, repr=False)
    _stop: threading.Event = dataclasses.field(
        default_factory=threading.Event, init=False, repr=False
    )
    _toucher: threading.Thread | None = dataclasses.field(default=None, init=False, repr=False)

    def __post_init__(self) -> None:
        if self.size_mb < 1:
            raise FaultError("size_mb must be >= 1")

    def check(self) -> None:
        if self.size_mb < 1:
            raise FaultError("size_mb must be >= 1")
        # Safety gate (Linux only): never allocate beyond what the host can
        # spare, or the kernel OOM killer may pick this very process.
        if LINUX:
            avail_mb = _mem_available_mb()
            if self.size_mb > avail_mb:
                raise FaultError(
                    f"size_mb={self.size_mb} exceeds available memory ({avail_mb} MB)"
                )

    def inject(self) -> None:
        self.check()
        self._buf = bytearray(self.size_mb * 1024 * 1024)
        self._stop.clear()
        self._toucher = threading.Thread(
            target=_touch_pages, args=(self._buf, self._stop), daemon=True
        )
        self._toucher.start()

    def recover(self) -> None:
        self._stop.set()
        if self._toucher is not None:
            self._toucher.join(timeout=2)
            self._toucher = None
        self._buf = None  # release for the GC


@dataclasses.dataclass
class PortFault(BaseFault):
    """TCP port occupation (chaosblade ``network port occupy`` equivalent).

    The listener grabs the port so a real service on it becomes unreachable;
    recovery closes the listener.
    """

    name: ClassVar[str] = "port"
    description: ClassVar[str] = "TCP port occupation"

    port: int = 8080
    _sock: socket.socket | None = dataclasses.field(default=None, init=False, repr=False)

    def __post_init__(self) -> None:
        if not 1 <= self.port <= 65535:
            raise FaultError("port must be in [1, 65535]")

    def check(self) -> None:
        if not 1 <= self.port <= 65535:
            raise FaultError("port must be in [1, 65535]")

    def inject(self) -> None:
        self.check()
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            sock.bind(("", self.port))
            sock.listen(1)
        except OSError as exc:
            sock.close()
            raise FaultError(f"port {self.port} already in use: {exc}") from None
        self._sock = sock

    def recover(self) -> None:
        if self._sock is not None:
            self._sock.close()
            self._sock = None


@dataclasses.dataclass
class ProcessFault(BaseFault):
    """Terminate processes matching a pattern (chaosblade ``process kill``
    equivalent). An instantaneous fault: killed processes cannot come back,
    so ``recover`` is a no-op.

    Safety boundary: the current process and its ancestors are never matched,
    so the injector cannot kill itself or its parent shell.
    """

    name: ClassVar[str] = "process"
    description: ClassVar[str] = "terminate processes matching a pattern"

    pattern: str = ""
    _targets: list[int] = dataclasses.field(default_factory=list, init=False, repr=False)

    def check(self) -> None:
        if not LINUX:
            raise FaultError("ProcessFault requires Linux (/proc scan); not available here")
        if not self.pattern.strip():
            raise FaultError("pattern must not be empty")
        self._targets = _match_processes(self.pattern)
        if not self._targets:
            raise FaultError(f"no process matches pattern {self.pattern!r}")

    def inject(self) -> None:
        self.check()
        killed = 0
        for pid in self._targets:
            try:
                os.kill(pid, signal.SIGKILL)
                killed += 1
            except ProcessLookupError:
                pass
        if killed == 0:
            raise FaultError("no matching process could be killed (already gone?)")

    def recover(self) -> None:
        # Idempotent by design: an instantaneous kill has nothing to undo.
        return


@dataclasses.dataclass
class PodKillFault(BaseFault):
    """Delete a Kubernetes pod (chaosblade ``k8s pod-kill`` equivalent).

    Deleting a pod is not reversible, so ``recover`` is a no-op: the
    intended rollback is Kubernetes self-healing (the Deployment recreates
    the pod). With ``wait_ready`` set, inject blocks until a replacement pod
    is Ready, turning the experiment into a self-healing verification.
    """

    name: ClassVar[str] = "pod-kill"
    description: ClassVar[str] = "delete a Kubernetes pod (self-healing chaos)"

    namespace: str = "default"
    pod: str = ""
    selector: str = ""
    count: int = 1
    wait_ready: float = 60.0
    _targets: list[str] = dataclasses.field(default_factory=list, init=False, repr=False)
    _healed: str = dataclasses.field(default="", init=False, repr=False)

    def check(self) -> None:
        if not self.namespace:
            raise FaultError("namespace must not be empty")
        if bool(self.pod) == bool(self.selector):
            raise FaultError("exactly one of pod or selector must be set")
        self.count = max(self.count, 1)  # normalize: at least one pod
        if self.wait_ready > 0 and not self.selector:
            raise FaultError(
                "wait-ready requires a label selector (a replacement pod cannot be matched by name)"
            )
        if not k8s.available():
            raise FaultError("kubectl unavailable (kubectl or minikube required)")
        if self.pod:
            if not k8s.pod_exists(self.namespace, self.pod):
                raise FaultError(f"pod {self.namespace}/{self.pod} not found")
            self._targets = [self.pod]
            return
        names = k8s.get_pod_names(self.namespace, self.selector)
        if not names:
            raise FaultError(
                f"no pod matches selector {self.selector!r} in namespace {self.namespace}"
            )
        self._targets = names[: self.count]

    def inject(self) -> None:
        self.check()
        for name in self._targets:
            k8s.delete_pod(self.namespace, name)
        if self.wait_ready > 0:
            self._healed = k8s.wait_for_replacement(
                self.namespace, self.selector, self._targets[0], self.wait_ready
            )

    def recover(self) -> None:
        # No-op by design: a deleted pod cannot be restored; the recovery is
        # Kubernetes self-healing, which is what the experiment verifies.
        return

    def healed(self) -> str:
        return self._healed


def _touch_pages(buf: bytearray, stop: threading.Event) -> None:
    """Write one byte per page so the kernel backs the allocation with
    physical memory (dirty pages stay resident)."""
    page_size = 4096
    while not stop.is_set():
        for i in range(0, len(buf), page_size):
            buf[i] = i & 0xFF
        stop.wait(0.05)


def _mem_available_mb() -> int:
    """Return free memory in MB from /proc/meminfo (Linux only)."""
    try:
        with open("/proc/meminfo", encoding="utf-8") as fh:
            for line in fh:
                if line.startswith("MemAvailable:"):
                    return int(line.split()[1]) // 1024
    except (OSError, ValueError):
        pass
    raise FaultError("cannot read /proc/meminfo")


def _match_processes(pattern: str) -> list[int]:
    """Scan /proc for pids whose comm or cmdline contains ``pattern``,
    excluding the current process and its ancestor chain."""
    protected = _protected_pids()
    targets: list[int] = []
    try:
        entries = os.listdir("/proc")
    except OSError as exc:
        raise FaultError(f"cannot scan /proc: {exc}") from None
    for name in entries:
        if not name.isdigit():
            continue
        pid = int(name)
        if pid in protected:
            continue
        try:
            comm = Path(f"/proc/{pid}/comm").read_text(encoding="utf-8").strip()
            cmdline = Path(f"/proc/{pid}/cmdline").read_bytes().decode("utf-8", "ignore")
        except OSError:
            continue  # process exited while scanning
        if pattern in comm or pattern in cmdline.replace("\x00", " "):
            targets.append(pid)
    return targets


def _protected_pids() -> set[int]:
    """Pids that must never be targeted: this process and its ancestors
    (walked via /proc/<pid>/status PPid)."""
    protected: set[int] = set()
    pid = os.getpid()
    while pid > 1:
        protected.add(pid)
        try:
            status = Path(f"/proc/{pid}/status").read_text(encoding="utf-8")
        except OSError:
            break
        ppid = -1
        for line in status.splitlines():
            if line.startswith("PPid:"):
                ppid = int(line.split()[1])
                break
        if ppid == pid or ppid < 1:
            break
        pid = ppid
    return protected


# -- mysql: database-instance faults ----------------------------------------


_IDENT_RE = re.compile(r"^[A-Za-z0-9_]+$")


def _mysql_base_args(fault: MySQLFault) -> list[str]:
    args = [
        "mysql",
        f"-h{fault.host}",
        f"-P{fault.port}",
        f"-u{fault.user}",
        "--connect-timeout=5",
    ]
    if fault.database:
        args.append(f"-D{fault.database}")
    return args


def _mysql_env(fault: MySQLFault) -> dict[str, str]:
    """Password travels via MYSQL_PWD so it never hits the command line."""
    env = dict(os.environ)
    env["MYSQL_PWD"] = fault.password
    return env


def _run_mysql(fault: MySQLFault, sql: str, timeout: float = 10.0) -> subprocess.CompletedProcess:
    """Run one mysql -e batch; swappable in tests (no real DB needed)."""
    return subprocess.run(
        _mysql_base_args(fault) + ["-e", sql],
        env=_mysql_env(fault),
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def _spawn_mysql(fault: MySQLFault, sql: str) -> subprocess.Popen:
    """Start a background mysql session that holds a connection; swappable."""
    return subprocess.Popen(
        _mysql_base_args(fault) + ["-e", sql],
        env=_mysql_env(fault),
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


def _mysql_last_line(stdout: str) -> str:
    """The value row of a mysql -e result (after the column-name header)."""
    for line in reversed(stdout.strip().splitlines()):
        if line.strip():
            return line.strip()
    return ""


@dataclasses.dataclass
class MySQLFault(BaseFault):
    """Database-instance faults via the mysql CLI client.

    Modes (chaosblade ``blade create mysql`` equivalents):
    - ``connection``: occupy ``connections`` slots of the connection pool;
    - ``lock``: hold ``LOCK TABLES t WRITE``, blocking writes to the table;
    - ``session``: kill sessions matching user / db / command / SQL pattern.

    The password travels via the ``MYSQL_PWD`` environment variable and is
    never part of ``describe()``, the timeline, or any log output.
    """

    name: ClassVar[str] = "mysql"
    description: ClassVar[str] = (
        "database instance faults via mysql client (connection/lock/session)"
    )

    host: str = "127.0.0.1"
    port: int = 3306
    user: str = "root"
    password: str = dataclasses.field(default="", repr=False)
    database: str = ""
    mode: str = "connection"
    connections: int = 20
    table: str = ""
    session_user: str = ""
    session_db: str = ""
    session_command: str = ""
    session_pattern: str = ""
    # Used to size the SLEEP() of holder sessions; the experiment timer also
    # kills the holders at the same deadline.
    duration: float = 0.0

    _clients: list[subprocess.Popen] = dataclasses.field(
        default_factory=list, init=False, repr=False
    )
    _killed: int = dataclasses.field(default=0, init=False, repr=False)

    def check(self) -> None:
        if self.mode not in ("connection", "lock", "session"):
            raise FaultError(f"unsupported mysql mode {self.mode!r} (connection|lock|session)")
        if self.port == 0:
            self.port = 3306  # zero value means "unset", not an invalid port
        if not 1 <= self.port <= 65535:
            raise FaultError("port must be in [1, 65535]")
        if not self.user or not self.password:
            raise FaultError("user and password are required")
        if not shutil.which("mysql"):
            raise FaultError("mysql CLI not found: install the mysql client")
        if self.mode == "connection":
            if self.connections < 1:
                raise FaultError("connections must be >= 1")
            if self.duration <= 0:
                raise FaultError("duration must be > 0 for connection mode")
            # Never occupy more slots than the server allows.
            res = _run_mysql(self, "SELECT @@max_connections")
            if res.returncode != 0:
                raise FaultError(
                    f"cannot connect to mysql at {self.host}:{self.port} ({res.stderr.strip()})"
                )
            max_conn = int(_mysql_last_line(res.stdout))
            if self.connections >= max_conn:
                raise FaultError(
                    f"connections={self.connections} must be < max_connections={max_conn}"
                )
        elif self.mode == "lock":
            if not self.database or not self.table:
                raise FaultError("lock mode requires database and table")
            if not _IDENT_RE.fullmatch(self.database) or not _IDENT_RE.fullmatch(self.table):
                raise FaultError("database/table must match [A-Za-z0-9_]+")
            if self.duration <= 0:
                raise FaultError("duration must be > 0 for lock mode")
            res = _run_mysql(
                self,
                "SELECT COUNT(*) FROM information_schema.tables "
                f"WHERE table_schema='{self.database}' AND table_name='{self.table}'",
            )
            if res.returncode != 0:
                raise FaultError(
                    f"cannot connect to mysql at {self.host}:{self.port} ({res.stderr.strip()})"
                )
            if _mysql_last_line(res.stdout) == "0":
                raise FaultError(f"table {self.database}.{self.table} not found")
        elif not any(
            [self.session_user, self.session_db, self.session_command, self.session_pattern]
        ):
            raise FaultError(
                "session mode requires at least one of "
                "session-user/session-db/session-command/session-pattern"
            )

    def inject(self) -> None:
        if self.mode == "connection":
            self._inject_connection()
        elif self.mode == "lock":
            self._inject_lock()
        else:
            self._inject_session()

    def _inject_connection(self) -> None:
        sleep_sql = f"SELECT SLEEP({int(self.duration)})"
        for i in range(self.connections):
            try:
                self._clients.append(_spawn_mysql(self, sleep_sql))
            except OSError as exc:
                self.recover()
                raise FaultError(
                    f"failed to open mysql connection {i + 1}/{self.connections}: {exc}"
                )

    def _inject_lock(self) -> None:
        lock_sql = f"LOCK TABLES `{self.table}` WRITE; SELECT SLEEP({int(self.duration)})"
        try:
            self._clients.append(_spawn_mysql(self, lock_sql))
        except OSError as exc:
            self.recover()
            raise FaultError(f"failed to acquire table lock: {exc}")

    def _inject_session(self) -> None:
        res = _run_mysql(
            self,
            "SELECT ID, USER, HOST, DB, COMMAND, INFO FROM information_schema.PROCESSLIST "
            "WHERE ID <> CONNECTION_ID()",
        )
        if res.returncode != 0:
            raise FaultError(f"cannot query processlist: {res.stderr.strip()}")
        ids: list[str] = []
        for line in res.stdout.strip().splitlines()[1:]:  # skip header
            fields = line.split("\t")
            if len(fields) < 6:
                continue
            sid, suser, sdb, scmd = fields[0], fields[1], fields[3], fields[4]
            sinfo = "\t".join(fields[5:])
            if self.session_user and suser != self.session_user:
                continue
            if self.session_db and sdb != self.session_db:
                continue
            if self.session_command and scmd != self.session_command:
                continue
            if self.session_pattern and self.session_pattern not in sinfo:
                continue
            ids.append(sid)
        if not ids:
            raise FaultError("no session matches the given criteria")
        kill = _run_mysql(self, "; ".join(f"KILL {sid}" for sid in ids))
        if kill.returncode != 0:
            raise FaultError(f"failed to kill sessions: {kill.stderr.strip()}")
        self._killed = len(ids)

    def killed(self) -> int:
        """How many sessions were terminated (session mode only)."""
        return self._killed

    def recover(self) -> None:
        # Killing the holder processes ends their sessions, which releases
        # both the occupied connections and the table lock. Session-mode
        # kills are irreversible (like the process atom): nothing to roll
        # back. Idempotent: already-exited clients are skipped.
        for proc in self._clients:
            if proc.poll() is None:
                proc.kill()
        self._clients.clear()


FAULTS: dict[str, type[BaseFault]] = {
    NetworkFault.name: NetworkFault,
    CpuFault.name: CpuFault,
    MemFault.name: MemFault,
    PortFault.name: PortFault,
    ProcessFault.name: ProcessFault,
    PodKillFault.name: PodKillFault,
    MySQLFault.name: MySQLFault,
}


def build_fault(kind: str, **params) -> BaseFault:
    try:
        cls = FAULTS[kind]
    except KeyError:
        raise FaultError(f"unknown fault kind {kind!r}; available: {sorted(FAULTS)}") from None
    return cls(**params)
