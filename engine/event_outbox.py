#!/usr/bin/env python3
"""Durably relay irreplaceable engine events to the manager.

Files are written atomically below the bind-mounted runtime directory before an
AMI callback returns.  A separate worker retries transient failures until the
manager has durably accepted the event.  The manager's event handlers are
idempotent, so losing the HTTP response and retrying is safe.
"""
import json
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.request
import uuid

RUNDIR = os.environ.get("VOWIFI_RUNDIR", "/run/vowifi")
OUTBOX = Path(os.environ.get("VITI_EVENT_OUTBOX_DIR", os.path.join(RUNDIR, "event-outbox")))


class _Response:
    def __init__(self, status_code):
        self.status_code = status_code


class _HTTPSession:
    @staticmethod
    def post(url, **kwargs):
        request = urllib.request.Request(
            url,
            data=json.dumps(kwargs["json"], separators=(",", ":")).encode("utf-8"),
            headers={"Content-Type": "application/json", **kwargs.get("headers", {})},
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=kwargs.get("timeout", 10)) as response:
                return _Response(response.status)
        except urllib.error.HTTPError as error:
            return _Response(error.code)


HTTP = _HTTPSession()


def load_env():
    values = {}
    path = os.environ.get("VOWIFI_ENV", os.path.join(RUNDIR, "engine.env"))
    try:
        with open(path, encoding="utf-8") as source:
            for line in source:
                if "=" in line:
                    key, value = line.rstrip("\n").split("=", 1)
                    values[key] = value
    except OSError:
        pass
    return values


def _sync_directory(directory):
    try:
        descriptor = os.open(directory, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError:
        # The atomic rename is still useful on filesystems without directory fsync.
        pass


def enqueue_event(event, args, recorded_at_ns=None, outbox=None, extra=None):
    """Persist one event and return only after its final filename is durable."""
    directory = Path(outbox or OUTBOX)
    directory.mkdir(parents=True, exist_ok=True)
    env = load_env()
    event_id = uuid.uuid4().hex
    payload = {
        "event_id": event_id,
        "instance": os.environ.get("VOWIFI_ID") or env.get("VOWIFI_ID", "1"),
        "event": event,
        "args": [str(value) for value in args],
        "recorded_at_ns": int(recorded_at_ns or time.time_ns()),
    }
    for key in ("tunnel_generation", "tunnel_sequence", "call_id", "trace_id"):
        if extra and key in extra:
            payload[key] = extra[key]
    final_path = directory / ("%020d-%s.json" % (payload["recorded_at_ns"], event_id))
    temporary = directory / (".%s.tmp" % event_id)
    encoded = json.dumps(payload, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    try:
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            output.write(encoded)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, final_path)
        _sync_directory(str(directory))
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
    return final_path


def _manager_settings():
    env = load_env()
    manager_url = os.environ.get("MANAGER_URL") or env.get("MANAGER_URL", "")
    token = os.environ.get("VOWIFI_MANAGER_TOKEN") or env.get("VOWIFI_MANAGER_TOKEN", "")
    return manager_url.rstrip("/"), token


def deliver_file(path, manager_url=None, token=None, session=HTTP):
    """Try one file. Return True only after a 2xx response and durable removal."""
    path = Path(path)
    if manager_url is None:
        manager_url, configured_token = _manager_settings()
        token = configured_token if token is None else token
    if not manager_url:
        return False
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
        response = session.post(
            manager_url + "/api/engine/event",
            json=payload,
            headers={"X-Viti-Engine-Token": token} if token else {},
            timeout=10,
        )
    except FileNotFoundError:
        # Another worker may have delivered and removed this exact durable
        # event between discovery and read.
        return True
    except (OSError, ValueError, urllib.error.URLError) as error:
        print("[event-outbox] delivery deferred for %s: %r" % (path.name, error), flush=True)
        return False
    terminal_rejection = response.status_code == 410 and payload.get("event") == "participant_joined"
    if not terminal_rejection and (response.status_code < 200 or response.status_code >= 300):
        print("[event-outbox] manager returned HTTP %d for %s; retrying" %
              (response.status_code, path.name), flush=True)
        return False
    try:
        path.unlink()
        _sync_directory(str(path.parent))
    except FileNotFoundError:
        return True
    except OSError as error:
        print("[event-outbox] accepted event could not be removed %s: %r" %
              (path.name, error), flush=True)
        return False
    # A terminally rejected join is durably consumed but deliberately not
    # admitted. notify.py must fail its dialplan System() call in that case.
    return not terminal_rejection


def flush_once(outbox=None, manager_url=None, token=None, session=HTTP):
    directory = Path(outbox or OUTBOX)
    try:
        files = sorted(directory.glob("*.json"))
    except OSError:
        return 0
    delivered = 0
    for path in files:
        if not deliver_file(path, manager_url, token, session):
            # A response may reach AMI before the local `submitted` event. Keep
            # it durable, but continue so the later mapping can be accepted;
            # the next pass can then correlate the early response.
            continue
        delivered += 1
    return delivered


def run_forever():
    delay = 1
    while True:
        delivered = flush_once()
        if delivered:
            delay = 1
            continue
        time.sleep(delay)
        delay = min(delay * 2, 30)


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--flush-once":
        raise SystemExit(0 if flush_once() >= 0 else 1)
    run_forever()
