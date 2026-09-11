#!/usr/bin/env python3
"""Run the deterministic StrongSwan SWu laboratory lifecycle."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


ROOT = Path(__file__).resolve().parents[2]
COMPOSE_FILE = ROOT / "test" / "swu-lab" / "compose.yml"
COMMON_SERVICES = ("epdg", "aka-broker", "nat-gateway")
# Fixed overrides select the artifact under test and must survive per-scenario
# network/timer overrides.  Keeping them separate prevents a scenario reset
# from silently falling back to a different image.
COMPOSE_FIXED_OVERRIDES = {}
COMPOSE_OVERRIDES = {}
BACKEND_SERVICES = (
    "strongswan-backend",
    "strongswan-probe",
    "strongswan-lifecycle-probe",
    "strongswan-failure-probe",
    "strongswan-terminated-probe",
)


def compose(*args, capture=False):
    command = compose_command(*args)
    print("+", " ".join(command), flush=True)
    return subprocess.run(
        command,
        cwd=ROOT,
        check=True,
        text=True,
        capture_output=capture,
        env=compose_environment(),
    )


def compose_command(*args):
    return ["docker", "compose", "-f", str(COMPOSE_FILE), *args]


def compose_popen(*args):
    command = compose_command(*args)
    print("+", " ".join(command), flush=True)
    return subprocess.Popen(
        command,
        cwd=ROOT,
        env=compose_environment(),
    )


def compose_environment():
    return {**os.environ, **COMPOSE_FIXED_OVERRIDES, **COMPOSE_OVERRIDES}


def build_services_serially(services):
    """Build lab services one solve at a time.

    Docker Engine 29.6.1 embeds BuildKit 0.31.1, whose concurrent job-value
    iteration can panic the whole daemon when a multi-service Compose build
    resolves shared vertices.  Separate solves avoid that upstream race and
    still reuse BuildKit's layer cache for services sharing a Dockerfile.
    """
    for service in services:
        compose("build", service)


def broker_request(path, payload=None):
    encoded = json.dumps(payload).encode().hex() if payload is not None else ""
    script = (
        "import urllib.request;"
        f"d=bytes.fromhex({encoded!r}) if {bool(encoded)!r} else None;"
        f"q=urllib.request.Request('http://127.0.0.1:8080{path}',data=d,"
        "headers={'Authorization':'Bearer lab-token','Content-Type':'application/json'});"
        "print(urllib.request.urlopen(q,timeout=2).read().decode())"
    )
    deadline = time.monotonic() + 5
    while True:
        try:
            result = compose(
                "exec", "-T", "aka-broker", "python3", "-c", script, capture=True
            )
            return json.loads(result.stdout)
        except subprocess.CalledProcessError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.2)


def wait_for_backend_exit(service, timeout=15, require_failure=True):
    container = compose("ps", "-a", "-q", service, capture=True).stdout.strip()
    if not container:
        raise AssertionError(f"{service} container was not created")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = subprocess.run(
            ["docker", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", container],
            check=True,
            text=True,
            capture_output=True,
        )
        status, exit_code = result.stdout.strip().split()
        if status == "exited":
            if require_failure and int(exit_code) == 0:
                raise AssertionError(f"{service} unexpectedly exited successfully")
            return int(exit_code)
        time.sleep(0.2)
    raise AssertionError(f"{service} did not exit within {timeout}s")


def backend_container_state(service):
    container = compose("ps", "-a", "-q", service, capture=True).stdout.strip()
    if not container:
        return "missing", None
    result = subprocess.run(
        [
            "docker", "inspect", "--format",
            "{{.State.Status}} {{.State.ExitCode}}", container,
        ],
        check=True,
        text=True,
        capture_output=True,
    )
    status, exit_code = result.stdout.strip().split()
    return status, int(exit_code)


def run_probe(service, mode=None):
    args = ["run", "--rm"]
    if mode:
        args.extend(("-e", f"LAB_PROBE_MODE={mode}"))
    compose(*args, service)


def request_control(service, action):
    if action not in {"stop", "reauth", "rekey-child", "rekey-ike"}:
        raise ValueError(f"unsupported backend control action: {action}")
    compose(
        "exec", "-T", service, "sh", "-c",
        f"printf '{action}\\n' > /run/vowifi/swu.ctl",
    )


def wait_for_broker_requests(minimum, timeout=30):
    deadline = time.monotonic() + timeout
    state = {}
    while time.monotonic() < deadline:
        state = broker_request("/state")
        if state.get("authenticate_requests", 0) >= minimum:
            return state
        time.sleep(0.2)
    raise AssertionError(
        f"AKA broker did not receive {minimum} request(s) within {timeout}s: {state}"
    )


def reset_lab():
    # Explicitly enable opt-in services so Compose also removes their
    # containers and volumes after packaged-entrypoint checks.
    compose("--profile", "entrypoint", "down", "-v", "--remove-orphans")


def run_attach(backend, connected_probe, lifecycle_probe):
    reset_lab()
    compose("up", "-d", *COMMON_SERVICES, backend)
    compose("run", "--rm", connected_probe)
    compose("stop", "-t", "15", backend)
    compose("run", "--rm", lifecycle_probe)
    print(f"LAB_ATTACH_SUITE_OK={backend.removesuffix('-backend')}")


def run_auth_failure(
    backend,
    connected_probe,
    lifecycle_probe,
    failure_probe,
    mode,
    *,
    recover=False,
    expected_epdg_log=None,
):
    reset_lab()
    compose("up", "-d", *COMMON_SERVICES)
    control = {"mode": mode, "reset_count": True}
    if mode == "timeout":
        control["delay_seconds"] = 3
    broker_request("/control", control)
    compose("up", "-d", backend)
    wait_for_backend_exit(backend)
    compose("run", "--rm", failure_probe)
    state = broker_request("/state")
    if state["authenticate_requests"] < 1:
        raise AssertionError(f"{mode} did not exercise the AKA provider")
    if expected_epdg_log:
        # Docker may report the backend exit before the responder's final log
        # line has reached the Compose log reader.  Poll the bounded log stream
        # so the AUTS assertion observes protocol evidence, not log-driver timing.
        wait_for_log("epdg", expected_epdg_log)
    if recover:
        broker_request("/control", {"mode": "success"})
        compose("start", backend)
        compose("run", "--rm", connected_probe)
        compose("stop", "-t", "15", backend)
        compose("run", "--rm", lifecycle_probe)
    print(f"LAB_AUTH_SCENARIO_OK={backend.removesuffix('-backend')}:{mode}")


def run_auth_faults(backend, connected_probe, lifecycle_probe, failure_probe):
    run_auth_failure(
        backend, connected_probe, lifecycle_probe, failure_probe,
        "unavailable", recover=True,
    )
    run_auth_failure(
        backend, connected_probe, lifecycle_probe, failure_probe, "timeout"
    )
    run_auth_failure(
        backend, connected_probe, lifecycle_probe, failure_probe,
        "auts", expected_epdg_log="received synchronization request",
    )


def epdg_sa_state():
    output = compose(
        "exec", "-T", "epdg", "swanctl", "--list-sas", "--raw", capture=True
    ).stdout
    ike = re.search(
        r"initiator-spi=([0-9a-f]+).*?responder-spi=([0-9a-f]+)", output
    )
    if not ike:
        raise AssertionError("the ePDG has no established IKE SA")
    children = set(re.findall(
        r"state=INSTALLED\b(?:(?!\bstate=).)*?spi-in=([0-9a-f]+)", output
    ))
    if not children:
        raise AssertionError("the ePDG has no installed CHILD SA")
    return {"ike": ike.groups(), "children": children, "raw": output}


def wait_for_sa_change(before, kind, timeout=10):
    deadline = time.monotonic() + timeout
    last = before
    while time.monotonic() < deadline:
        last = epdg_sa_state()
        if last[kind] != before[kind]:
            return last
        time.sleep(0.2)
    raise AssertionError(f"{kind} SA did not change within {timeout}s: {last['raw']}")


def wait_for_log(service, needle, timeout=8):
    deadline = time.monotonic() + timeout
    logs = ""
    while time.monotonic() < deadline:
        logs = compose(
            "logs", "--no-color", "--tail", "300", service, capture=True
        ).stdout
        if needle in logs:
            return logs
        time.sleep(0.2)
    raise AssertionError(f"{service} did not log {needle!r} within {timeout}s")


def start_protocol_lab(backend, connected_probe, **overrides):
    COMPOSE_OVERRIDES.clear()
    COMPOSE_OVERRIDES.update({key: str(value) for key, value in overrides.items()})
    reset_lab()
    compose("up", "-d", *COMMON_SERVICES, backend)
    compose("run", "--rm", connected_probe)


def finish_protocol_lab(backend, lifecycle_probe):
    compose("stop", "-t", "15", backend)
    compose("run", "--rm", lifecycle_probe)


def run_liveness_and_natt(backend, connected_probe, lifecycle_probe):
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_NATT_KEEPALIVE=1,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    state = epdg_sa_state()
    if "encap=yes" not in state["raw"] or "local-port=4500" not in state["raw"]:
        raise AssertionError("the established CHILD SA is not using NAT-T")
    capture = compose(
        "exec", "-T", "epdg", "timeout", "6", "tcpdump", "-ln", "-i", "eth0",
        "-c", "1", "udp dst port 4500 and udp[4:2] = 9 and udp[8] = 0xff",
        capture=True,
    )
    if "1 packet captured" not in capture.stderr and "1 packet captured" not in capture.stdout:
        raise AssertionError("no RFC 3948 NAT-T keepalive was captured")
    compose("run", "--rm", connected_probe)
    finish_protocol_lab(backend, lifecycle_probe)

    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=1,
        LAB_SWU_NATT_KEEPALIVE=20,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    wait_for_log("epdg", "parsed INFORMATIONAL request")
    compose("run", "--rm", connected_probe)
    finish_protocol_lab(backend, lifecycle_probe)
    print(f"LAB_LIVENESS_NATT_OK={backend.removesuffix('-backend')}")


def run_client_rekey(backend, connected_probe, lifecycle_probe, kind):
    timer = {
        "child": {"LAB_SWU_CHILD_REKEY_MINUTES": 0.05, "LAB_SWU_IKE_REKEY_MINUTES": 0},
        "ike": {"LAB_SWU_CHILD_REKEY_MINUTES": 0, "LAB_SWU_IKE_REKEY_MINUTES": 0.05},
    }[kind]
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_NATT_KEEPALIVE=1,
        LAB_SWU_REKEY_TIMEOUT=3,
        **timer,
    )
    before = epdg_sa_state()
    wait_for_sa_change(before, "children" if kind == "child" else "ike")
    compose("run", "--rm", connected_probe)
    finish_protocol_lab(backend, lifecycle_probe)
    print(f"LAB_CLIENT_REKEY_OK={backend.removesuffix('-backend')}:{kind}")


def run_responder_rekey(backend, connected_probe, lifecycle_probe, kind):
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    before = epdg_sa_state()
    target = "children" if kind == "child" else "ike"
    compose("exec", "-T", "epdg", "swanctl", "--rekey", f"--{kind}",
            "ims" if kind == "child" else "viti-lab")
    wait_for_sa_change(before, target)
    compose("run", "--rm", connected_probe)
    finish_protocol_lab(backend, lifecycle_probe)
    print(f"LAB_RESPONDER_REKEY_OBSERVED={backend.removesuffix('-backend')}:{kind}")


def run_operator_delete(backend, connected_probe, terminated_probe):
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    compose("exec", "-T", "epdg", "swanctl", "--terminate", "--ike", "viti-lab")
    wait_for_backend_exit(backend, require_failure=False)
    compose("run", "--rm", terminated_probe)
    print(f"LAB_OPERATOR_DELETE_OBSERVED={backend.removesuffix('-backend')}")


def run_full_reauthentication(backend, connected_probe):
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    before_sa = epdg_sa_state()
    before_auth = broker_request("/state")["authenticate_requests"]
    request_control(backend, "reauth")
    wait_for_broker_requests(before_auth + 1)
    after_sa = wait_for_sa_change(before_sa, "ike", timeout=30)
    if "remote-vips=[2001:db8:39:100::1]" not in after_sa["raw"]:
        raise AssertionError("full reauthentication changed the assigned inner address")
    run_probe(connected_probe, "reauth")
    print(f"LAB_FULL_REAUTH_OBSERVED={backend.removesuffix('-backend')}")


def run_auth_source_loss(backend, connected_probe, lifecycle_probe):
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    before_sa = epdg_sa_state()
    broker_request("/control", {"mode": "unavailable", "reset_count": True})
    run_probe(connected_probe, "transport")
    print(f"LAB_AUTH_SOURCE_LOSS_OK={backend.removesuffix('-backend')}")

    request_control(backend, "reauth")
    wait_for_broker_requests(1)
    time.sleep(2)
    status, _ = backend_container_state(backend)
    if status != "running":
        raise AssertionError("backend exited after authentication-source loss")
    after_sa = epdg_sa_state()
    if after_sa["ike"] != before_sa["ike"]:
        raise AssertionError("failed reauthentication replaced the valid IKE SA")
    run_probe(connected_probe, "transport")
    run_probe(connected_probe, "reauth")
    broker_request("/control", {"mode": "success"})
    print(f"LAB_FAILED_REAUTH_OBSERVED={backend.removesuffix('-backend')}")


def run_colliding_rekeys(backend, connected_probe, kind):
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_REKEY_TIMEOUT=3,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    before_sa = epdg_sa_state()
    # Fault-injection tools belong to the lab probe, not the production image
    # under test.  The probe joins the backend's network namespace, so the
    # qdisc remains attached to the same eth0 after this short-lived helper
    # exits without requiring tc to be packaged in the engine.
    compose(
        "run", "--rm", "--entrypoint", "tc", connected_probe,
        "qdisc", "replace", "dev", "eth0", "root", "netem", "delay", "600ms",
    )
    responder = compose_popen(
        "exec", "-T", "epdg", "sh", "-c",
        "sleep 0.15; exec swanctl --rekey "
        f"--{kind} {'ims' if kind == 'child' else 'viti-lab'}",
    )
    try:
        request_control(backend, f"rekey-{kind}")
        responder_result = responder.wait(timeout=15)
    except subprocess.TimeoutExpired:
        responder.kill()
        responder.wait()
        raise AssertionError(f"responder-side colliding {kind} rekey timed out")
    except Exception:
        responder.terminate()
        responder.wait(timeout=5)
        raise
    time.sleep(2)

    status, _ = backend_container_state(backend)
    transport_ok = False
    if status == "running":
        try:
            run_probe(connected_probe, "transport")
            transport_ok = True
        except subprocess.CalledProcessError:
            transport_ok = False

    if responder_result != 0:
        raise AssertionError(
            f"responder-side colliding {kind} rekey failed with {responder_result}"
        )
    if not transport_ok:
        raise AssertionError(f"transport failed after colliding {kind} rekeys")
    wait_for_sa_change(before_sa, "children" if kind == "child" else "ike")
    print(f"LAB_COLLIDING_REKEY_OK={backend.removesuffix('-backend')}:{kind}")
    print(f"LAB_COLLIDING_REKEY_OBSERVED={backend.removesuffix('-backend')}:{kind}")


def crash_backend(service):
    compose("exec", "-T", service, "pkill", "-KILL", "-x", "charon")
    wait_for_backend_exit(service)


def run_crash_recovery(backend, connected_probe, lifecycle_probe):
    start_protocol_lab(
        backend,
        connected_probe,
        LAB_SWU_LIVENESS_PERIOD=0,
        LAB_SWU_CHILD_REKEY_MINUTES=0,
        LAB_SWU_IKE_REKEY_MINUTES=0,
    )
    crash_backend(backend)
    run_probe(lifecycle_probe, "crashed")
    compose("start", backend)
    run_probe(connected_probe, "transport")
    run_probe(lifecycle_probe, "recovered")
    print(f"LAB_CRASH_RECOVERY_OBSERVED={backend.removesuffix('-backend')}")


def run_protocol_scenarios(
    backend, connected_probe, lifecycle_probe, terminated_probe
):
    run_liveness_and_natt(backend, connected_probe, lifecycle_probe)
    run_client_rekey(backend, connected_probe, lifecycle_probe, "child")
    run_client_rekey(backend, connected_probe, lifecycle_probe, "ike")
    run_responder_rekey(backend, connected_probe, lifecycle_probe, "child")
    run_responder_rekey(backend, connected_probe, lifecycle_probe, "ike")
    run_operator_delete(backend, connected_probe, terminated_probe)


def run_resilience_scenarios(backend, connected_probe, lifecycle_probe):
    run_full_reauthentication(backend, connected_probe)
    run_auth_source_loss(backend, connected_probe, lifecycle_probe)
    run_colliding_rekeys(backend, connected_probe, "child")
    run_colliding_rekeys(backend, connected_probe, "ike")
    run_crash_recovery(backend, connected_probe, lifecycle_probe)


def run_production_entrypoint_recovery_and_shutdown():
    """Crash charon in the packaged namespace, recover, then stop PID 1."""
    reset_lab()
    compose(
        "--profile", "entrypoint", "up", "-d",
        "epdg", "aka-broker", "nat-gateway", "engine-entrypoint",
    )
    wait_for_log("engine-entrypoint", "starting Asterisk", timeout=120)
    compose(
        "--profile", "entrypoint", "run", "--rm", "engine-entrypoint-probe"
    )
    before = compose(
        "--profile", "entrypoint", "ps", "-q", "engine-entrypoint", capture=True
    ).stdout.strip()
    compose(
        "--profile", "entrypoint", "exec", "-T",
        "engine-entrypoint", "pkill", "-KILL", "-x", "charon",
    )
    compose(
        "--profile", "entrypoint", "run", "--rm",
        "-e", "LAB_PROBE_MODE=recovered", "engine-entrypoint-probe",
    )
    after = compose(
        "--profile", "entrypoint", "ps", "-q", "engine-entrypoint", capture=True
    ).stdout.strip()
    if not before or before != after:
        raise AssertionError(
            "production entrypoint container was recreated during charon recovery"
        )
    print("LAB_PRODUCTION_ENTRYPOINT_CRASH_RECOVERY_OK=strongswan")

    compose("--profile", "entrypoint", "stop", "-t", "20", "engine-entrypoint")
    compose(
        "--profile", "entrypoint", "run", "--rm",
        "engine-entrypoint-terminated-probe",
    )
    print("LAB_PRODUCTION_ENTRYPOINT_SHUTDOWN_OK=strongswan")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--backend-image",
        help=(
            "use an already-built production image for the strongSwan backend; "
            "the lab still builds its independent ePDG, broker, and probes"
        ),
    )
    parser.add_argument(
        "--scenario",
        choices=(
            "all", "attach", "auth-faults", "protocol", "resilience", "entrypoint",
        ),
        default="all",
    )
    parser.add_argument("--no-build", action="store_true")
    parser.add_argument("--keep", action="store_true", help="leave the lab running")
    args = parser.parse_args()
    if args.scenario == "entrypoint" and not args.backend_image:
        parser.error("the entrypoint scenario requires --backend-image")

    (
        backend,
        connected_probe,
        lifecycle_probe,
        failure_probe,
        terminated_probe,
    ) = BACKEND_SERVICES
    services = (*COMMON_SERVICES, *BACKEND_SERVICES)
    if args.backend_image:
        COMPOSE_FIXED_OVERRIDES["LAB_STRONGSWAN_BACKEND_IMAGE"] = args.backend_image
        COMPOSE_FIXED_OVERRIDES["LAB_ENGINE_IMAGE"] = args.backend_image
    reset_lab()
    try:
        if not args.no_build:
            build_services = tuple(
                service for service in services
                if not args.backend_image or service != backend
            )
            build_services_serially(build_services)
        if args.backend_image:
            inspected = subprocess.run(
                ["docker", "image", "inspect", args.backend_image],
                check=False,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            if inspected.returncode != 0:
                parser.error(f"backend image does not exist locally: {args.backend_image}")
            print(f"LAB_PACKAGED_BACKEND_IMAGE={args.backend_image}", flush=True)
        if args.scenario in ("all", "attach"):
            run_attach(backend, connected_probe, lifecycle_probe)
        if args.scenario in ("all", "auth-faults"):
            run_auth_faults(backend, connected_probe, lifecycle_probe, failure_probe)
        if args.scenario in ("all", "protocol"):
            run_protocol_scenarios(
                backend, connected_probe, lifecycle_probe, terminated_probe
            )
        if args.scenario in ("all", "resilience"):
            run_resilience_scenarios(backend, connected_probe, lifecycle_probe)
        if args.backend_image and args.scenario in ("all", "resilience", "entrypoint"):
            run_production_entrypoint_recovery_and_shutdown()
        print("LAB_SUITE_OK=strongswan")
    except Exception:
        try:
            logs = compose(
                "logs", "--no-color", "--tail", "300", *COMMON_SERVICES, backend,
                capture=True,
            )
            sys.stderr.write(logs.stdout)
            sys.stderr.write(logs.stderr)
        except subprocess.CalledProcessError as log_error:
            sys.stderr.write(log_error.stdout or "")
            sys.stderr.write(log_error.stderr or "")
        raise
    finally:
        if not args.keep:
            reset_lab()
        COMPOSE_FIXED_OVERRIDES.clear()
        COMPOSE_OVERRIDES.clear()


if __name__ == "__main__":
    main()
