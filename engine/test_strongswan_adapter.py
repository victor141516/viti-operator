import tempfile
import unittest
from pathlib import Path

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
        adapter.session = lambda: Session()
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
        self.assertEqual(connection["remote"]["id"], "ims")
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
        connection = strongswan_adapter.build_connection(
            environment(SWU_IDR_MODE="fqdn")
        )[strongswan_adapter.CONNECTION]
        self.assertEqual(
            connection["remote"]["id"],
            "ims.apn.epc.mnc001.mcc001.pub.3gppnetwork.org",
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


if __name__ == "__main__":
    unittest.main()
