import importlib.util
import itertools
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import event_outbox


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

    def test_tunnel_process_transitions_are_committed_synchronously(self):
        self.module.SWU_TUNNEL_GENERATION = "process-one"
        self.module.SWU_TUNNEL_SEQUENCE = itertools.count(1)
        with mock.patch.object(event_outbox, "enqueue_event") as enqueue:
            self.assertTrue(self.module.swu_notify("tunnel_start"))
            self.assertTrue(self.module.swu_notify("tunnel_up", "connected"))

        self.assertEqual(enqueue.call_count, 2)
        first = enqueue.call_args_list[0]
        second = enqueue.call_args_list[1]
        self.assertEqual(first.args[:2], ("tunnel_start", []))
        self.assertEqual(first.kwargs["extra"], {
            "tunnel_generation": "process-one", "tunnel_sequence": 1,
        })
        self.assertEqual(second.args[:2], ("tunnel_up", ["connected"]))
        self.assertEqual(second.kwargs["extra"]["tunnel_sequence"], 2)
        self.assertGreater(first.kwargs["recorded_at_ns"], 0)

    def test_process_does_not_start_without_a_durable_generation_claim(self):
        with mock.patch.object(self.module, "swu_notify", return_value=False):
            with self.assertRaisesRegex(RuntimeError, "durably claim"):
                self.module.main()

    def test_http_aka_timeout_is_configurable(self):
        with mock.patch.dict(os.environ, {
            "SWU_HTTP_AKA_URL": "http://aka.test/authenticate",
            "SWU_HTTP_AKA_TIMEOUT": "1.25",
        }), mock.patch.object(
            self.module.requests, "post", side_effect=TimeoutError("fixture timeout")
        ) as post:
            with self.assertRaisesRegex(RuntimeError, "AKA request failed"):
                self.module.http_res_ck_ik("00" * 16, "11" * 16)

        self.assertEqual(post.call_args.kwargs["timeout"], 1.25)

    def test_http_aka_rejects_nonpositive_timeout(self):
        with mock.patch.dict(os.environ, {
            "SWU_HTTP_AKA_URL": "http://aka.test/authenticate",
            "SWU_HTTP_AKA_TIMEOUT": "0",
        }), mock.patch.object(self.module.requests, "post") as post:
            with self.assertRaisesRegex(RuntimeError, "must be positive"):
                self.module.http_res_ck_ik("00" * 16, "11" * 16)

        post.assert_not_called()

    def test_network_backoff_timer_overrides_transient_reject_policy(self):
        client = object.__new__(self.module.swu)
        client.reject_reason_code = None
        client.reject_reason_policy = None
        client.reject_backoff_seconds = None
        payloads = [[
            self.module.N,
            [self.module.RESERVED, self.module.BACKOFF_TIMER, b"", bytes([0xA5])],
        ]]

        client.note_reject(self.module.AUTHENTICATION_FAILED, payloads)

        self.assertEqual(client.reject_reason_code, "AUTHENTICATION_FAILED")
        self.assertEqual(client.reject_reason_policy, "backoff")
        self.assertEqual(client.reject_backoff_seconds, 600)

    def ike_timer_subject(self):
        subject = self.module.swu.__new__(self.module.swu)
        subject.ike_rekey_period = 45 * 60
        subject._ike_sa_time = 1_000.0
        subject._ike_rekey_outstanding = False
        subject._ike_rekey_sent_at = None
        subject._ike_rekey_retry_at = None
        subject._ike_rekey_failed = False
        subject._rekey_outstanding = False
        subject.rekey_response_timeout = 10
        return subject

    def test_proactive_ike_rekey_starts_before_carrier_expiry(self):
        subject = self.ike_timer_subject()
        subject.state_ue_create_sa = mock.Mock()

        with mock.patch.object(self.module.time, "monotonic", return_value=3_701.0):
            subject._ike_rekey_tick()

        subject.state_ue_create_sa.assert_called_once_with()
        self.assertIsNone(subject._ike_rekey_retry_at)

    def test_ike_rekey_timeout_keeps_current_sa_and_schedules_retry(self):
        subject = self.ike_timer_subject()
        subject._ike_rekey_outstanding = True
        subject._ike_rekey_sent_at = 3_000.0
        subject.state_ue_create_sa = mock.Mock()

        with mock.patch.object(self.module.time, "monotonic", return_value=3_011.0):
            subject._ike_rekey_tick()

        subject.state_ue_create_sa.assert_not_called()
        self.assertFalse(subject._ike_rekey_outstanding)
        self.assertEqual(subject._ike_rekey_retry_at, 3_071.0)
        self.assertEqual(subject._ike_sa_time, 1_000.0)

    def test_ike_rekey_waits_for_child_rekey_exchange(self):
        subject = self.ike_timer_subject()
        subject._rekey_outstanding = True
        subject.state_ue_create_sa = mock.Mock()

        with mock.patch.object(self.module.time, "monotonic", return_value=3_701.0):
            subject._ike_rekey_tick()

        subject.state_ue_create_sa.assert_not_called()
        self.assertEqual(subject._ike_rekey_retry_at, 3_706.0)


if __name__ == "__main__":
    unittest.main()
