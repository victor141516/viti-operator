import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import event_outbox


class Response:
    def __init__(self, status_code):
        self.status_code = status_code


class EventOutboxTest(unittest.TestCase):
    def test_event_is_persisted_before_delivery_and_retried_after_http_500(self):
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(event_outbox, "load_env", return_value={"VOWIFI_ID": "engine-1"}):
                path = event_outbox.enqueue_event(
                    "sms_protocol", ["status_report", "sms-1", "+34600000000", "1", "1", "7", "0", "0", "-1", "1700000000"],
                    recorded_at_ns=123, outbox=directory,
                )
            payload = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(payload["event"], "sms_protocol")
            self.assertEqual(payload["instance"], "engine-1")
            self.assertEqual(payload["args"][-1], "1700000000")

            session = mock.Mock()
            session.post.side_effect = [Response(500), Response(204)]
            self.assertEqual(event_outbox.flush_once(directory, "http://manager", "secret", session), 0)
            self.assertTrue(path.exists())
            self.assertEqual(event_outbox.flush_once(directory, "http://manager", "secret", session), 1)
            self.assertFalse(path.exists())
            self.assertEqual(session.post.call_args.kwargs["headers"], {"X-Viti-Engine-Token": "secret"})

    def test_connection_failure_keeps_event(self):
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(event_outbox, "load_env", return_value={}):
                path = event_outbox.enqueue_event("sms_protocol", ["rp_ack"], outbox=directory)
            session = mock.Mock()
            session.post.side_effect = OSError("offline")
            self.assertFalse(event_outbox.deliver_file(path, "http://manager", "", session))
            self.assertTrue(path.exists())

    def test_tunnel_ordering_metadata_is_persisted_for_retry(self):
        with tempfile.TemporaryDirectory() as directory:
            path = event_outbox.enqueue_event(
                "tunnel_up", [], recorded_at_ns=456, outbox=directory,
                extra={"tunnel_generation": "process-one", "tunnel_sequence": 7},
            )
            payload = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(payload["tunnel_generation"], "process-one")
            self.assertEqual(payload["tunnel_sequence"], 7)

    def test_uncorrelated_early_event_does_not_block_later_mapping(self):
        with tempfile.TemporaryDirectory() as directory:
            early = event_outbox.enqueue_event("sms_protocol", ["status_report"], recorded_at_ns=1, outbox=directory)
            mapping = event_outbox.enqueue_event("sms_protocol", ["submitted"], recorded_at_ns=2, outbox=directory)
            session = mock.Mock()
            session.post.side_effect = [Response(409), Response(200)]
            self.assertEqual(event_outbox.flush_once(directory, "http://manager", "", session), 1)
            self.assertTrue(early.exists())
            self.assertFalse(mapping.exists())


if __name__ == "__main__":
    unittest.main()
