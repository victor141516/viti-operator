#!/usr/bin/env python3
"""Stable observable contract shared by SWu tunnel backends.

This module deliberately contains no IKE or IPsec implementation details.  It
defines the runtime state that the engine entrypoint, manager and tests may
observe regardless of whether the tunnel is owned by the legacy Python client
or by strongSwan.
"""

from dataclasses import dataclass
import ipaddress
import json
import math
from pathlib import Path
import time


CONTRACT_VERSION = 1
BACKENDS = frozenset(("python", "strongswan"))
STATES = frozenset(("CONNECTING", "CONNECTED", "DOWN"))
TRANSITIONS = frozenset(("tunnel_start", "tunnel_up", "tunnel_down"))
CONTROL_ACTIONS = frozenset(("stop", "reauth", "rekey-child", "rekey-ike"))
CONTROL_ALIASES = {
    "q": "stop",
    "r": "reauth",
    "c": "rekey-child",
    "i": "rekey-ike",
}


class ContractViolation(ValueError):
    """Raised when a backend publishes an invalid observable value."""


def normalize_control_action(action):
    """Normalize one newline-delimited command accepted by ``swu.ctl``."""
    action = str(action).strip().lower()
    action = CONTROL_ALIASES.get(action, action)
    if action not in CONTROL_ACTIONS:
        raise ContractViolation("unknown tunnel control action")
    return action


def encode_control_action(action):
    return (normalize_control_action(action) + "\n").encode("ascii")


@dataclass(frozen=True)
class TunnelObservation:
    backend: str
    state: str
    timestamp: int
    inner_ip: str = ""
    pcscf: str = ""
    iface: str = ""


def status_payload(backend, state, timestamp, **extra):
    """Build the versioned status envelope without discarding backend fields."""
    payload = {
        "contract_version": CONTRACT_VERSION,
        "backend": str(backend),
        "state": str(state),
        "ts": int(timestamp),
    }
    payload.update(extra)
    validate_status(payload)
    return payload


def validate_status(payload):
    """Validate and normalize one externally observed ``swu_status.json``."""
    if not isinstance(payload, dict):
        raise ContractViolation("tunnel status must be a JSON object")
    if payload.get("contract_version") != CONTRACT_VERSION:
        raise ContractViolation("unsupported tunnel contract version")
    backend = payload.get("backend")
    if backend not in BACKENDS:
        raise ContractViolation("unknown tunnel backend")
    state = payload.get("state")
    if state not in STATES:
        raise ContractViolation("unknown tunnel state")
    timestamp = payload.get("ts")
    if not isinstance(timestamp, int) or isinstance(timestamp, bool) or timestamp <= 0:
        raise ContractViolation("tunnel status timestamp must be a positive integer")

    inner_ip = str(payload.get("inner_ip") or "")
    pcscf = str(payload.get("pcscf") or "")
    iface = str(payload.get("iface") or "")
    if state == "CONNECTED":
        if not inner_ip or not iface:
            raise ContractViolation("connected status requires inner_ip and iface")
        try:
            ipaddress.ip_address(inner_ip)
        except ValueError as error:
            raise ContractViolation("connected inner_ip must be an IP literal") from error
        if pcscf:
            try:
                ipaddress.ip_address(pcscf)
            except ValueError as error:
                raise ContractViolation("P-CSCF must be an IP literal when present") from error

    return TunnelObservation(
        backend=backend,
        state=state,
        timestamp=timestamp,
        inner_ip=inner_ip,
        pcscf=pcscf,
        iface=iface,
    )


def read_status(rundir):
    """Read and validate the backend status through its public filesystem API."""
    path = Path(rundir) / "swu_status.json"
    return validate_status(json.loads(path.read_text(encoding="utf-8")))


def supervisor_retry_delay(rundir, minimum, now=None, maximum=86400):
    """Return a restart delay that honours a backend/carrier retry timer.

    A backend may publish ``backoff`` in its DOWN status (for example the
    3GPP BACKOFF_TIMER supplied by an ePDG).  The timer starts at the status
    timestamp, so process shutdown time counts towards it.  Invalid or stale
    status must never make the supervisor restart faster than its own
    exponential minimum, and an untrusted value is bounded to one day.
    """
    minimum = max(0, float(minimum))
    maximum = max(minimum, float(maximum))
    try:
        payload = json.loads(
            (Path(rundir) / "swu_status.json").read_text(encoding="utf-8")
        )
        validate_status(payload)
        backoff = payload.get("backoff")
        if isinstance(backoff, bool):
            raise ValueError("boolean backoff")
        backoff = float(backoff)
        if not math.isfinite(backoff) or backoff <= 0:
            raise ValueError("invalid backoff")
        timestamp = float(payload["ts"])
        current = time.time() if now is None else float(now)
        elapsed = max(0.0, current - timestamp)
        remaining = max(0.0, min(backoff, maximum) - elapsed)
        return int(math.ceil(max(minimum, remaining)))
    except (OSError, ValueError, TypeError, json.JSONDecodeError, ContractViolation):
        return int(math.ceil(minimum))


def validate_transition_sequence(events):
    """Validate durable lifecycle events from a single backend generation."""
    events = list(events)
    if not events:
        raise ContractViolation("tunnel transition sequence is empty")
    generation = events[0].get("tunnel_generation")
    if not isinstance(generation, str) or not generation:
        raise ContractViolation("tunnel generation is missing")
    expected_sequence = 1
    for event in events:
        if event.get("event") not in TRANSITIONS:
            raise ContractViolation("unexpected tunnel transition")
        if event.get("tunnel_generation") != generation:
            raise ContractViolation("transition sequence crosses tunnel generations")
        if event.get("tunnel_sequence") != expected_sequence:
            raise ContractViolation("tunnel transition sequence is not contiguous")
        timestamp = event.get("recorded_at_ns")
        if not isinstance(timestamp, int) or isinstance(timestamp, bool) or timestamp <= 0:
            raise ContractViolation("transition timestamp is invalid")
        expected_sequence += 1
    if events[0]["event"] != "tunnel_start":
        raise ContractViolation("first transition must claim tunnel_start")
    observed = tuple(event["event"] for event in events)
    allowed = {
        ("tunnel_start",),
        ("tunnel_start", "tunnel_up"),
        ("tunnel_start", "tunnel_down"),
        ("tunnel_start", "tunnel_up", "tunnel_down"),
    }
    if observed not in allowed:
        raise ContractViolation(f"invalid tunnel lifecycle: {observed!r}")
    return generation
