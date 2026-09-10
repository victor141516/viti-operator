import os
from pathlib import Path
import re
import sys
import unittest
from unittest import mock

import event_outbox
import notify


class NotifyTest(unittest.TestCase):
    def test_call_hooks_wait_for_durable_enqueue(self):
        candidates = [
            Path(__file__).with_name("templates") / "extensions.conf.j2",
            Path("/opt/vowifi/templates/extensions.conf.j2"),
        ]
        template = next(path for path in candidates if path.exists()).read_text(encoding="utf-8")
        background_call_hooks = [
            line for line in template.splitlines()
            if "notify.py call_" in line and re.search(r"\s&\)\s*$", line)
        ]
        self.assertEqual(background_call_hooks, [])

    def test_public_outbound_dialplan_rejects_non_e164_before_side_effects(self):
        candidates = [
            Path(__file__).with_name("templates") / "extensions.conf.j2",
            Path("/opt/vowifi/templates/extensions.conf.j2"),
        ]
        template = next(path for path in candidates if path.exists()).read_text(encoding="utf-8")
        guard = template.index("Set(VITI_PUBLIC_DIGITS=${FILTER(0-9,${EXTEN})})")
        persistence = template.index("notify.py call_out", guard)
        carrier_dial = template.index("Dial(PJSIP/${EXTEN}@volte_ims", guard)

        self.assertLess(guard, persistence)
        self.assertLess(guard, carrier_dial)
        self.assertIn('${EXTEN:0:1}" != "+"', template[guard:persistence])
        self.assertIn('${EXTEN:1}" != "${VITI_PUBLIC_DIGITS}', template[guard:persistence])
        self.assertIn("LEN(${VITI_PUBLIC_DIGITS})} < 8", template[guard:persistence])
        self.assertIn("LEN(${VITI_PUBLIC_DIGITS})} > 15", template[guard:persistence])
        self.assertIn("n(invalid_destination)", template[guard:])

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
                    mock.patch.object(event_outbox, "enqueue_event", return_value=Path("queued.json")) as enqueue, \
                    mock.patch.object(event_outbox, "deliver_file") as deliver:
                notify.main()

            enqueue.assert_called_once()
            args, kwargs = enqueue.call_args
            self.assertEqual(args, (event, event_args))
            self.assertEqual(kwargs["extra"], {"call_id": "linked-42", "trace_id": "trace-99"})
            self.assertGreater(kwargs["recorded_at_ns"], 0)
            if event in ("call_in", "call_out"):
                deliver.assert_called_once_with(Path("queued.json"))
            else:
                deliver.assert_not_called()

    def test_client_disconnect_does_not_end_carrier_and_reports_connection_generation(self):
        candidates = [
            Path(__file__).with_name("templates") / "extensions.conf.j2",
            Path("/opt/vowifi/templates/extensions.conf.j2"),
        ]
        template = next(path for path in candidates if path.exists()).read_text(encoding="utf-8")
        client_context = template[template.index("[viti-client-conference]"):template.index("[viti-carrier-conference]")]

        self.assertIn("notify.py participant_joined", client_context)
        self.assertIn("notify.py participant_left", client_context)
        self.assertIn("${VITI_CONNECTION_ID}", client_context)
        self.assertNotIn("ConfKick", client_context)
        self.assertIn("explicit, idempotent manager operation", client_context)
        self.assertIn("Set(VITI_JOIN_ADMITTED=0)", client_context)
        join_hook = client_context.index("System(python3 /usr/local/bin/notify.py participant_joined")
        join_guard = client_context.index("${SYSTEMSTATUS}", join_hook)
        join_commit = client_context.index("Set(VITI_JOIN_ADMITTED=1)", join_guard)
        conference = client_context.index("ConfBridge", join_guard)
        self.assertLess(join_hook, join_guard)
        self.assertLess(join_guard, join_commit)
        self.assertLess(join_commit, conference)
        self.assertLess(join_guard, conference)
        self.assertIn("join_admission_failed", client_context[join_guard:])
        self.assertIn("participant_join_aborted", client_context[join_guard:])
        self.assertIn('${VITI_JOIN_ADMITTED}" != "1', client_context[conference:])

    def test_participant_event_keeps_call_and_trace_identity(self):
        response = mock.Mock(status_code=204, text="")
        with mock.patch.dict(os.environ, {}, clear=True), \
                mock.patch.object(sys, "argv", [
                    "notify.py", "participant_joined", "linked-42", "trace-99",
                    "web-a", "web-a", "leg-a", "PJSIP/web-0001",
                ]), \
                mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one", "MANAGER_URL": "http://manager"}), \
                mock.patch.object(event_outbox, "enqueue_event") as enqueue, \
                mock.patch.object(event_outbox.HTTP, "post", return_value=response) as post:
            result = notify.main()

        enqueue.assert_not_called()
        payload = post.call_args.kwargs["json"]
        self.assertEqual(payload["event"], "participant_joined")
        self.assertEqual(payload["call_id"], "linked-42")
        self.assertEqual(payload["trace_id"], "trace-99")
        self.assertEqual(payload["args"], ["web-a", "web-a", "leg-a", "PJSIP/web-0001"])
        self.assertEqual(result, 0)

    def test_participant_join_fails_closed_when_manager_admission_is_deferred(self):
        response = mock.Mock(status_code=502, text="temporary AMI failure")
        with mock.patch.dict(os.environ, {}, clear=True), \
                mock.patch.object(sys, "argv", [
                    "notify.py", "participant_joined", "linked-42", "trace-99",
                    "web-a", "web-a", "leg-a", "PJSIP/web-0001",
                ]), \
                mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one", "MANAGER_URL": "http://manager"}), \
                mock.patch.object(event_outbox, "enqueue_event") as enqueue, \
                mock.patch.object(event_outbox.HTTP, "post", return_value=response):
            result = notify.main()

        enqueue.assert_not_called()
        self.assertEqual(result, 1)

    def test_participant_join_fails_closed_without_manager_or_after_transport_error(self):
        argv = [
            "notify.py", "participant_joined", "linked-42", "trace-99",
            "web-a", "web-a", "leg-a", "PJSIP/web-0001",
        ]
        with mock.patch.dict(os.environ, {}, clear=True), \
                mock.patch.object(sys, "argv", argv), \
                mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one"}):
            self.assertEqual(notify.main(), 1)
        with mock.patch.dict(os.environ, {}, clear=True), \
                mock.patch.object(sys, "argv", argv), \
                mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one", "MANAGER_URL": "http://manager"}), \
                mock.patch.object(event_outbox.HTTP, "post", side_effect=OSError("offline")):
            self.assertEqual(notify.main(), 1)

    def test_aborted_admission_is_durable_and_keeps_generation_identity(self):
        with mock.patch.dict(os.environ, {}, clear=True), \
                mock.patch.object(sys, "argv", [
                    "notify.py", "participant_join_aborted", "linked-42", "trace-99",
                    "web-a", "web-a", "leg-a", "PJSIP/web-0001",
                ]), \
                mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one", "MANAGER_URL": "http://manager"}), \
                mock.patch.object(event_outbox, "enqueue_event", return_value=Path("queued.json")) as enqueue:
            result = notify.main()

        args, kwargs = enqueue.call_args
        self.assertEqual(args, ("participant_join_aborted", ["web-a", "web-a", "leg-a", "PJSIP/web-0001"]))
        self.assertEqual(kwargs["extra"], {"call_id": "linked-42", "trace_id": "trace-99"})
        self.assertEqual(result, 0)


if __name__ == "__main__":
    unittest.main()
