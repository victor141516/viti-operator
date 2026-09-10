import json
from pathlib import Path
import tempfile
import unittest

import tunnel_backend
import tunnel_contract


class TunnelContractTest(unittest.TestCase):
    def test_control_actions_have_stable_names_and_legacy_aliases(self):
        self.assertEqual(tunnel_contract.normalize_control_action("reauth\n"), "reauth")
        self.assertEqual(tunnel_contract.normalize_control_action("r"), "reauth")
        self.assertEqual(tunnel_contract.encode_control_action("rekey-child"), b"rekey-child\n")
        with self.assertRaisesRegex(tunnel_contract.ContractViolation, "unknown"):
            tunnel_contract.normalize_control_action("erase-everything")

    def test_connected_status_requires_backend_inner_address_and_interface(self):
        payload = tunnel_contract.status_payload(
            "python", "CONNECTED", 1,
            inner_ip="2001:db8::10", pcscf="2001:db8::20", iface="ipsec0",
        )
        observation = tunnel_contract.validate_status(payload)
        self.assertEqual(observation.backend, "python")
        self.assertEqual(observation.inner_ip, "2001:db8::10")

        for missing in ("inner_ip", "iface"):
            invalid = dict(payload)
            invalid.pop(missing)
            with self.subTest(missing=missing), self.assertRaises(tunnel_contract.ContractViolation):
                tunnel_contract.validate_status(invalid)

        without_pcscf = dict(payload)
        without_pcscf.pop("pcscf")
        self.assertEqual(tunnel_contract.validate_status(without_pcscf).pcscf, "")

    def test_runtime_status_is_consumed_only_through_public_file(self):
        with tempfile.TemporaryDirectory() as directory:
            payload = tunnel_contract.status_payload("python", "CONNECTING", 5)
            (Path(directory) / "swu_status.json").write_text(json.dumps(payload), encoding="utf-8")
            self.assertEqual(tunnel_contract.read_status(directory).state, "CONNECTING")

    def test_transition_sequence_is_generation_scoped_and_contiguous(self):
        events = [
            {"event": "tunnel_start", "tunnel_generation": "g1", "tunnel_sequence": 1,
             "recorded_at_ns": 10},
            {"event": "tunnel_up", "tunnel_generation": "g1", "tunnel_sequence": 2,
             "recorded_at_ns": 20},
            {"event": "tunnel_down", "tunnel_generation": "g1", "tunnel_sequence": 3,
             "recorded_at_ns": 30},
        ]
        self.assertEqual(tunnel_contract.validate_transition_sequence(events), "g1")
        events[2]["tunnel_sequence"] = 4
        with self.assertRaises(tunnel_contract.ContractViolation):
            tunnel_contract.validate_transition_sequence(events)

    def test_duplicate_down_transition_is_not_part_of_the_contract(self):
        events = [
            {"event": "tunnel_start", "tunnel_generation": "g1", "tunnel_sequence": 1,
             "recorded_at_ns": 10},
            {"event": "tunnel_up", "tunnel_generation": "g1", "tunnel_sequence": 2,
             "recorded_at_ns": 20},
            {"event": "tunnel_down", "tunnel_generation": "g1", "tunnel_sequence": 3,
             "recorded_at_ns": 30},
            {"event": "tunnel_down", "tunnel_generation": "g1", "tunnel_sequence": 4,
             "recorded_at_ns": 40},
        ]
        with self.assertRaisesRegex(
            tunnel_contract.ContractViolation, "invalid tunnel lifecycle"
        ):
            tunnel_contract.validate_transition_sequence(events)

    def test_python_backend_command_maps_the_stable_inputs(self):
        backend, command = tunnel_backend.backend_command({
            "SWU_BACKEND": "python",
            "SWU_SOURCE": "192.0.2.10",
            "SWU_EPDG": "198.51.100.20",
            "SWU_APN": "ims",
            "USIM_IMSI": "001010123456789",
            "SWU_MCC": "001",
            "SWU_MNC": "01",
            "SWU_IMEI": "123456789012345",
            "SWU_IMEISV": "1234567890123456",
            "USIM_READER_INDEX": "7",
        })
        self.assertEqual(backend, "python")
        self.assertEqual(command[command.index("-m") + 1], "7")
        self.assertEqual(command[command.index("-d") + 1], "198.51.100.20")
        self.assertEqual(command[command.index("-I") + 1], "001010123456789")

    def test_strongswan_selection_fails_closed_until_adapter_exists(self):
        with self.assertRaisesRegex(
            tunnel_backend.BackendConfigurationError, "adapter is not installed"
        ):
            tunnel_backend.backend_command({
                "SWU_BACKEND": "strongswan",
                "SWU_STRONGSWAN_BACKEND": "/definitely/missing/adapter.py",
            })


if __name__ == "__main__":
    unittest.main()
