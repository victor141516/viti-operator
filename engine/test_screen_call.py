import unittest
from unittest import mock

import screen_call


class ScreenCallTests(unittest.TestCase):
    @mock.patch.dict("os.environ", {"MANAGER_URL": "http://manager:8787", "VOWIFI_MANAGER_TOKEN": "secret", "VOWIFI_ID": "engine-1"}, clear=True)
    def test_returns_manager_action_without_aggregate_timeout(self):
        post = mock.Mock(return_value=(200, {"action": "take_control"}))
        self.assertEqual(screen_call.screening_action("call-1", post), "take_control")
        post.assert_called_once_with(
            "http://manager:8787/api/engine/screen",
            {"instance": "engine-1", "call_id": "call-1"},
            {"X-Viti-Engine-Token": "secret"},
        )

    @mock.patch.dict("os.environ", {"MANAGER_URL": "http://manager:8787"}, clear=True)
    def test_pending_heartbeats_preserve_bot_deadlines(self):
        post = mock.Mock(side_effect=[
            (200, {"action": "pending"}),
            (200, {"action": "pending"}),
            (200, {"action": "ring_clients"}),
        ])
        self.assertEqual(screen_call.screening_action("call-1", post), "ring_clients")
        self.assertEqual(post.call_count, 3)

    def test_http_poll_has_an_idle_timeout(self):
        response = mock.MagicMock()
        response.__enter__.return_value.status = 200
        response.__enter__.return_value.__iter__.return_value = iter(())
        with mock.patch.object(screen_call.urllib.request, "urlopen", return_value=response) as urlopen, \
                mock.patch.object(screen_call.json, "load", return_value={"action": "pending"}):
            status, payload = screen_call.post_json("http://manager/screen", {}, {})
        self.assertEqual((status, payload), (200, {"action": "pending"}))
        self.assertEqual(urlopen.call_args.kwargs["timeout"], screen_call.REQUEST_TIMEOUT_SECONDS)

    @mock.patch.dict("os.environ", {"MANAGER_URL": "http://manager:8787"}, clear=True)
    def test_every_transport_or_protocol_failure_fails_open(self):
        for outcome in [(503, {}), (200, {"action": "unknown"}), RuntimeError("offline")]:
            post = mock.Mock()
            if isinstance(outcome, Exception):
                post.side_effect = outcome
            else:
                post.return_value = outcome
            self.assertEqual(screen_call.screening_action("call-1", post), "ring_clients")


if __name__ == "__main__":
    unittest.main()
