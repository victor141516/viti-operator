#!/usr/bin/env python3
"""Apply tunnel endpoint changes to the long-running Asterisk process."""

import os
from pathlib import Path
import subprocess
import time


class TunnelReconciler:
    def __init__(self, rundir="/run/vowifi", render="/usr/local/bin/render.py", logger=None):
        self.rundir = Path(rundir)
        self.render = Path(render)
        self.log = logger or (lambda _message: None)

    def _read_marker(self, name):
        try:
            return (self.rundir / name).read_text(encoding="ascii").strip()
        except OSError:
            return ""

    def _write_marker(self, name, value):
        self.rundir.mkdir(parents=True, exist_ok=True)
        path = self.rundir / name
        temporary = path.with_suffix(path.suffix + ".tmp")
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(descriptor, "w", encoding="ascii") as output:
            output.write(value)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)

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
            # Initial attach happens before Asterisk starts.  The entrypoint
            # performs the first render after it observes the P-CSCF; recording
            # it here avoids treating that initial assignment as a live change.
            self._write_marker("pcscf.applied", address)
            return True
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
        if asterisk_running and previous:
            self.log(
                f"tunnel inner IP changed ({previous} -> {address}); "
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

        self._write_marker("inner_ip.applied", address)
        return True

    def apply(self, inner_ip, pcscf):
        # Restart first: a subsequent PJSIP reload/REGISTER must target the new transport.
        inner_changed = self.apply_inner_ip(inner_ip)
        pcscf_changed = self.apply_pcscf(pcscf)
        return inner_changed or pcscf_changed
