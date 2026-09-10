#!/usr/bin/env python3
"""Black-box assertions for a running SWu backend in the deterministic lab."""

import json
import os
from pathlib import Path
import socket
import subprocess
import time

from tunnel_contract import ContractViolation, validate_status, validate_transition_sequence


RUNDIR = Path(os.environ.get("VOWIFI_RUNDIR", "/run/vowifi"))
EXPECTED_BACKEND = os.environ["LAB_EXPECTED_BACKEND"]
EXPECTED_INNER = os.environ.get("LAB_EXPECTED_INNER", "2001:db8:39:100::1")
EXPECTED_PCSCF = os.environ.get("LAB_EXPECTED_PCSCF", "2001:db8:39:200::10")
ECHO_PORT = int(os.environ.get("LAB_ECHO_PORT", "18080"))
TIMEOUT = float(os.environ.get("LAB_ATTACH_TIMEOUT", "30"))
MODE = os.environ.get("LAB_PROBE_MODE", "connected")
EXPECT_DUPLICATE_DOWN = os.environ.get("LAB_EXPECT_LEGACY_DUPLICATE_DOWN") == "1"


def command(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT)


def wait_for_connected():
    deadline = time.monotonic() + TIMEOUT
    last = "status not published"
    while time.monotonic() < deadline:
        try:
            payload = json.loads((RUNDIR / "swu_status.json").read_text(encoding="utf-8"))
            observation = validate_status(payload)
            if observation.state == "CONNECTED":
                return observation
            last = f"state={observation.state}"
        except Exception as error:
            last = repr(error)
        time.sleep(0.2)
    raise AssertionError(f"backend did not connect within {TIMEOUT}s: {last}")


def wait_for_down():
    deadline = time.monotonic() + TIMEOUT
    last = "status not published"
    while time.monotonic() < deadline:
        try:
            payload = json.loads((RUNDIR / "swu_status.json").read_text(encoding="utf-8"))
            observation = validate_status(payload)
            if observation.state == "DOWN":
                return observation
            last = f"state={observation.state}"
        except Exception as error:
            last = repr(error)
        time.sleep(0.2)
    raise AssertionError(f"backend did not stop within {TIMEOUT}s: {last}")


def load_transitions():
    events = []
    for path in (RUNDIR / "event-outbox").glob("*.json"):
        event = json.loads(path.read_text(encoding="utf-8"))
        if event.get("event") in {"tunnel_start", "tunnel_up", "tunnel_down"}:
            events.append(event)
    return sorted(events, key=lambda event: event["tunnel_sequence"])


def echo(sock_type, payload):
    with socket.socket(socket.AF_INET6, sock_type) as sock:
        sock.settimeout(5)
        if sock_type == socket.SOCK_DGRAM:
            sock.sendto(payload, (EXPECTED_PCSCF, ECHO_PORT))
            received, _ = sock.recvfrom(len(payload) + 1)
        else:
            sock.connect((EXPECTED_PCSCF, ECHO_PORT))
            sock.sendall(payload)
            received = sock.recv(len(payload) + 1)
    if received != payload:
        raise AssertionError(f"echo mismatch: {received!r} != {payload!r}")


def probe_connected():
    observation = wait_for_connected()
    assert observation.backend == EXPECTED_BACKEND
    assert observation.inner_ip == EXPECTED_INNER
    assert observation.pcscf == EXPECTED_PCSCF
    assert observation.iface == "ipsec0"

    transitions = load_transitions()
    validate_transition_sequence(transitions)
    assert [event["event"] for event in transitions] == ["tunnel_start", "tunnel_up"]

    link = command("ip", "-details", "link", "show", "dev", observation.iface)
    assert "tun type tun" in link
    assert "mtu 1400" in link
    addresses = command("ip", "-6", "addr", "show", "dev", observation.iface)
    assert EXPECTED_INNER in addresses

    outer_route = command("ip", "route", "get", "172.30.39.10")
    assert "dev ipsec0" not in outer_route
    broker_address = socket.gethostbyname("aka-broker")
    broker_route = command("ip", "route", "get", broker_address)
    assert "dev ipsec0" not in broker_route
    inner_route = command("ip", "-6", "route", "get", EXPECTED_PCSCF)
    assert "dev ipsec0" in inner_route

    echo(socket.SOCK_DGRAM, b"viti-swu-lab-udp")
    echo(socket.SOCK_STREAM, b"viti-swu-lab-tcp")
    print(f"LAB_BACKEND_OK={EXPECTED_BACKEND}")


def probe_lifecycle():
    observation = wait_for_down()
    assert observation.backend == EXPECTED_BACKEND
    transitions = load_transitions()
    names = [event["event"] for event in transitions]
    if EXPECT_DUPLICATE_DOWN:
        assert EXPECTED_BACKEND == "python"
        assert names == ["tunnel_start", "tunnel_up", "tunnel_down", "tunnel_down"]
        try:
            validate_transition_sequence(transitions)
        except ContractViolation as error:
            assert "invalid tunnel lifecycle" in str(error)
        else:
            raise AssertionError("legacy duplicate tunnel_down was unexpectedly accepted")
        print("LAB_XFAIL=python:duplicate_tunnel_down")
        return
    validate_transition_sequence(transitions)
    assert names == ["tunnel_start", "tunnel_up", "tunnel_down"]
    print(f"LAB_LIFECYCLE_OK={EXPECTED_BACKEND}")


def main():
    if MODE == "connected":
        probe_connected()
    elif MODE == "lifecycle":
        probe_lifecycle()
    else:
        raise ValueError(f"unknown LAB_PROBE_MODE: {MODE}")


if __name__ == "__main__":
    main()
