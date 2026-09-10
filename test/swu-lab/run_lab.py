#!/usr/bin/env python3
"""Run the same external SWu laboratory lifecycle for either backend."""

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
COMMON_SERVICES = ("epdg", "aka-broker")
COMPOSE_OVERRIDES = {}
BACKEND_SERVICES = {
    "python": (
        "python-backend",
        "python-probe",
        "python-lifecycle-probe",
        "python-failure-probe",
        "python-terminated-probe",
    ),
    "strongswan": (
        "strongswan-backend",
        "strongswan-probe",
        "strongswan-lifecycle-probe",
        "strongswan-failure-probe",
        "strongswan-terminated-probe",
    ),
}


def compose(*args, capture=False):
    command = ["docker", "compose", "-f", str(COMPOSE_FILE), *args]
    print("+", " ".join(command), flush=True)
    return subprocess.run(
        command,
        cwd=ROOT,
        check=True,
        text=True,
        capture_output=capture,
        env={**os.environ, **COMPOSE_OVERRIDES},
    )


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


def reset_lab():
    compose("down", "-v", "--remove-orphans")


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
        logs = compose("logs", "--no-color", "epdg", capture=True).stdout
        if expected_epdg_log not in logs:
            raise AssertionError(
                f"{mode} did not produce expected ePDG evidence: {expected_epdg_log!r}"
            )
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
        LAB_SWU_LIVENESS_PERIOD=1,
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
    if backend.startswith("python-"):
        time.sleep(1)
        after = epdg_sa_state()
        if after[target] != before[target]:
            raise AssertionError(f"legacy Python unexpectedly completed responder {kind} rekey")
        print(f"LAB_XFAIL=python:responder_{kind}_rekey")
    else:
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


def run_protocol_scenarios(
    backend, connected_probe, lifecycle_probe, terminated_probe
):
    run_liveness_and_natt(backend, connected_probe, lifecycle_probe)
    run_client_rekey(backend, connected_probe, lifecycle_probe, "child")
    run_client_rekey(backend, connected_probe, lifecycle_probe, "ike")
    run_responder_rekey(backend, connected_probe, lifecycle_probe, "child")
    run_responder_rekey(backend, connected_probe, lifecycle_probe, "ike")
    run_operator_delete(backend, connected_probe, terminated_probe)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--backend", choices=sorted(BACKEND_SERVICES), required=True)
    parser.add_argument(
        "--scenario",
        choices=("all", "attach", "auth-faults", "protocol"),
        default="all",
    )
    parser.add_argument("--no-build", action="store_true")
    parser.add_argument("--keep", action="store_true", help="leave the lab running")
    args = parser.parse_args()

    (
        backend,
        connected_probe,
        lifecycle_probe,
        failure_probe,
        terminated_probe,
    ) = BACKEND_SERVICES[args.backend]
    services = (*COMMON_SERVICES, *BACKEND_SERVICES[args.backend])
    reset_lab()
    try:
        if not args.no_build:
            compose("build", *services)
        if args.scenario in ("all", "attach"):
            run_attach(backend, connected_probe, lifecycle_probe)
        if args.scenario in ("all", "auth-faults"):
            run_auth_faults(backend, connected_probe, lifecycle_probe, failure_probe)
        if args.scenario in ("all", "protocol"):
            run_protocol_scenarios(
                backend, connected_probe, lifecycle_probe, terminated_probe
            )
        print(f"LAB_SUITE_OK={args.backend}")
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
        COMPOSE_OVERRIDES.clear()


if __name__ == "__main__":
    main()
