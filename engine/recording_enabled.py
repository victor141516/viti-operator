#!/usr/bin/env python3
"""Print whether product call recording is enabled on the manager."""

import os

import requests

from notify import load_env


def main():
    env = load_env()
    manager_url = os.environ.get("MANAGER_URL") or env.get("MANAGER_URL", "")
    manager_token = os.environ.get("VOWIFI_MANAGER_TOKEN") or env.get("VOWIFI_MANAGER_TOKEN", "")
    if not manager_url:
        print("0", end="")
        return
    try:
        response = requests.get(
            f"{manager_url.rstrip('/')}/api/engine/recording",
            headers={"X-Viti-Engine-Token": manager_token},
            timeout=3,
        )
        response.raise_for_status()
        print("1" if response.json().get("enabled") else "0", end="")
    except Exception:
        print("0", end="")


if __name__ == "__main__":
    main()
