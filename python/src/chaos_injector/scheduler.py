"""Experiment scheduler (恢复与监控层).

Owns the inject -> observe window -> auto-rollback lifecycle. Rollback is
guaranteed through three redundant paths, mirroring the safety design of the
industrial platform:

    1. a ``threading.Timer`` auto-recovery after the configured duration;
    2. a context manager so ``KeyboardInterrupt`` / exceptions always recover;
    3. an explicit :meth:`Experiment.recover` for manual stop.

Every state transition is recorded on a timeline (check/inject/recover/error),
which can be persisted as JSON evidence for later analysis.
"""

from __future__ import annotations

import contextlib
import json
import threading
import time
from collections.abc import Callable
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .faults import BaseFault, FaultError, snapshot_env

EventCallback = Callable[[dict[str, Any]], None]


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


class Experiment:
    """One fault injection experiment with guaranteed rollback."""

    def __init__(
        self,
        name: str,
        fault: BaseFault,
        duration: float,
        on_event: EventCallback | None = None,
        seed: int | None = None,
    ) -> None:
        if duration <= 0:
            raise ValueError("duration must be > 0")
        self.name = name
        self.fault = fault
        self.duration = duration
        self.on_event = on_event
        self.seed = seed  # PRNG seed of the random-decision layer, if any
        self.snapshot_before: dict[str, Any] = {}
        self.snapshot_after: dict[str, Any] = {}
        self.timeline: list[dict[str, Any]] = []
        self._timer: threading.Timer | None = None
        self._recovered = False

    # -- lifecycle ---------------------------------------------------------

    def start(self) -> None:
        self.snapshot_before = snapshot_env()
        self._record("start", f"fault={self.fault.describe()} duration={self.duration}s")
        try:
            self.fault.check()
        except FaultError as exc:
            self._record("error", str(exc))
            raise
        self._record("check", "preconditions ok")
        self.fault.inject()
        self._record("inject", f"fault={self.fault.name} active")
        self._timer = threading.Timer(self.duration, self._auto_recover)
        self._timer.daemon = True
        self._timer.start()
        self._record("armed", f"auto-recovery in {self.duration:.1f}s")

    def recover(self) -> None:
        """Roll the fault back. Safe to call any number of times."""
        if self._recovered:
            return
        self._recovered = True
        if self._timer is not None:
            self._timer.cancel()
        self.fault.recover()
        self.snapshot_after = snapshot_env()
        self._record("recover", f"fault={self.fault.name} rolled back")

    def _auto_recover(self) -> None:
        self.recover()
        self._record("auto", "auto-recovery fired by timer")

    def run(self) -> list[dict[str, Any]]:
        """Block until the auto-recovery timer fires, then confirm rollback."""
        self.start()
        try:
            deadline = time.monotonic() + self.duration + 2.0  # safety bound
            while not self._recovered and time.monotonic() < deadline:
                time.sleep(0.05)
        finally:
            self.recover()
        self._record("done", "experiment finished")
        return self.timeline

    # -- context manager ---------------------------------------------------

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        # Interruptions and exceptions must never leave a fault behind.
        self.recover()

    # -- timeline ----------------------------------------------------------

    def _record(self, phase: str, detail: str) -> None:
        event = {"at": _utc_now(), "phase": phase, "detail": detail}
        self.timeline.append(event)
        if self.on_event is not None:
            self.on_event(event)

    def write_timeline(self, target: Path) -> None:
        target.parent.mkdir(parents=True, exist_ok=True)
        payload = {
            "experiment": self.name,
            "fault": self.fault.describe(),
            "duration_sec": self.duration,
            "recovered": self._recovered,
            "snapshot": {"before": self.snapshot_before, "after": self.snapshot_after},
            "events": self.timeline,
        }
        if self.seed is not None:
            payload["seed"] = self.seed
        target.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")


@contextlib.contextmanager
def experiment(name: str, fault: BaseFault, duration: float, on_event: EventCallback | None = None):
    """Convenience context manager: auto-recovery on any exit path."""
    exp = Experiment(name, fault, duration, on_event)
    exp.start()
    try:
        yield exp
    finally:
        exp.recover()
