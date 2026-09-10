import os
import sys
import unittest
from unittest import mock

import event_outbox
import notify


class NotifyTest(unittest.TestCase):
    def test_ordered_tunnel_transition_uses_durable_outbox(self):
        environment = {
            "VITI_TUNNEL_GENERATION": "process-one",
            "VITI_TUNNEL_SEQUENCE": "7",
        }
        with mock.patch.dict(os.environ, environment, clear=True), \
                mock.patch.object(sys, "argv", ["notify.py", "tunnel_down", "reauth failed"]), \
                mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one"}), \
                mock.patch.object(event_outbox, "enqueue_event") as enqueue:
            notify.main()

        enqueue.assert_called_once()
        args, kwargs = enqueue.call_args
        self.assertEqual(args[:2], ("tunnel_down", ["reauth failed"]))
        self.assertEqual(kwargs["extra"], {
            "tunnel_generation": "process-one",
            "tunnel_sequence": 7,
        })
        self.assertGreater(kwargs["recorded_at_ns"], 0)

    def test_call_lifecycle_uses_durable_outbox_with_stable_identity(self):
        events = {
            "call_in": ["+34123456789"],
            "call_out": ["+34123456789"],
            "call_active": ["out", "+34123456789"],
            "call_result": ["out", "+34123456789", "ANSWER", "16"],
        }
        for event, event_args in events.items():
            with self.subTest(event=event), \
                    mock.patch.dict(os.environ, {}, clear=True), \
                    mock.patch.object(sys, "argv", [
                        "notify.py", event, "linked-42", "trace-99", *event_args,
                    ]), \
                    mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one"}), \
                    mock.patch.object(event_outbox, "enqueue_event") as enqueue:
                notify.main()

            enqueue.assert_called_once()
            args, kwargs = enqueue.call_args
            self.assertEqual(args, (event, event_args))
            self.assertEqual(kwargs["extra"], {"call_id": "linked-42", "trace_id": "trace-99"})
            self.assertGreater(kwargs["recorded_at_ns"], 0)


if __name__ == "__main__":
    unittest.main()
