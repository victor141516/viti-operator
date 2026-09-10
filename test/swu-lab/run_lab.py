#!/usr/bin/env python3
"""Run the same external SWu laboratory lifecycle for either backend."""

import argparse
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[2]
COMPOSE_FILE = ROOT / "test" / "swu-lab" / "compose.yml"
COMMON_SERVICES = ("epdg", "aka-broker")
BACKEND_SERVICES = {
    "python": ("python-backend", "python-probe", "python-lifecycle-probe"),
    "strongswan": (
        "strongswan-backend",
        "strongswan-probe",
        "strongswan-lifecycle-probe",
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--backend", choices=sorted(BACKEND_SERVICES), required=True)
    parser.add_argument("--no-build", action="store_true")
    parser.add_argument("--keep", action="store_true", help="leave the lab running")
    args = parser.parse_args()

    backend, connected_probe, lifecycle_probe = BACKEND_SERVICES[args.backend]
    services = (*COMMON_SERVICES, backend, connected_probe, lifecycle_probe)
    compose("down", "-v", "--remove-orphans")
    try:
        if not args.no_build:
            compose("build", *services)
        compose("up", "-d", *COMMON_SERVICES, backend)
        compose("run", "--rm", connected_probe)
        compose("stop", "-t", "15", backend)
        compose("run", "--rm", lifecycle_probe)
        print(f"LAB_SUITE_OK={args.backend}")
    except subprocess.CalledProcessError:
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
            compose("down", "-v", "--remove-orphans")


if __name__ == "__main__":
    main()
