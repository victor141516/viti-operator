import importlib.util
import tempfile
import unittest
from pathlib import Path
from unittest import mock


class InnerAddressRefreshTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        path = Path(__file__).with_name("swu_ike.py")
        spec = importlib.util.spec_from_file_location("swu_ike_tested", path)
        cls.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.module)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.module.SWU_RUNDIR = self.temp.name

    def tearDown(self):
        self.temp.cleanup()

    def test_initial_address_is_recorded_without_restarting_asterisk(self):
        with mock.patch.object(self.module.subprocess, "call", return_value=1) as call:
            self.module.swu_apply_inner_ip("2001:db8::10")

        self.assertEqual(
            (Path(self.temp.name) / "inner_ip.applied").read_text(),
            "2001:db8::10",
        )
        call.assert_called_once_with(
            ["asterisk", "-rx", "core show uptime"],
            stdout=self.module.subprocess.DEVNULL,
            stderr=self.module.subprocess.DEVNULL,
        )

    def test_changed_address_restarts_core_and_restores_registration(self):
        marker = Path(self.temp.name) / "inner_ip.applied"
        marker.write_text("2001:db8::10")

        def call(command, **_kwargs):
            if command[-1] == "core show uptime":
                return 0
            if command[-1] == "core restart now":
                return 0
            if command[-1] == "pjsip send register volte_ims":
                return 0
            self.fail("unexpected command: %r" % command)

        with mock.patch.object(self.module.subprocess, "call", side_effect=call) as calls, \
                mock.patch.object(
                    self.module.subprocess,
                    "check_output",
                    return_value=b"volte_ims Registered",
                ), \
                mock.patch.object(self.module.time, "sleep"):
            self.module.swu_apply_inner_ip("2001:db8::20")

        self.assertEqual(marker.read_text(), "2001:db8::20")
        self.assertEqual(calls.call_count, 3)

    def test_unchanged_address_is_a_noop(self):
        marker = Path(self.temp.name) / "inner_ip.applied"
        marker.write_text("2001:db8::10")

        with mock.patch.object(self.module.subprocess, "call") as call:
            self.module.swu_apply_inner_ip("2001:db8::10")

        call.assert_not_called()


if __name__ == "__main__":
    unittest.main()
