import subprocess
import signal
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import strongswan_adapter
import tunnel_contract


def environment(**extra):
    values = {
        "SWU_SOURCE": "192.0.2.10",
        "SWU_EPDG": "198.51.100.20",
        "SWU_APN": "ims",
        "USIM_IMSI": "001010123456789",
        "SWU_MCC": "001",
        "SWU_MNC": "01",
        "SWU_CHILD_REKEY_MINUTES": "30",
        "SWU_IKE_REKEY_MINUTES": "45",
        "SWU_LIVENESS_PERIOD": "20",
    }
    values.update(extra)
    return values


class StrongSwanAdapterTest(unittest.TestCase):
    def test_tunnel_default_route_is_restricted_to_the_assigned_source(self):
        runner = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 0),
            subprocess.CompletedProcess([], 0),
            subprocess.CompletedProcess([], 2),
            subprocess.CompletedProcess([], 0),
            subprocess.CompletedProcess(
                [], 0, stdout="220: from 2001:db8::10 lookup 220\n"
            ),
        ])
        strongswan_adapter.isolate_tunnel_route("2001:db8::10", runner)
        self.assertEqual(runner.call_args_list, [
            mock.call(
                [
                    "ip", "-6", "rule", "del", "priority", "220",
                    "table", "220",
                ],
                check=False,
                capture_output=True,
                text=True,
            ),
            mock.call(
                [
                    "ip", "-6", "rule", "del", "priority", "220",
                    "table", "220",
                ],
                check=False,
                capture_output=True,
                text=True,
            ),
            mock.call(
                [
                    "ip", "-6", "rule", "del", "priority", "220",
                    "table", "220",
                ],
                check=False,
                capture_output=True,
                text=True,
            ),
            mock.call([
                "ip", "-6", "rule", "add", "priority", "220",
                "from", "2001:db8::10/128", "lookup", "220",
            ], check=True),
            mock.call(
                ["ip", "-6", "rule", "show"],
                check=True,
                capture_output=True,
                text=True,
            ),
        ])

    def test_tunnel_route_rejects_a_remaining_broad_rule(self):
        runner = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 2),
            subprocess.CompletedProcess([], 0),
            subprocess.CompletedProcess(
                [],
                0,
                stdout=(
                    "220: from all lookup 220\n"
                    "220: from 2001:db8::10 lookup 220\n"
                ),
            ),
        ])
        with self.assertRaisesRegex(RuntimeError, "routing isolation failed"):
            strongswan_adapter.isolate_tunnel_route("2001:db8::10", runner)

    def test_ipv4_inner_address_is_rejected_by_the_ipv6_only_profile(self):
        with self.assertRaisesRegex(ValueError, "requires an IPv6"):
            strongswan_adapter.isolate_tunnel_route("192.0.2.1")

    def test_auth_readiness_url_is_derived_from_broker_contract(self):
        self.assertEqual(
            strongswan_adapter.auth_readiness_url({
                "SWU_HTTP_AKA_URL": "http://manager:8787/api/v1/authenticate",
            }),
            "http://manager:8787/api/v1/authenticate/ready",
        )

    def test_auth_readiness_requires_a_boolean(self):
        self.assertEqual(
            strongswan_adapter.parse_auth_readiness(
                b'{"ready":false,"detail":"SIM absent"}'
            ),
            (False, "SIM absent"),
        )
        with self.assertRaisesRegex(ValueError, "boolean ready"):
            strongswan_adapter.parse_auth_readiness(b'{"ready":"yes"}')

    def test_adapter_waits_without_starting_ike_until_provider_recovers(self):
        adapter = strongswan_adapter.StrongSwanAdapter(environment())
        observations = iter([(False, "SIM absent"), (True, "")])
        adapter.fetch_auth_readiness = lambda: next(observations)
        adapter.env["SWU_HTTP_AKA_READY_INTERVAL"] = "0.01"
        self.assertTrue(adapter.wait_auth_ready())
        self.assertTrue(adapter.auth_waiting)

    def test_vici_failure_preserves_partial_control_records(self):
        class Session:
            class Transport:
                @staticmethod
                def close():
                    return None

            transport = Transport()

            @staticmethod
            def initiate(_payload):
                def records():
                    yield {"msg": b"parsed IKE_AUTH response 1 [ N(AUTH_FAILED) N((41041)) ]"}
                    raise RuntimeError("establishing CHILD_SA failed")
                return records()

        adapter = strongswan_adapter.StrongSwanAdapter(environment())
        adapter.session = lambda _timeout=10.0: Session()
        with self.assertRaises(strongswan_adapter.ViciCommandError) as raised:
            adapter.request("initiate", {"child": "ims"})
        self.assertEqual(len(raised.exception.records), 1)

    def test_private_carrier_backoff_is_normalized(self):
        error = strongswan_adapter.ViciCommandError(
            "initiate",
            RuntimeError("establishing CHILD_SA failed"),
            [{"msg": b"parsed IKE_AUTH response 1 [ N(AUTH_FAILED) N((41041)) ]"}],
        )
        self.assertEqual(strongswan_adapter.classify_failure(error, environment()), {
            "reason_code": "AUTHENTICATION_FAILED",
            "reason_policy": "backoff",
            "backoff": 600,
        })

    def test_local_authentication_provider_failure_is_rate_limited(self):
        error = strongswan_adapter.ViciCommandError(
            "initiate",
            RuntimeError("establishing CHILD_SA failed"),
            [{"msg": b"Viti AKA broker request failed"}],
        )
        self.assertEqual(strongswan_adapter.classify_failure(error, environment()), {
            "reason_code": "AUTHENTICATION_PROVIDER_UNAVAILABLE",
            "reason_policy": "backoff",
            "backoff": 60,
        })

    def test_vici_auth_timeout_outlives_the_broker(self):
        self.assertEqual(
            strongswan_adapter.initiate_timeout_ms(
                environment(SWU_HTTP_AKA_TIMEOUT="35")
            ),
            "45000",
        )
        self.assertEqual(
            strongswan_adapter.initiate_timeout_ms(
                environment(
                    SWU_HTTP_AKA_TIMEOUT="35",
                    SWU_IKE_AUTH_TIMEOUT="52.5",
                )
            ),
            "52500",
        )
        self.assertEqual(
            strongswan_adapter.initiate_socket_timeout(
                environment(SWU_HTTP_AKA_TIMEOUT="35")
            ),
            50.0,
        )

    def test_initiate_uses_long_socket_timeout_for_slow_aka(self):
        adapter = strongswan_adapter.StrongSwanAdapter(
            environment(SWU_HTTP_AKA_TIMEOUT="35")
        )
        observed = []

        class Session:
            class Transport:
                @staticmethod
                def close():
                    return None

            transport = Transport()

            @staticmethod
            def load_conn(_payload):
                return {}

            @staticmethod
            def initiate(_payload):
                return iter(({"success": b"yes"},))

        def session(timeout):
            observed.append(timeout)
            return Session()

        adapter.session = session
        adapter.load_and_initiate()
        self.assertEqual(observed, [10.0, 50.0])

    def test_reauthentication_uses_long_socket_timeout_for_slow_aka(self):
        adapter = strongswan_adapter.StrongSwanAdapter(
            environment(SWU_HTTP_AKA_TIMEOUT="35")
        )
        with mock.patch.object(adapter, "request") as request:
            adapter.handle_control("reauth")
        request.assert_called_once_with(
            "rekey",
            {"ike": strongswan_adapter.CONNECTION, "reauth": "yes"},
            socket_timeout=50.0,
        )

    def test_runtime_persists_backoff_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = strongswan_adapter.RuntimeContract({
                "VOWIFI_RUNDIR": directory,
                "VITI_EVENT_OUTBOX_DIR": directory + "/event-outbox",
            })
            runtime.start()
            runtime.disconnected(
                "AUTHENTICATION_FAILED", "backoff", backoff=600
            )
            import json
            status = json.loads(
                (Path(directory) / "swu_status.json").read_text(encoding="utf-8")
            )
            self.assertEqual(status["reason_policy"], "backoff")
            self.assertEqual(status["backoff"], 600)

    def test_vici_connection_is_derived_from_stable_inputs(self):
        payload = strongswan_adapter.build_connection(environment())
        connection = payload[strongswan_adapter.CONNECTION]
        identity = "0001010123456789@nai.epc.mnc001.mcc001.3gppnetwork.org"
        self.assertEqual(connection["local"]["id"], identity)
        self.assertEqual(connection["local"]["eap_id"], identity)
        self.assertEqual(connection["remote"]["id"], "%any")
        self.assertEqual(connection["local_addrs"], ["192.0.2.10"])
        self.assertEqual(connection["remote_addrs"], ["198.51.100.20"])
        self.assertEqual(connection["vips"], ["::"])
        self.assertEqual(connection["children"]["ims"]["local_ts"], ["dynamic"])
        self.assertEqual(connection["children"]["ims"]["remote_ts"], ["::/0"])
        self.assertEqual(connection["rekey_time"], "2700s")
        self.assertEqual(connection["children"]["ims"]["esp_proposals"][:4], [
            "aes128-sha1",
            "aes256-sha256",
            "aes128-sha256",
            "aes256-sha1",
        ])

    def test_fqdn_responder_identity_matches_3gpp_format(self):
        self.assertEqual(
            strongswan_adapter.responder_identity(
                environment(SWU_IDR_MODE="fqdn")
            ),
            "ims.apn.epc.mnc001.mcc001.pub.3gppnetwork.org",
        )

    def test_responder_request_identity_rejects_config_injection(self):
        with self.assertRaisesRegex(ValueError, "invalid characters"):
            strongswan_adapter.responder_identity(
                environment(SWU_APN="ims\n  load = malicious")
            )

    def test_established_sa_requires_both_ike_and_child_state(self):
        message = {
            "viti-swu": {
                "state": b"ESTABLISHED",
                "uniqueid": b"17",
                "local-vips": [b"2001:db8::10"],
                "initiator-spi": b"a",
                "responder-spi": b"b",
                "child-sas": {"ims-18": {"state": b"INSTALLED"}},
            }
        }
        self.assertEqual(
            strongswan_adapter.established_sa([message])["inner_ip"],
            "2001:db8::10",
        )
        message["viti-swu"]["child-sas"]["ims-18"]["state"] = b"REKEYING"
        self.assertIsNone(strongswan_adapter.established_sa([message]))

    def test_sa_loss_requires_a_continuous_grace_period(self):
        self.assertFalse(strongswan_adapter.sa_loss_expired(None, 20.0, 5.0))
        self.assertFalse(strongswan_adapter.sa_loss_expired(16.0, 20.0, 5.0))
        self.assertTrue(strongswan_adapter.sa_loss_expired(15.0, 20.0, 5.0))

    def test_charon_config_uses_requested_natt_keepalive(self):
        with tempfile.TemporaryDirectory() as directory:
            base = f"{directory}/base.conf"
            with open(base, "w", encoding="utf-8") as output:
                output.write("charon {\n  load_modular = no\n}\n")
            adapter = strongswan_adapter.StrongSwanAdapter(environment(
                VOWIFI_RUNDIR=directory,
                SWU_STRONGSWAN_BASE_CONFIG=base,
                SWU_NATT_KEEPALIVE="1",
            ))
            adapter.charon_command = ["true"]

            class Process:
                returncode = 0

                @staticmethod
                def poll():
                    return 0

            original_popen = strongswan_adapter.subprocess.Popen
            adapter.wait_for_vici = lambda: None
            try:
                strongswan_adapter.subprocess.Popen = lambda *args, **kwargs: Process()
                adapter.start_charon()
            finally:
                strongswan_adapter.subprocess.Popen = original_popen
            generated = Path(directory, "strongswan.conf").read_text(encoding="utf-8")
            self.assertIn("request_idr = ims", generated)
            generated_path = Path(directory) / "strongswan.conf"
            generated = generated_path.read_text(encoding="utf-8")
            self.assertIn("keep_alive = 1s", generated)
            self.assertEqual(
                adapter.env["STRONGSWAN_CONF"], str(generated_path)
            )

    def test_runtime_emits_one_lifecycle_per_generation(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = strongswan_adapter.RuntimeContract({
                "VOWIFI_RUNDIR": directory,
                "VITI_EVENT_OUTBOX_DIR": directory + "/event-outbox",
            })
            runtime.start()
            runtime.connected("2001:db8::10", "2001:db8::20", "ipsec0")
            runtime.connected("2001:db8::10", "2001:db8::20", "ipsec0")
            runtime.disconnected("test")
            runtime.disconnected("duplicate")

            events = []
            for path in runtime.outbox.glob("*.json"):
                import json
                events.append(json.loads(path.read_text(encoding="utf-8")))
            events.sort(key=lambda event: event["tunnel_sequence"])
            tunnel_contract.validate_transition_sequence(events)
            self.assertEqual(
                [event["event"] for event in events],
                ["tunnel_start", "tunnel_up", "tunnel_down"],
            )

    def test_runtime_deduplicates_connected_status_between_heartbeats(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = strongswan_adapter.RuntimeContract({
                "VOWIFI_RUNDIR": directory,
                "VITI_EVENT_OUTBOX_DIR": directory + "/event-outbox",
                "SWU_STATUS_HEARTBEAT_SECONDS": "30",
            })
            with mock.patch.object(runtime, "write_status") as write:
                runtime.connected("2001:db8::10", "2001:db8::20", "ipsec0")
                runtime.connected("2001:db8::10", "2001:db8::20", "ipsec0")
            write.assert_called_once()

    def test_shutdown_persists_tunnel_down_before_peer_teardown(self):
        with tempfile.TemporaryDirectory() as directory:
            events = []

            class Runtime:
                rundir = Path(directory)

                @staticmethod
                def start():
                    events.append("start")

                @staticmethod
                def disconnected(**_failure):
                    events.append("down")

            adapter = strongswan_adapter.StrongSwanAdapter(environment())
            adapter.runtime = Runtime()
            adapter.wait_auth_ready = lambda: False
            adapter.terminate = lambda: events.append("terminate")

            self.assertEqual(adapter.run(), 0)
            self.assertEqual(events, ["start", "down", "terminate"])

    def test_persistence_failure_still_terminates_charon_before_propagating(self):
        with tempfile.TemporaryDirectory() as directory:
            events = []

            class Runtime:
                rundir = Path(directory)

                @staticmethod
                def start():
                    events.append("start")

                @staticmethod
                def disconnected(**_failure):
                    events.append("down-attempt")
                    raise OSError("outbox fsync failed")

            adapter = strongswan_adapter.StrongSwanAdapter(environment())
            adapter.runtime = Runtime()
            adapter.wait_auth_ready = lambda: False
            adapter.terminate = lambda: events.append("terminate")

            with self.assertRaisesRegex(OSError, "outbox fsync failed"):
                adapter.run()
            self.assertEqual(events, ["start", "down-attempt", "terminate"])

    def test_interrupted_vici_operation_is_a_clean_signal_shutdown(self):
        with tempfile.TemporaryDirectory() as directory:
            failures = []

            class Runtime:
                rundir = Path(directory)

                @staticmethod
                def start():
                    return None

                @staticmethod
                def disconnected(**failure):
                    failures.append(failure)

            adapter = strongswan_adapter.StrongSwanAdapter(environment())
            adapter.runtime = Runtime()
            adapter.wait_auth_ready = lambda: True
            adapter.start_charon = lambda: None

            def interrupted():
                adapter.stop_requested = True
                adapter.signal_received = signal.SIGTERM
                raise OSError("VICI socket closed")

            adapter.load_and_initiate = interrupted
            adapter.terminate = lambda: None

            self.assertEqual(adapter.run(), 0)
            self.assertEqual(failures, [{
                "reason_code": "signal",
                "reason_policy": "transient",
            }])


if __name__ == "__main__":
    unittest.main()
