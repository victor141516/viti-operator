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
