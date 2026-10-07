"""Render an isolated Asterisk-only fixture; never run the carrier entrypoint."""
import os
import pathlib
import shutil
import socket

target = pathlib.Path("/tmp/native-voice-lab")
target.mkdir(exist_ok=True)
for source in pathlib.Path("/lab").glob("*.conf"):
    shutil.copyfile(source, target / source.name)
inner = socket.gethostbyname(socket.gethostname())
advertise = os.environ.get("NATIVE_LAB_ADVERTISE", "127.0.0.1")
socket.inet_aton(advertise)
(target / "rtp.conf").write_text(
    "[general]\nrtpstart=14600\nrtpend=14609\nicesupport=yes\n"
    f"[ice_host_candidates]\n{inner} => {advertise},include_local_address\n"
)
os.makedirs("/var/run/asterisk", exist_ok=True)
os.execv("/usr/sbin/asterisk", ["asterisk", "-f", "-C", str(target / "asterisk.conf")])
