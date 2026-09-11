#!/usr/bin/env python3
"""Black-box assertions for a running SWu backend in the deterministic lab."""

import json
import os
from pathlib import Path
import socket
import subprocess
import time

from tunnel_contract import validate_status, validate_transition_sequence


RUNDIR = Path(os.environ.get("VOWIFI_RUNDIR", "/run/vowifi"))
EXPECTED_BACKEND = "strongswan"
EXPECTED_INNER = os.environ.get("LAB_EXPECTED_INNER", "2001:db8:39:100::1")
EXPECTED_PCSCF = os.environ.get("LAB_EXPECTED_PCSCF", "2001:db8:39:200::10")
EPDG_ADDRESS = os.environ.get("LAB_EPDG", os.environ.get("SWU_EPDG", "172.30.40.10"))
ECHO_PORT = int(os.environ.get("LAB_ECHO_PORT", "18080"))
TIMEOUT = float(os.environ.get("LAB_ATTACH_TIMEOUT", "30"))
MODE = os.environ.get("LAB_PROBE_MODE", "connected")


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


def load_all_transitions():
    events = []
    for path in (RUNDIR / "event-outbox").glob("*.json"):
        event = json.loads(path.read_text(encoding="utf-8"))
        if event.get("event") in {"tunnel_start", "tunnel_up", "tunnel_down"}:
            events.append(event)
    events.sort(key=lambda event: event["recorded_at_ns"])
    return events


def load_transitions():
    events = load_all_transitions()
    starts = [event for event in events if event["event"] == "tunnel_start"]
    if not starts:
        return events
    latest_generation = starts[-1]["tunnel_generation"]
    return sorted(
        (event for event in events if event["tunnel_generation"] == latest_generation),
        key=lambda event: event["tunnel_sequence"],
    )


def grouped_transitions():
    groups = []
    by_generation = {}
    for event in load_all_transitions():
        generation = event["tunnel_generation"]
        if generation not in by_generation:
            by_generation[generation] = []
            groups.append(by_generation[generation])
        by_generation[generation].append(event)
    for events in groups:
        events.sort(key=lambda event: event["tunnel_sequence"])
    return groups


def echo(sock_type, payload):
    with socket.socket(socket.AF_INET6, sock_type) as sock:
        sock.settimeout(5)
        sock.bind((EXPECTED_INNER, 0))
        if sock_type == socket.SOCK_DGRAM:
            sock.sendto(payload, (EXPECTED_PCSCF, ECHO_PORT))
            received, _ = sock.recvfrom(len(payload) + 1)
        else:
            sock.connect((EXPECTED_PCSCF, ECHO_PORT))
            sock.sendall(payload)
            chunks = []
            remaining = len(payload)
            while remaining:
                chunk = sock.recv(remaining)
                if not chunk:
                    raise AssertionError(
                        f"TCP echo closed with {remaining} byte(s) still missing"
                    )
                chunks.append(chunk)
                remaining -= len(chunk)
            received = b"".join(chunks)
    if received != payload:
        raise AssertionError(f"echo mismatch: {received!r} != {payload!r}")


def probe_transport():
    observation = wait_for_connected()
    assert observation.backend == EXPECTED_BACKEND
    assert observation.inner_ip == EXPECTED_INNER
    assert observation.pcscf == EXPECTED_PCSCF
    assert observation.iface == "ipsec0"

    link = command("ip", "-details", "link", "show", "dev", observation.iface)
    assert "tun type tun" in link
    assert "mtu 1400" in link
    addresses = command("ip", "-6", "addr", "show", "dev", observation.iface)
    assert EXPECTED_INNER in addresses

    outer_route = command("ip", "route", "get", EPDG_ADDRESS)
    assert "dev ipsec0" not in outer_route
    broker_address = socket.gethostbyname("aka-broker")
    broker_route = command("ip", "route", "get", broker_address)
    assert "dev ipsec0" not in broker_route
    rules = command("ip", "-6", "rule", "show")
    assert f"from {EXPECTED_INNER} lookup 220" in rules
    assert "from all lookup 220" not in rules
    inner_route = command(
        "ip", "-6", "route", "get", EXPECTED_PCSCF, "from", EXPECTED_INNER
    )
    assert "dev ipsec0" in inner_route

    # This is a userspace ESP dataplane. In particular, loading kernel-netlink
    # for Linux address/route discovery must never select its XFRM IPsec backend
    # instead of kernel-libipsec.
    assert command("ip", "xfrm", "state").strip() == ""
    assert command("ip", "xfrm", "policy").strip() == ""

    echo(socket.SOCK_DGRAM, b"viti-swu-lab-udp")
    echo(socket.SOCK_STREAM, b"viti-swu-lab-tcp")
    echo(socket.SOCK_DGRAM, bytes(range(256)) * 16)
    echo(socket.SOCK_STREAM, bytes(range(256)) * 512)
    return observation


