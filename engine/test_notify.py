import os
from pathlib import Path
import re
import sys
import unittest
from unittest import mock

try:
    from jinja2 import Environment
except ImportError:  # The engine image includes python3-jinja2; lightweight host tests may not.
    Environment = None

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
            if event in ("call_in", "call_out", "call_active"):
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

    def test_service_media_uses_non_owning_conference_context_and_service_recording_role(self):
        candidates = [
            Path(__file__).with_name("templates") / "extensions.conf.j2",
            Path("/opt/vowifi/templates/extensions.conf.j2"),
        ]
        template = next(path for path in candidates if path.exists()).read_text(encoding="utf-8")
        service_context = template[
            template.index("[viti-service-conference]"):
            template.index("[viti-carrier-conference]")
        ]

        self.assertIn("ConfBridge(${VITI_CALL_ID},viti_bridge,viti_client)", service_context)
        self.assertIn("viti-start-client-recording", service_context)
        self.assertNotIn("participant_joined", service_context)
        self.assertNotIn("participant_left", service_context)
        recording_context = template[template.index("[viti-start-client-recording]"):]
        self.assertIn("VITI_PARTICIPANT_ROLE", recording_context)
        self.assertIn("?service:client", recording_context)

    def test_service_originated_call_preserves_manager_media_identity(self):
        candidates = [
            Path(__file__).with_name("templates") / "extensions.conf.j2",
            Path("/opt/vowifi/templates/extensions.conf.j2"),
        ]
        template = next(path for path in candidates if path.exists()).read_text(encoding="utf-8")
        public_call = template[template.index("exten => _[+0-9].,1"):template.index("; Report the outcome", template.index("exten => _[+0-9].,1"))]

        self.assertIn('${VITI_CALL_ID}" = ""]?Set(VITI_CALL_ID=${CHANNEL(linkedid)})', public_call)
        self.assertIn('${VITI_TRACE_ID}" = ""]?Set(VITI_TRACE_ID=${PJSIP_HEADER', public_call)
        self.assertIn('${VITI_CLIENT_ID}" = ""]?Set(__VITI_CLIENT_ID=', public_call)
        self.assertIn('${VITI_PARTICIPANT_ID}" = ""]?Set(__VITI_PARTICIPANT_ID=', public_call)

        outgoing_dispatch = template[
            template.index("[viti-outgoing-conference]"):
            template.index("[viti-client-conference]")
        ]
        self.assertIn('VITI_PARTICIPANT_ROLE', outgoing_dispatch)
        self.assertIn('viti-service-conference', outgoing_dispatch)
        self.assertIn('viti-client-conference', outgoing_dispatch)
        self.assertIn('viti-carrier-conference', outgoing_dispatch)

    def test_incoming_screening_fails_open_before_ringing_and_active_handoff_has_no_ringback(self):
        candidates = [
            Path(__file__).with_name("templates") / "extensions.conf.j2",
            Path("/opt/vowifi/templates/extensions.conf.j2"),
        ]
        template = next(path for path in candidates if path.exists()).read_text(encoding="utf-8")
        incoming = template[
            template.index("[volte_ims]"):
            template.index("[volte_ims_msg]")
        ]
        screening = incoming.index("screen_call.py")
        self.assertIn("${TRIM(${SHELL(python3 /usr/local/bin/screen_call.py", incoming)
        ring = incoming.index("Ringing()", screening)
        answer = incoming.index("n(screen_control),Answer()", screening)
        conference = incoming.index("Goto(viti-carrier-conference,s,1)", answer)

        self.assertLess(screening, ring)
        self.assertLess(answer, conference)
        self.assertIn('VITI_SCREEN_ACTION}" = "hangup"', incoming[screening:ring])
        self.assertIn('VITI_SCREEN_ACTION}" = "take_control"', incoming[screening:ring])

        no_endpoint_branch = incoming[
            incoming.index("{% else %}", ring):
            incoming.index("{% endif %}", incoming.index("{% else %}", ring))
        ]
        self.assertIn("same => n,Wait(60)", no_endpoint_branch)
        self.assertNotIn("Answer()", no_endpoint_branch)

        active_handoff = template[
            template.index("[viti-ring-clients]"):
            template.index("[viti-outgoing-conference]")
        ]
        self.assertIn("Dial(", active_handoff)
        self.assertIn("G(viti-screened-client^s^1)", active_handoff)
        self.assertNotIn("Ringing()", active_handoff)
        self.assertIn("[viti-screened-client]", active_handoff)

    @unittest.skipIf(Environment is None, "Jinja2 is not installed in the host test environment")
    def test_incoming_screening_without_human_endpoints_stays_unanswered(self):
        candidates = [
            Path(__file__).with_name("templates") / "extensions.conf.j2",
            Path("/opt/vowifi/templates/extensions.conf.j2"),
        ]
        template = next(path for path in candidates if path.exists()).read_text(encoding="utf-8")
        rendered = Environment(trim_blocks=True, lstrip_blocks=True).from_string(template).render(
            external_accounts=[],
            webrtc_enable=False,
        )
        incoming = rendered[
            rendered.index("[volte_ims]"):
            rendered.index("[volte_ims_msg]")
        ]
        unanswered_handoff = incoming[
            incoming.index("same => n,Ringing()"):
            incoming.index("same => n(screen_control),Answer()")
        ]

        self.assertIn("same => n,Wait(60)", unanswered_handoff)
        self.assertNotIn("Answer()", unanswered_handoff)

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

    def test_diagnostics_do_not_turn_a_real_204_admission_into_failure(self):
        with mock.patch.dict(os.environ, {"VOWIFI_DIAGNOSTICS_ENABLED": "1"}, clear=True), \
                mock.patch.object(sys, "argv", [
                    "notify.py", "participant_joined", "linked-42", "trace-99",
                    "web-a", "web-a", "leg-a", "PJSIP/web-0001",
                ]), \
                mock.patch.object(notify, "load_env", return_value={"VOWIFI_ID": "engine-one", "MANAGER_URL": "http://manager"}), \
                mock.patch.object(os, "makedirs"), \
                mock.patch.object(notify, "append_json", side_effect=[None, OSError("log disk full")]) as append, \
                mock.patch.object(event_outbox.HTTP, "post", return_value=event_outbox._Response(204)):
            result = notify.main()

        self.assertEqual(result, 0)
        self.assertGreaterEqual(append.call_count, 2)

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
