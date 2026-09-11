#!/usr/bin/env python3
"""Block an unanswered Asterisk carrier leg until Viti chooses its first route.

Every local failure returns ``ring_clients``. Screening is optional automation;
losing the manager must never turn a real incoming call into a dropped call.
"""

import json
import os
import sys
import urllib.request

from notify import load_env


VALID_ACTIONS = {"ring_clients", "take_control", "hangup"}


def post_json(url, payload, headers):
    request = urllib.request.Request(
        url,
        data=json.dumps(payload, separators=(",", ":")).encode("utf-8"),
        headers={"Content-Type": "application/json", **headers},
        method="POST",
    )
    # Deliberately omit urlopen's timeout. The services own their advertised
    # deadlines, including an unlimited processing phase after carrier answer.
    with urllib.request.urlopen(request) as response:
        return response.status, json.load(response)


def screening_action(call_id, post=post_json):
    env = load_env()
    manager_url = (os.environ.get("MANAGER_URL") or env.get("MANAGER_URL", "")).rstrip("/")
    manager_token = os.environ.get("VOWIFI_MANAGER_TOKEN") or env.get("VOWIFI_MANAGER_TOKEN", "")
    instance = os.environ.get("VOWIFI_ID") or env.get("VOWIFI_ID", "1")
    if not manager_url or not call_id:
        return "ring_clients"
    headers = {"X-Viti-Engine-Token": manager_token} if manager_token else {}
    try:
        status, response = post(
            manager_url + "/api/engine/screen",
            {"instance": instance, "call_id": call_id},
            headers,
        )
        if status != 200:
            return "ring_clients"
        action = str(response.get("action", "")).strip().lower()
        return action if action in VALID_ACTIONS else "ring_clients"
    except Exception:
        return "ring_clients"


def main():
    call_id = sys.argv[1].strip() if len(sys.argv) > 1 else ""
    print(screening_action(call_id))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