def probe_connected():
    probe_transport()
    transitions = load_transitions()
    validate_transition_sequence(transitions)
    assert [event["event"] for event in transitions] == ["tunnel_start", "tunnel_up"]
    print(f"LAB_BACKEND_OK={EXPECTED_BACKEND}")


def probe_transport_only():
    probe_transport()
    print(f"LAB_TRANSPORT_OK={EXPECTED_BACKEND}")


def probe_reauthentication():
    probe_transport()
    transitions = load_transitions()
    names = [event["event"] for event in transitions]
    validate_transition_sequence(transitions)
    assert names == ["tunnel_start", "tunnel_up"]
    print(f"LAB_REAUTH_OK={EXPECTED_BACKEND}")


def probe_crashed():
    transitions = load_transitions()
    names = [event["event"] for event in transitions]
    observation = wait_for_down()
    assert observation.backend == EXPECTED_BACKEND
    validate_transition_sequence(transitions)
    assert names == ["tunnel_start", "tunnel_up", "tunnel_down"]
    print(f"LAB_CRASH_OBSERVED={EXPECTED_BACKEND}")


def probe_recovered():
    observation = wait_for_connected()
    assert observation.backend == EXPECTED_BACKEND
    groups = grouped_transitions()
    assert len(groups) == 2
    assert groups[0][0]["tunnel_generation"] != groups[1][0]["tunnel_generation"]
    first_names = [event["event"] for event in groups[0]]
    second_names = [event["event"] for event in groups[1]]
    assert first_names == ["tunnel_start", "tunnel_up", "tunnel_down"]
    validate_transition_sequence(groups[0])
    validate_transition_sequence(groups[1])
    assert second_names == ["tunnel_start", "tunnel_up"]
    assert groups[0][-1]["recorded_at_ns"] < groups[1][0]["recorded_at_ns"]
    print(f"LAB_RECOVERY_OK={EXPECTED_BACKEND}")


def probe_authentication_recovered():
    observation = wait_for_connected()
    assert observation.backend == EXPECTED_BACKEND
    groups = grouped_transitions()
    assert len(groups) == 2
    assert [event["event"] for event in groups[0]] == [
        "tunnel_start", "tunnel_up", "tunnel_down"
    ]
    assert [event["event"] for event in groups[1]] == [
        "tunnel_start", "tunnel_up"
    ]
    for events in groups:
        validate_transition_sequence(events)
    print(f"LAB_AUTH_RECOVERY_OK={EXPECTED_BACKEND}")


def probe_lifecycle():
    observation = wait_for_down()
    assert observation.backend == EXPECTED_BACKEND
    transitions = load_transitions()
    names = [event["event"] for event in transitions]
    validate_transition_sequence(transitions)
    assert names == ["tunnel_start", "tunnel_up", "tunnel_down"]
    print(f"LAB_LIFECYCLE_OK={EXPECTED_BACKEND}")


def probe_failed_attach():
    observation = wait_for_down()
    assert observation.backend == EXPECTED_BACKEND
    transitions = load_transitions()
    validate_transition_sequence(transitions)
    assert [event["event"] for event in transitions] == ["tunnel_start", "tunnel_down"]
    print(f"LAB_FAILED_ATTACH_OK={EXPECTED_BACKEND}")


def probe_terminated():
    observation = wait_for_down()
    assert observation.backend == EXPECTED_BACKEND
    transitions = load_transitions()
    validate_transition_sequence(transitions)
    assert [event["event"] for event in transitions] == [
        "tunnel_start", "tunnel_up", "tunnel_down"
    ]
    print(f"LAB_OPERATOR_DELETE_OK={EXPECTED_BACKEND}")


def main():
    if MODE == "connected":
        probe_connected()
    elif MODE == "lifecycle":
        probe_lifecycle()
    elif MODE == "failed_attach":
        probe_failed_attach()
    elif MODE == "terminated":
        probe_terminated()
    elif MODE == "transport":
        probe_transport_only()
    elif MODE == "reauth":
        probe_reauthentication()
    elif MODE == "crashed":
        probe_crashed()
    elif MODE == "recovered":
        probe_recovered()
    elif MODE == "auth_recovered":
        probe_authentication_recovered()
    else:
        raise ValueError(f"unknown LAB_PROBE_MODE: {MODE}")


if __name__ == "__main__":
    main()
