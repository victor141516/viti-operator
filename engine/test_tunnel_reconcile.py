import tempfile
import unittest
from pathlib import Path
from unittest import mock

import tunnel_reconcile


class TunnelReconcilerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.render = Path(self.temp.name) / "render.py"
        self.render.write_text("# fixture\n", encoding="ascii")
        self.subject = tunnel_reconcile.TunnelReconciler(self.temp.name, self.render)

    def tearDown(self):
        self.temp.cleanup()

    def test_initial_inner_address_remains_pending_until_entrypoint_commit(self):
        with mock.patch.object(self.subject, "_call", return_value=1) as call:
            self.assertFalse(self.subject.apply_inner_ip("2001:db8::10"))
        self.assertFalse((Path(self.temp.name) / self.subject.MARKER_FILE).exists())
        call.assert_called_once_with(["asterisk", "-rx", "core show uptime"])

    def test_changed_inner_address_restarts_and_restores_registration(self):
        self.subject.record_initial("2001:db8::10", "")
        calls = []

        def call(command):
            calls.append(command[-1])
            return 0

        with mock.patch.object(self.subject, "_call", side_effect=call), mock.patch.object(
            tunnel_reconcile.subprocess,
            "check_output",
            return_value=b"volte_ims Registered",
        ), mock.patch.object(tunnel_reconcile.time, "sleep"):
            self.assertTrue(self.subject.apply_inner_ip("2001:db8::20"))

        self.assertEqual(calls, [
            "core show uptime", "core restart now", "pjsip send register volte_ims",
        ])
        self.assertEqual(
            self.subject._read_marker("inner_ip.applied"),
            "2001:db8::20",
        )

    def test_late_first_observation_renders_ims_before_restarting_asterisk(self):
        calls = []
        rendered = False

        def call(command):
            nonlocal rendered
            calls.append(command[-1])
            if command[0] == "python3":
                rendered = True
            return 0

        def registrations(_command, **_kwargs):
            self.assertTrue(rendered, "IMS objects must be rendered before recovery wait")
            return b"volte_ims Registered"

        with mock.patch.object(self.subject, "_call", side_effect=call), mock.patch.object(
            tunnel_reconcile.subprocess,
            "check_output",
            side_effect=registrations,
        ), mock.patch.object(tunnel_reconcile.time, "sleep"):
            self.assertTrue(self.subject.apply("2001:db8::20", "2001:db8::30"))

        self.assertEqual(calls, [
            "core show uptime",
            str(self.render),
            "core restart now",
            "pjsip send register volte_ims",
        ])
        self.assertEqual(
            self.subject._read_marker("inner_ip.applied"),
            "2001:db8::20",
        )
        self.assertEqual(
            self.subject._read_marker("pcscf.applied"),
            "2001:db8::30",
        )

    def test_failed_combined_reconcile_advances_neither_marker(self):
        with mock.patch.object(self.subject, "_call", side_effect=[0, 0, 1]):
            with self.assertRaisesRegex(RuntimeError, "core restart failed"):
                self.subject.apply("2001:db8::20", "2001:db8::30")
        self.assertFalse((Path(self.temp.name) / self.subject.MARKER_FILE).exists())

    def test_combined_marker_replace_failure_preserves_previous_observation(self):
        self.subject.record_initial("2001:db8::10", "2001:db8::11")
        with mock.patch.object(tunnel_reconcile.os, "replace", side_effect=OSError("disk")):
            with self.assertRaisesRegex(OSError, "disk"):
                self.subject.record_initial("2001:db8::20", "2001:db8::30")
        self.assertEqual(
            self.subject._read_marker("inner_ip.applied"), "2001:db8::10"
        )
        self.assertEqual(
            self.subject._read_marker("pcscf.applied"), "2001:db8::11"
        )

    def test_unchanged_inner_address_is_a_noop(self):
        self.subject.record_initial("2001:db8::10", "")
        with mock.patch.object(self.subject, "_call") as call:
            self.assertFalse(self.subject.apply_inner_ip("2001:db8::10"))
        call.assert_not_called()

    def test_pcscf_change_renders_reloads_and_registers(self):
        self.subject.record_initial("", "2001:db8::10")
        with mock.patch.object(self.subject, "_call", return_value=0) as call:
            self.assertTrue(self.subject.apply_pcscf("2001:db8::20"))
        self.assertEqual([item.args[0] for item in call.call_args_list], [
            ["asterisk", "-rx", "core show uptime"],
            ["python3", str(self.render)],
            ["asterisk", "-rx", "module reload res_pjsip.so"],
            ["asterisk", "-rx", "pjsip send register volte_ims"],
        ])
        self.assertEqual(
            self.subject._read_marker("pcscf.applied"),
            "2001:db8::20",
        )

    def test_failed_reconcile_does_not_advance_marker(self):
        with mock.patch.object(self.subject, "_call", side_effect=[0, 1]):
            with self.assertRaisesRegex(RuntimeError, "render failed"):
                self.subject.apply_pcscf("2001:db8::20")
        self.assertFalse((Path(self.temp.name) / self.subject.MARKER_FILE).exists())

    def test_initial_pcscf_remains_pending_until_entrypoint_commit(self):
        with mock.patch.object(self.subject, "_call", return_value=1) as call:
            self.assertFalse(self.subject.apply_pcscf("2001:db8::20"))
        self.assertFalse((Path(self.temp.name) / self.subject.MARKER_FILE).exists())
        call.assert_called_once_with(["asterisk", "-rx", "core show uptime"])


if __name__ == "__main__":
    unittest.main()
