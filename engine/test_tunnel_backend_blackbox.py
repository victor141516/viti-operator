"""External-process contract tests for every SWu tunnel backend.

These tests never import ``swu_ike`` or inspect its objects.  Protocol-success
scenarios live in the deterministic ePDG lab; this file establishes the common
process/filesystem boundary and exercises a bounded failed-attach lifecycle.
"""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import tunnel_contract


class PythonTunnelBackendBlackBoxTest(unittest.TestCase):
    def test_failed_attach_publishes_one_generation_and_terminal_status(self):
        backend = Path(__file__).with_name("swu_ike.py")
        with tempfile.TemporaryDirectory() as directory:
            rundir = Path(directory)
            env = dict(os.environ)
            env.update({
                "VOWIFI_RUNDIR": str(rundir),
                "VITI_EVENT_OUTBOX_DIR": str(rundir / "event-outbox"),
                "SWU_CP_MODE": "v6",
                "SWU_IKE_RETRANS_TIMEOUTS": "0.1",
                "SWU_REDACT_SECRETS": "1",
            })
            result = subprocess.run(
                [
                    sys.executable, str(backend),
                    "-s", "127.0.0.1",
                    "-d", "127.0.0.1",
                    "-a", "ims",
                    "-I", "001010123456789",
                    "-M", "001",
                    "-N", "001",
                    "-K", "00000000000000000000000000000000",
                    "-C", "00000000000000000000000000000000",
                ],
                env=env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=15,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            status = tunnel_contract.read_status(rundir)
            self.assertEqual(status.backend, "python")
            self.assertEqual(status.state, "DOWN")

            events = []
            for path in sorted((rundir / "event-outbox").glob("*.json")):
                events.append(json.loads(path.read_text(encoding="utf-8")))
            tunnel_contract.validate_transition_sequence(events)
            self.assertEqual(
                [event["event"] for event in events],
                ["tunnel_start", "tunnel_down"],
            )


if __name__ == "__main__":
    unittest.main()
