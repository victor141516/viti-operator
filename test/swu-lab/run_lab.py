#!/usr/bin/env python3
"""Run the same external SWu laboratory lifecycle for either backend."""

import argparse
import json
from pathlib import Path
import subprocess
import sys
import time


ROOT = Path(__file__).resolve().parents[2]
COMPOSE_FILE = ROOT / "test" / "swu-lab" / "compose.yml"
COMMON_SERVICES = ("epdg", "aka-broker")
BACKEND_SERVICES = {
    "python": (
        "python-backend",
        "python-probe",
        "python-lifecycle-probe",
        "python-failure-probe",
    ),
    "strongswan": (
        "strongswan-backend",
        "strongswan-probe",
        "strongswan-lifecycle-probe",
        "strongswan-failure-probe",
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


def wait_for_backend_exit(service, timeout=15):
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
            if int(exit_code) == 0:
                raise AssertionError(f"{service} unexpectedly exited successfully")
            return
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--backend", choices=sorted(BACKEND_SERVICES), required=True)
    parser.add_argument(
        "--scenario", choices=("all", "attach", "auth-faults"), default="all"
    )
    parser.add_argument("--no-build", action="store_true")
    parser.add_argument("--keep", action="store_true", help="leave the lab running")
    args = parser.parse_args()

    backend, connected_probe, lifecycle_probe, failure_probe = BACKEND_SERVICES[args.backend]
    services = (*COMMON_SERVICES, *BACKEND_SERVICES[args.backend])
    reset_lab()
    try:
        if not args.no_build:
            compose("build", *services)
        if args.scenario in ("all", "attach"):
            run_attach(backend, connected_probe, lifecycle_probe)
        if args.scenario in ("all", "auth-faults"):
            run_auth_faults(backend, connected_probe, lifecycle_probe, failure_probe)
        print(f"LAB_SUITE_OK={args.backend}")
    except Exception:
        try:
            logs = compose("logs", "--no-color", *COMMON_SERVICES, backend, capture=True)
            sys.stderr.write(logs.stdout)
            sys.stderr.write(logs.stderr)
        except subprocess.CalledProcessError as log_error:
            sys.stderr.write(log_error.stdout or "")
            sys.stderr.write(log_error.stderr or "")
        raise
    finally:
        if not args.keep:
            reset_lab()


if __name__ == "__main__":
    main()
