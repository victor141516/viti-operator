#!/usr/bin/env python3
"""Apply tunnel endpoint changes to the long-running Asterisk process."""

import os
import json
from pathlib import Path
import subprocess
import time


class TunnelReconciler:
    MARKER_FILE = "tunnel.applied.json"
    MARKER_KEYS = {
        "inner_ip.applied": "inner_ip",
        "pcscf.applied": "pcscf",
    }

    def __init__(self, rundir="/run/vowifi", render="/usr/local/bin/render.py", logger=None):
        self.rundir = Path(rundir)
        self.render = Path(render)
        self.log = logger or (lambda _message: None)

    def _read_marker(self, name):
        key = self.MARKER_KEYS[name]
        try:
            payload = json.loads(
                (self.rundir / self.MARKER_FILE).read_text(encoding="utf-8")
            )
            value = payload.get(key, "")
            return value if isinstance(value, str) else ""
        except (OSError, ValueError, TypeError):
            return ""

    def _write_marker(self, name, value):
        self._write_markers(**{self.MARKER_KEYS[name]: value})

    def _write_markers(self, **updates):
        """Atomically replace the complete applied tunnel observation."""
        self.rundir.mkdir(parents=True, exist_ok=True)
        current = {
            "inner_ip": self._read_marker("inner_ip.applied"),
            "pcscf": self._read_marker("pcscf.applied"),
        }
        current.update(updates)
        path = self.rundir / self.MARKER_FILE
        temporary = path.with_suffix(path.suffix + ".tmp")
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(current, output, separators=(",", ":"), sort_keys=True)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)

    def record_initial(self, inner_ip, pcscf):
        """Commit the initial observation after entrypoint rendering succeeds."""
        updates = {}
        if inner_ip:
            updates["inner_ip"] = inner_ip
        if pcscf:
            updates["pcscf"] = pcscf
        if updates:
            self._write_markers(**updates)

    @staticmethod
    def _call(command):
        try:
            return subprocess.call(
                command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
            )
        except OSError:
            # Asterisk is intentionally absent from the protocol-only lab and
            # is not yet running during the production engine's first attach.
            return 127

    def apply_pcscf(self, address):
        """Reload PJSIP only when the carrier-assigned P-CSCF changes."""
        if not address or self._read_marker("pcscf.applied") == address:
            return False
        if not self.render.exists():
            return False
        previous = self._read_marker("pcscf.applied") or "unset"
        if self._call(["asterisk", "-rx", "core show uptime"]) != 0:
            # The entrypoint renders and atomically records the complete first
            # observation before starting Asterisk.  Never consume it here.
            return False
        self.log(f"P-CSCF changed ({previous} -> {address}); reloading PJSIP")
        if self._call(["python3", str(self.render)]) != 0:
            raise RuntimeError("render failed while applying the P-CSCF")
        if self._call(["asterisk", "-rx", "module reload res_pjsip.so"]) != 0:
            raise RuntimeError("PJSIP reload failed while applying the P-CSCF")
        if self._call(["asterisk", "-rx", "pjsip send register volte_ims"]) != 0:
            raise RuntimeError("IMS registration failed after applying the P-CSCF")
        self._write_marker("pcscf.applied", address)
        return True

    def apply_inner_ip(self, address):
        """Restart the non-reloadable PJSIP transport after an inner-IP change."""
        if not address:
            return False
        previous = self._read_marker("inner_ip.applied")
        if previous == address:
            return False

        asterisk_running = self._call(["asterisk", "-rx", "core show uptime"]) == 0
        if asterisk_running:
            self.log(
                f"tunnel inner IP changed ({previous or 'unset'} -> {address}); "
                "restarting the Asterisk transport"
            )
            if self._call(["asterisk", "-rx", "core restart now"]) != 0:
                raise RuntimeError("Asterisk core restart failed after inner-IP change")

            for _ in range(60):
                time.sleep(0.5)
                try:
                    ready = subprocess.check_output(
                        ["asterisk", "-rx", "pjsip show registrations"],
                        stderr=subprocess.DEVNULL,
                    )
                except (OSError, subprocess.SubprocessError):
                    continue
                if b"volte_ims" in ready and self._call(
                    ["asterisk", "-rx", "pjsip send register volte_ims"]
                ) == 0:
                    break
            else:
                raise RuntimeError("Asterisk did not recover after inner-IP change")

        if not asterisk_running:
            return False
        self._write_marker("inner_ip.applied", address)
        return True

    def apply(self, inner_ip, pcscf):
        """Apply a tunnel observation without exposing a half-configured IMS stack.

        A first attach may complete after Asterisk has already started without a
        P-CSCF.  In that case both the transport address and all IMS objects must
        be installed by one restart: rendering after the restart would leave no
        ``volte_ims`` registration for the recovery wait to observe.
        """
        inner_changed = bool(
            inner_ip and self._read_marker("inner_ip.applied") != inner_ip
        )
        pcscf_changed = bool(
            pcscf and self._read_marker("pcscf.applied") != pcscf
        )
        if not inner_changed and not pcscf_changed:
            return False

        asterisk_running = self._call(["asterisk", "-rx", "core show uptime"]) == 0
        if not asterisk_running:
            # The entrypoint renders and atomically records the complete first
            # observation before starting Asterisk.  Repeated monitor samples
            # remain pending until that transaction commits.
            return False

        if inner_changed and pcscf_changed:
            if not self.render.exists():
                raise RuntimeError("render is unavailable while applying tunnel changes")
            self.log(
                "tunnel endpoints changed "
                f"(inner={inner_ip}, pcscf={pcscf}); rendering and restarting Asterisk"
            )
            if self._call(["python3", str(self.render)]) != 0:
                raise RuntimeError("render failed while applying tunnel changes")
            if self._call(["asterisk", "-rx", "core restart now"]) != 0:
                raise RuntimeError("Asterisk core restart failed after tunnel changes")

            for _ in range(60):
                time.sleep(0.5)
                try:
                    ready = subprocess.check_output(
                        ["asterisk", "-rx", "pjsip show registrations"],
                        stderr=subprocess.DEVNULL,
                    )
                except (OSError, subprocess.SubprocessError):
                    continue
                if b"volte_ims" in ready and self._call(
                    ["asterisk", "-rx", "pjsip send register volte_ims"]
                ) == 0:
                    break
            else:
                raise RuntimeError("Asterisk did not recover after tunnel changes")

            # Advance neither marker until render, restart and registration all
            # succeed, so a failed transaction is retried in full.
            self._write_markers(inner_ip=inner_ip, pcscf=pcscf)
            return True

        if inner_changed:
            return self.apply_inner_ip(inner_ip)
        return self.apply_pcscf(pcscf)
