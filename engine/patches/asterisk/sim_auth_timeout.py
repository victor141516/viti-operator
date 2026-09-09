#!/usr/bin/env python3
"""Allow SIM authentication through a local or remote Viti provider.

The sysmocom VoLTE registration code waits only three seconds for an AMI
AuthResponse.  That is too short once the request crosses the manager and an
ADB/Android provider: a valid response can arrive just after Asterisk has
discarded the pending challenge.  Fifteen seconds is still bounded while
covering USB bridges and ordinary remote-provider latency.
"""

from pathlib import Path


source = Path("res/res_pjsip_outbound_registration.c")
text = source.read_text(encoding="utf-8")
needle = "#define SIM_TIMEOUT 3"
replacement = "#define SIM_TIMEOUT 15"

if text.count(needle) != 1:
    raise SystemExit(f"expected exactly one {needle!r} in {source}")

source.write_text(text.replace(needle, replacement), encoding="utf-8")
print("extended AMI SIM authentication timeout to 15 seconds")
