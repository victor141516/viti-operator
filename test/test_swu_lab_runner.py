import importlib.util
import os
from pathlib import Path
from unittest import mock
import unittest


RUNNER_PATH = Path(__file__).parent / "swu-lab" / "run_lab.py"
SPEC = importlib.util.spec_from_file_location("viti_swu_lab_runner", RUNNER_PATH)
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class ComposeEnvironmentTests(unittest.TestCase):
    def tearDown(self):
        RUNNER.COMPOSE_FIXED_OVERRIDES.clear()
        RUNNER.COMPOSE_OVERRIDES.clear()

    def test_packaged_backend_image_survives_scenario_override_reset(self):
        RUNNER.COMPOSE_FIXED_OVERRIDES["LAB_STRONGSWAN_BACKEND_IMAGE"] = "packaged:under-test"
        RUNNER.COMPOSE_OVERRIDES["LAB_SWU_LIVENESS_PERIOD"] = "1"

        RUNNER.COMPOSE_OVERRIDES.clear()
        RUNNER.COMPOSE_OVERRIDES["LAB_SWU_NATT_KEEPALIVE"] = "2"

        environment = RUNNER.compose_environment()
        self.assertEqual(environment["LAB_STRONGSWAN_BACKEND_IMAGE"], "packaged:under-test")
        self.assertEqual(environment["LAB_SWU_NATT_KEEPALIVE"], "2")
        self.assertNotIn("LAB_SWU_LIVENESS_PERIOD", environment)

    def test_scenario_override_wins_over_host_and_fixed_values(self):
        variable = "VITI_TEST_COMPOSE_OVERRIDE_PRECEDENCE"
        previous = os.environ.get(variable)
        try:
            os.environ[variable] = "host"
            RUNNER.COMPOSE_FIXED_OVERRIDES[variable] = "fixed"
            RUNNER.COMPOSE_OVERRIDES[variable] = "scenario"
            self.assertEqual(RUNNER.compose_environment()[variable], "scenario")
        finally:
            if previous is None:
                os.environ.pop(variable, None)
            else:
                os.environ[variable] = previous

    @mock.patch.object(RUNNER, "compose")
    def test_lab_services_are_built_in_separate_solves(self, compose):
        RUNNER.build_services_serially(("epdg", "aka-broker", "python-backend"))

        self.assertEqual(
            compose.call_args_list,
            [
                mock.call("build", "epdg"),
                mock.call("build", "aka-broker"),
                mock.call("build", "python-backend"),
            ],
        )

    @mock.patch.object(RUNNER, "epdg_sa_state", return_value={"ike": (), "children": set()})
    @mock.patch.object(RUNNER, "start_protocol_lab")
    @mock.patch.object(RUNNER, "compose_popen")
    @mock.patch.object(RUNNER, "compose")
    def test_collision_delay_is_injected_from_probe_image(
        self, compose, compose_popen, start_protocol_lab, epdg_sa_state
    ):
        compose.side_effect = RuntimeError("stop after netem setup")

        with self.assertRaisesRegex(RuntimeError, "stop after netem setup"):
            RUNNER.run_colliding_rekeys("strongswan-backend", "strongswan-probe", "child")

        compose.assert_called_once_with(
            "run", "--rm", "--entrypoint", "tc", "strongswan-probe",
            "qdisc", "replace", "dev", "eth0", "root", "netem", "delay", "600ms",
        )


if __name__ == "__main__":
    unittest.main()
