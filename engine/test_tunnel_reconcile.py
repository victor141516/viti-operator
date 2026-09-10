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

    def test_initial_inner_address_is_recorded_without_restarting_asterisk(self):
        with mock.patch.object(self.subject, "_call", return_value=1) as call:
            self.assertTrue(self.subject.apply_inner_ip("2001:db8::10"))
        self.assertEqual(
            (Path(self.temp.name) / "inner_ip.applied").read_text(),
            "2001:db8::10",
        )
        call.assert_called_once_with(["asterisk", "-rx", "core show uptime"])

    def test_changed_inner_address_restarts_and_restores_registration(self):
        (Path(self.temp.name) / "inner_ip.applied").write_text("2001:db8::10")
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
            (Path(self.temp.name) / "inner_ip.applied").read_text(),
            "2001:db8::20",
        )

    def test_unchanged_inner_address_is_a_noop(self):
        (Path(self.temp.name) / "inner_ip.applied").write_text("2001:db8::10")
        with mock.patch.object(self.subject, "_call") as call:
            self.assertFalse(self.subject.apply_inner_ip("2001:db8::10"))
        call.assert_not_called()

    def test_pcscf_change_renders_reloads_and_registers(self):
        (Path(self.temp.name) / "pcscf.applied").write_text("2001:db8::10")
        with mock.patch.object(self.subject, "_call", return_value=0) as call:
            self.assertTrue(self.subject.apply_pcscf("2001:db8::20"))
        self.assertEqual([item.args[0] for item in call.call_args_list], [
            ["asterisk", "-rx", "core show uptime"],
            ["python3", str(self.render)],
            ["asterisk", "-rx", "module reload res_pjsip.so"],
            ["asterisk", "-rx", "pjsip send register volte_ims"],
        ])
        self.assertEqual(
            (Path(self.temp.name) / "pcscf.applied").read_text(),
            "2001:db8::20",
        )

    def test_failed_reconcile_does_not_advance_marker(self):
        with mock.patch.object(self.subject, "_call", side_effect=[0, 1]):
            with self.assertRaisesRegex(RuntimeError, "render failed"):
                self.subject.apply_pcscf("2001:db8::20")
        self.assertFalse((Path(self.temp.name) / "pcscf.applied").exists())

    def test_initial_pcscf_is_recorded_without_rendering_before_asterisk(self):
        with mock.patch.object(self.subject, "_call", return_value=1) as call:
            self.assertTrue(self.subject.apply_pcscf("2001:db8::20"))
        self.assertEqual(
            (Path(self.temp.name) / "pcscf.applied").read_text(),
            "2001:db8::20",
        )
        call.assert_called_once_with(["asterisk", "-rx", "core show uptime"])


if __name__ == "__main__":
    unittest.main()
