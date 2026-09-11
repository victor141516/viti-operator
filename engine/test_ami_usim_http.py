import base64
import importlib.util
import os
import tempfile
import unittest
from types import SimpleNamespace
from pathlib import Path
from unittest import mock


class Response:
    def __init__(self, payload, status_code=200):
        self.payload = payload
        self.status_code = status_code

    def json(self):
        return self.payload


def encoded(value):
    return base64.b64encode(value).decode("ascii").rstrip("=")


class HTTPAKAProviderTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        os.environ["VOWIFI_RUNDIR"] = cls.temp.name
        path = Path(__file__).with_name("ami_usim.py")
        spec = importlib.util.spec_from_file_location("ami_usim_tested", path)
        cls.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.module)
        cls.module.HTTP_AKA_URL = "http://manager.test/api/v1/authenticate"

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    def test_success_response(self):
        payload = {
            "res": encoded(bytes(range(4))),
            "ck": encoded(bytes(range(16))),
            "ik": encoded(bytes(range(16, 32))),
        }
        with mock.patch.object(self.module.requests, "post", return_value=Response(payload)) as post:
            res, ck, ik, auts = self.module.http_res_ck_ik("00" * 16, "11" * 16)
        self.assertEqual(len(res), 8)
        self.assertEqual(len(ck), 32)
        self.assertEqual(len(ik), 32)
        self.assertIsNone(auts)
        self.assertEqual(post.call_args.kwargs["json"].keys(), {"rand", "autn"})

    def test_sync_failure_response(self):
        payload = {"auts": encoded(bytes(range(14)))}
        with mock.patch.object(self.module.requests, "post", return_value=Response(payload)):
            res, ck, ik, auts = self.module.http_res_ck_ik("00" * 16, "11" * 16)
        self.assertIsNone(res)
        self.assertIsNone(ck)
        self.assertIsNone(ik)
        self.assertEqual(len(auts), 28)

    def test_sms_protocol_event_is_forwarded_without_losing_zero_values(self):
        event = SimpleNamespace(
            Phase="status_report", MessageID="sms-1", Destination="+34600000000",
            Part=1, Parts=2, RPReference=0, TPReference=0, Status=0, Cause=None,
            ServiceCentreTime=1700000000,
        )
        with mock.patch.object(self.module.event_outbox, "enqueue_event") as enqueue:
            self.module.forward_sms_protocol_event(event)
        self.assertEqual(enqueue.call_args.args, (
            "sms_protocol", ["status_report", "sms-1", "+34600000000", "1", "2", "0", "0", "0", "", "1700000000"],
        ))

    def test_unknown_sms_protocol_event_is_not_forwarded(self):
        with mock.patch.object(self.module.event_outbox, "enqueue_event") as enqueue:
            self.module.forward_sms_protocol_event(SimpleNamespace(Phase="bogus"))
        enqueue.assert_not_called()

    def test_received_dtmf_is_forwarded_with_source_identity(self):
        event = SimpleNamespace(
            Direction="Received", Linkedid="call-42", Channel="UnicastRTP/manager-0001",
            Uniqueid="media-42", Digit="#", DurationMs=240,
        )
        with mock.patch.object(self.module.event_outbox, "enqueue_event") as enqueue:
            self.module.forward_dtmf_event(event)
        enqueue.assert_called_once_with(
            "dtmf", ["UnicastRTP/manager-0001", "media-42", "#", "240"],
            extra={"call_id": "call-42"},
        )

    def test_sent_dtmf_is_not_rebroadcast_as_a_new_source_event(self):
        event = SimpleNamespace(
            Direction="Sent", Linkedid="call-42", Channel="PJSIP/client-0001",
            Uniqueid="client-42", Digit="5", DurationMs=240,
        )
        with mock.patch.object(self.module.event_outbox, "enqueue_event") as enqueue:
            self.module.forward_dtmf_event(event)
        enqueue.assert_not_called()


if __name__ == "__main__":
    unittest.main()
