#!/usr/bin/env python3
"""Select and execute the configured SWu tunnel backend.

The entrypoint invokes this stable launcher instead of knowing the legacy
client's CLI.  The strongSwan adapter will implement the same external runtime
contract once its black-box acceptance suite exists.
"""

import os
import sys


class BackendConfigurationError(ValueError):
    pass


def _required(env, name):
    value = str(env.get(name, "")).strip()
    if not value:
        raise BackendConfigurationError("%s is required" % name)
    return value


def python_backend_command(env=None):
    env = os.environ if env is None else env
    command = [
        str(env.get("SWU_PYTHON", "python3")),
        str(env.get("SWU_PYTHON_BACKEND", "/usr/local/bin/swu_ike.py")),
        "-m", str(env.get("USIM_READER_INDEX", "0")),
        "-s", _required(env, "SWU_SOURCE"),
        "-d", _required(env, "SWU_EPDG"),
        "-a", str(env.get("SWU_APN", "ims")),
        "-I", _required(env, "USIM_IMSI"),
        "-M", _required(env, "SWU_MCC"),
        "-N", _required(env, "SWU_MNC"),
        "-E", str(env.get("SWU_IMEI", "")),
        "-V", str(env.get("SWU_IMEISV", "")),
    ]
    return command


def backend_command(env=None):
    env = os.environ if env is None else env
    backend = str(env.get("SWU_BACKEND", "python")).strip().lower()
    if backend == "python":
        return backend, python_backend_command(env)
    if backend == "strongswan":
        adapter = str(env.get(
            "SWU_STRONGSWAN_BACKEND", "/usr/local/bin/strongswan_adapter.py"
        ))
        if not os.path.isfile(adapter):
            raise BackendConfigurationError(
                "strongSwan backend is selected but its adapter is not installed"
            )
        return backend, [str(env.get("SWU_PYTHON", "python3")), adapter]
    raise BackendConfigurationError("unsupported SWU_BACKEND %r" % backend)


def main():
    try:
        backend, command = backend_command()
    except BackendConfigurationError as error:
        print("[tunnel-backend] configuration error: %s" % error, file=sys.stderr, flush=True)
        return 2
    print("[tunnel-backend] starting %s backend" % backend, flush=True)
    os.execvpe(command[0], command, os.environ)


if __name__ == "__main__":
    raise SystemExit(main())
