#!/usr/bin/env python3
"""Run strongSwan behind the implementation-independent SWu contract.

The adapter owns one charon process, loads the connection through VICI and
translates VICI state into the engine's stable status, event and control files.
ESP is handled by strongSwan's kernel-libipsec
plugin on ``ipsec0``; this module never configures kernel XFRM state.
"""

from collections import OrderedDict
import ipaddress
import itertools
import json
import os
from pathlib import Path
import re
import selectors
import signal
import socket
import subprocess
import sys
import time
from urllib import request as urlrequest
import uuid

import event_outbox
from tunnel_contract import normalize_control_action, status_payload
from tunnel_reconcile import TunnelReconciler


BACKEND = "strongswan"
CONNECTION = "viti-swu"
CHILD = "ims"
BACKOFF_NOTIFY = "N((41041))"
AUTH_PROVIDER_FAILURE_MARKERS = (
    "Viti AKA broker request failed",
    "Viti AKA broker returned HTTP",
    "Viti AKA broker returned invalid",
)


class ViciCommandError(RuntimeError):
    def __init__(self, method, cause, records):
        super().__init__(f"VICI {method} failed: {cause}")
        self.method = method
        self.records = tuple(records)


def record_text(value):
    """Flatten VICI control records without depending on their exact schema."""
    if isinstance(value, dict):
        return " ".join(record_text(item) for item in value.values())
    if isinstance(value, (list, tuple)):
        return " ".join(record_text(item) for item in value)
    return text(value)


def classify_failure(error, env):
    """Translate carrier failures visible in VICI into the stable contract."""
    evidence = str(error)
    if isinstance(error, ViciCommandError):
        evidence += " " + " ".join(record_text(item) for item in error.records)
    if BACKOFF_NOTIFY in evidence:
        seconds = int(positive_float(
            env, "SWU_BACKOFF_41041_SECONDS", "600"
        ))
        return {
            "reason_code": "AUTHENTICATION_FAILED",
            "reason_policy": "backoff",
            "backoff": seconds,
        }
    if any(marker in evidence for marker in AUTH_PROVIDER_FAILURE_MARKERS):
        seconds = int(positive_float(
            env, "SWU_AUTH_PROVIDER_BACKOFF_SECONDS", "60"
        ))
        return {
            "reason_code": "AUTHENTICATION_PROVIDER_UNAVAILABLE",
            "reason_policy": "backoff",
            "backoff": seconds,
        }
    return {
        "reason_code": "strongswan_failure",
        "reason_policy": "transient",
    }


def auth_readiness_url(env):
    explicit = str(env.get("SWU_HTTP_AKA_READY_URL", "")).strip()
    if explicit:
        return explicit
    authenticate = str(env.get("SWU_HTTP_AKA_URL", "")).strip().rstrip("/")
    return authenticate + "/ready" if authenticate else ""


def parse_auth_readiness(encoded):
    payload = json.loads(encoded.decode("utf-8"))
    ready = payload.get("ready")
    if not isinstance(ready, bool):
        raise ValueError("AKA readiness response is missing a boolean ready field")
    return ready, str(payload.get("detail", "")).strip()


def required(env, name):
    value = str(env.get(name, "")).strip()
    if not value:
        raise ValueError(f"{name} is required")
    return value


def positive_float(env, name, default, allow_zero=False):
    value = float(str(env.get(name, default)).strip() or default)
    if value < 0 or (value == 0 and not allow_zero):
        raise ValueError(f"{name} must be {'non-negative' if allow_zero else 'positive'}")
    return value


def permanent_nai(env):
    imsi = required(env, "USIM_IMSI")
    mcc = required(env, "SWU_MCC").zfill(3)
    mnc = required(env, "SWU_MNC").zfill(3)
    if not (imsi.isdigit() and mcc.isdigit() and mnc.isdigit()):
        raise ValueError("IMSI, MCC and MNC must contain only digits")
    return f"0{imsi}@nai.epc.mnc{mnc}.mcc{mcc}.3gppnetwork.org"


def responder_identity(env):
    apn = str(env.get("SWU_APN", "ims")).strip() or "ims"
    if str(env.get("SWU_IDR_MODE", "apn")).strip().lower() == "fqdn":
        mcc = required(env, "SWU_MCC").zfill(3)
        mnc = required(env, "SWU_MNC").zfill(3)
        value = f"{apn}.apn.epc.mnc{mnc}.mcc{mcc}.pub.3gppnetwork.org"
    else:
        value = apn
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,253}", value):
        raise ValueError("SWu responder request identity contains invalid characters")
    return value


def duration(seconds):
    """Return a whole-second duration accepted by strongSwan's VICI parser."""
    if seconds == 0:
        return "0s"
    return f"{max(1, round(seconds))}s"


def initiate_timeout_ms(env):
    """Keep VICI alive longer than the synchronous AKA broker request."""
    broker = positive_float(env, "SWU_HTTP_AKA_TIMEOUT", "40")
    configured = str(env.get("SWU_IKE_AUTH_TIMEOUT", "")).strip()
    seconds = (
        positive_float(env, "SWU_IKE_AUTH_TIMEOUT", configured)
        if configured else max(45.0, broker + 10.0)
    )
    return str(max(1, round(seconds * 1000)))


def initiate_socket_timeout(env):
    """Allow VICI to outlive charon's configured synchronous initiation wait."""
    return int(initiate_timeout_ms(env)) / 1000.0 + 5.0


def build_connection(env):
    """Build the VICI load-conn payload from the stable backend inputs."""
    identity = permanent_nai(env)
    child_rekey = positive_float(
        env, "SWU_CHILD_REKEY_MINUTES", "30", allow_zero=True
    ) * 60
    ike_rekey = positive_float(
        env, "SWU_IKE_REKEY_MINUTES", "45", allow_zero=True
    ) * 60
    dpd_delay = positive_float(env, "SWU_LIVENESS_PERIOD", "20", allow_zero=True)

    connection = OrderedDict([
        ("version", "2"),
        ("local_addrs", [required(env, "SWU_SOURCE")]),
        ("remote_addrs", [required(env, "SWU_EPDG")]),
        ("send_certreq", "no"),
        ("encap", "yes"),
        ("fragmentation", "yes"),
        ("mobike", "no"),
        ("unique", "replace"),
        ("keyingtries", "1"),
        ("rekey_time", duration(ike_rekey)),
        ("reauth_time", "0s"),
        ("rand_time", "0s"),
        ("over_time", "30s"),
        ("dpd_delay", duration(dpd_delay)),
        ("proposals", [
            "aes256-sha256-modp2048",
            "aes128-sha256-modp2048",
            "aes256-sha1-modp2048",
            "aes128-sha1-modp2048",
        ]),
        ("vips", ["::"]),
        ("local", OrderedDict([
            ("auth", "eap-aka"),
            ("id", identity),
            ("eap_id", identity),
        ])),
        ("remote", OrderedDict([
            ("auth", "eap"),
            # The IDr request transports the APN per 3GPP TS 33.402, but some
            # ePDGs authenticate with a different responder identity (Digi
            # uses 0.0.0.0).  request_idr below decouples the wire request from
            # this authentication constraint.
            ("id", "%any"),
        ])),
        ("children", OrderedDict([
            (CHILD, OrderedDict([
                ("mode", "tunnel"),
                ("local_ts", ["dynamic"]),
                ("remote_ts", ["::/0"]),
                ("rekey_time", duration(child_rekey)),
                ("life_time", duration(child_rekey * 4 / 3) if child_rekey else "0s"),
                ("rand_time", "0s"),
                ("esp_proposals", [
                    "aes128-sha1",
                    "aes256-sha256",
                    "aes128-sha256",
                    "aes256-sha1",
                    "aes128-sha1-modp2048",
                    "aes256-sha256-modp2048",
                    "aes128-sha256-modp2048",
                    "aes256-sha1-modp2048",
                ]),
            ])),
        ])),
    ])
    return OrderedDict([(CONNECTION, connection)])


def text(value):
    if isinstance(value, bytes):
        return value.decode("utf-8", "replace")
    return str(value)


def established_sa(messages):
    """Return normalized details for an installed CHILD_SA, or ``None``."""
    for message in messages:
        for name, ike in message.items():
            if text(ike.get("state", "")) != "ESTABLISHED":
                continue
            children = ike.get("child-sas", {})
            installed = [
                child for child in children.values()
                if text(child.get("state", "")) == "INSTALLED"
            ]
            if not installed:
                continue
            vips = [text(value).split("/", 1)[0] for value in ike.get("local-vips", [])]
            if not vips:
                continue
            return {
                "name": name,
                "uniqueid": text(ike.get("uniqueid", "")),
                "inner_ip": vips[0],
                "initiator_spi": text(ike.get("initiator-spi", "")),
                "responder_spi": text(ike.get("responder-spi", "")),
            }
    return None


def sa_loss_expired(missing_since, now, grace):
    """Return true only after a continuous post-establishment absence."""
    return missing_since is not None and now - missing_since >= grace


def isolate_tunnel_route(inner_ip, runner=subprocess.run):
    """Restrict strongSwan's table-220 default route to IMS-source traffic.

    kernel-libipsec needs the route to feed packets into its TUN device, but its
    default ``from all`` rule would also capture unrelated IPv6 traffic in the
    shared Asterisk namespace. IMS sockets bind the assigned virtual address,
    so a source-specific rule preserves the tunnel without hijacking the host.
    """
    address = ipaddress.ip_address(inner_ip)
    if address.version != 6:
        raise ValueError("the StrongSwan SWu profile requires an IPv6 inner address")
    runner(["ip", "-6", "rule", "del", "priority", "220"], check=False)
    runner([
        "ip", "-6", "rule", "add", "priority", "220",
        "from", f"{address}/128", "lookup", "220",
    ], check=True)


class RuntimeContract:
    def __init__(self, env):
        self.rundir = Path(env.get("VOWIFI_RUNDIR", "/run/vowifi"))
        self.outbox = Path(env.get(
            "VITI_EVENT_OUTBOX_DIR", self.rundir / "event-outbox"
        ))
        self.generation = uuid.uuid4().hex
        self.sequence = itertools.count(1)
        self.started = False
        self.up = False
        self.down = False
        self.last_connected = None
        self.last_status_write = 0.0
        self.status_heartbeat = positive_float(
            env, "SWU_STATUS_HEARTBEAT_SECONDS", "30", allow_zero=True
        )
        self.rundir.mkdir(parents=True, exist_ok=True)

    def write_status(self, state, **extra):
        payload = status_payload(BACKEND, state, int(time.time()), **extra)
        temporary = self.rundir / "swu_status.json.tmp"
        final = self.rundir / "swu_status.json"
        encoded = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            output.write(encoded)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, final)

    def event(self, name, arg=None):
        event_outbox.enqueue_event(
            name,
            [arg] if arg else [],
            outbox=self.outbox,
            recorded_at_ns=time.time_ns(),
            extra={
                "tunnel_generation": self.generation,
                "tunnel_sequence": next(self.sequence),
            },
        )

    def start(self):
        self.write_status("CONNECTING")
        self.event("tunnel_start")
        self.started = True

    def connected(self, inner_ip, pcscf, iface):
        observation = (inner_ip, pcscf, iface)
        now = time.monotonic()
        if (
            observation == self.last_connected
            and self.status_heartbeat
            and now - self.last_status_write < self.status_heartbeat
        ):
            return
        self.write_status(
            "CONNECTED", inner_ip=inner_ip, pcscf=pcscf, iface=iface
        )
        self.last_connected = observation
        self.last_status_write = now
        if not self.up:
            self.event("tunnel_up", pcscf or None)
            self.up = True

    def disconnected(self, reason_code="backend_stopped", reason_policy="transient", **extra):
        if self.down:
            return
        self.write_status(
            "DOWN", reason_code=reason_code, reason_policy=reason_policy, **extra
        )
        if self.started:
            self.event("tunnel_down", reason_code)
        self.down = True
        self.up = False


class StrongSwanAdapter:
    def __init__(self, env=None):
        self.env = dict(os.environ if env is None else env)
        self.runtime = RuntimeContract(self.env)
        self.iface = str(self.env.get("SWU_IFACE", "ipsec0")).strip() or "ipsec0"
        if self.iface != "ipsec0":
            raise ValueError("kernel-libipsec exposes the required interface as ipsec0")
        self.mtu = int(str(self.env.get("SWU_TUN_MTU", "1400")) or "1400")
        if not 1280 <= self.mtu <= 65535:
            raise ValueError("SWU_TUN_MTU must be between 1280 and 65535")
        self.vici_socket = Path(self.env.get("SWU_VICI_SOCKET", "/var/run/charon.vici"))
        self.charon_command = self.env.get(
            "SWU_CHARON", "/usr/libexec/ipsec/charon"
        ).split()
        self.charon = None
        self.control = None
        self.active_session = None
        self.stop_requested = False
        self.signal_received = None
        self.was_established = False
        self.auth_waiting = False
        self.routed_inner_ip = ""
        self.reconciler = TunnelReconciler(
            self.runtime.rundir,
            self.env.get("SWU_RENDER", "/usr/local/bin/render.py"),
            self.log,
        )

    def log(self, message):
        print(f"[strongswan-adapter] {message}", flush=True)

    def session(self, timeout=10.0):
        import vici

        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(timeout)
        sock.connect(str(self.vici_socket))
        return vici.Session(sock)

    def fetch_auth_readiness(self):
        url = auth_readiness_url(self.env)
        if not url:
            return True, ""
        headers = {"Accept": "application/json"}
        token = str(self.env.get("SWU_HTTP_AKA_TOKEN", "")).strip()
        if token:
            headers["Authorization"] = "Bearer " + token
        timeout = positive_float(
            self.env, "SWU_HTTP_AKA_READY_TIMEOUT", "5"
        )
        with urlrequest.urlopen(
            urlrequest.Request(url, headers=headers), timeout=timeout
        ) as response:
            if response.status != 200:
                raise RuntimeError(f"AKA readiness returned HTTP {response.status}")
            return parse_auth_readiness(response.read(64 * 1024))

    def wait_auth_ready(self):
        interval = positive_float(
            self.env, "SWU_HTTP_AKA_READY_INTERVAL", "2"
        )
        last_detail = None
        while not self.stop_requested:
            try:
                ready, detail = self.fetch_auth_readiness()
            except (OSError, ValueError, RuntimeError, json.JSONDecodeError) as error:
                ready, detail = False, f"readiness check failed: {error}"
            if ready:
                if self.auth_waiting:
                    self.log("SIM authentication provider is ready; starting IKE")
                return True
            self.auth_waiting = True
            if detail != last_detail:
                self.log(
                    "waiting for SIM authentication provider: "
                    + (detail or "not ready")
                )
                last_detail = detail
            deadline = time.monotonic() + interval
            while not self.stop_requested and time.monotonic() < deadline:
                time.sleep(min(0.1, deadline - time.monotonic()))
        return False

    @staticmethod
    def close_session(session):
        try:
            session.transport.close()
        except OSError:
            pass

    def request(self, method, payload=None, socket_timeout=10.0):
        session = self.session(socket_timeout)
        self.active_session = session
        try:
            result = getattr(session, method)(payload) if payload is not None else getattr(session, method)()
            if hasattr(result, "__iter__") and not isinstance(
                result, (dict, str, bytes)
            ):
                records = []
                try:
                    for record in result:
                        records.append(record)
                except Exception as error:
                    raise ViciCommandError(method, error, records) from error
                return records
            return result
        finally:
            if self.active_session is session:
                self.active_session = None
            self.close_session(session)

    def wait_for_vici(self):
        deadline = time.monotonic() + positive_float(
            self.env, "SWU_CHARON_START_TIMEOUT", "15"
        )
        while time.monotonic() < deadline:
            if self.charon.poll() is not None:
                raise RuntimeError(f"charon exited during startup ({self.charon.returncode})")
            if self.vici_socket.exists():
                try:
                    version = self.request("version")
                    if text(version.get("daemon", "")) == "charon":
                        return
                except (OSError, RuntimeError):
                    pass
            time.sleep(0.1)
        raise TimeoutError("charon VICI socket did not become ready")

    def start_charon(self):
        base_path = Path(self.env.get(
            "SWU_STRONGSWAN_BASE_CONFIG", "/etc/strongswan.conf"
        ))
        base = base_path.read_text(encoding="utf-8")
        marker = "charon {"
        if marker not in base:
            raise ValueError(f"{base_path} does not contain a charon section")
        keep_alive = positive_float(
            self.env, "SWU_NATT_KEEPALIVE", "20", allow_zero=True
        )
        generated = base.replace(
            marker,
            f"{marker}\n  keep_alive = {duration(keep_alive)}"
            f"\n  request_idr = {responder_identity(self.env)}",
            1,
        )
        config_path = self.runtime.rundir / "strongswan.conf"
        temporary = config_path.with_suffix(".conf.tmp")
        temporary.write_text(generated, encoding="utf-8")
        os.replace(temporary, config_path)
        self.env["STRONGSWAN_CONF"] = str(config_path)
        try:
            self.vici_socket.unlink()
        except FileNotFoundError:
            pass
        self.charon = subprocess.Popen(self.charon_command, env=self.env)
        self.wait_for_vici()
        self.log("charon is ready through VICI")

    def load_and_initiate(self):
        self.request("load_conn", build_connection(self.env))
        logs = self.request(
            "initiate",
            {"child": CHILD, "timeout": initiate_timeout_ms(self.env)},
            socket_timeout=initiate_socket_timeout(self.env),
        )
        if logs:
            self.log(f"VICI initiation completed with {len(logs)} control records")

    def current_sa(self):
        return established_sa(self.request("list_sas", {"ike": CONNECTION}))

    def read_pcscf(self):
        try:
            return (self.runtime.rundir / "pcscf").read_text(encoding="ascii").strip()
        except OSError:
            return ""

    def wait_connected(self):
        deadline = time.monotonic() + positive_float(
            self.env, "SWU_CONNECT_TIMEOUT", "45"
        )
        require_pcscf = str(self.env.get("SWU_REQUIRE_PCSCF", "1")).lower() not in {
            "0", "no", "false", "off",
        }
        last_sa = None
        while time.monotonic() < deadline:
            if self.charon.poll() is not None:
                raise RuntimeError(f"charon exited while connecting ({self.charon.returncode})")
            last_sa = self.current_sa()
            pcscf = self.read_pcscf()
            if last_sa and (pcscf or not require_pcscf):
                subprocess.run(
                    ["ip", "link", "set", "dev", self.iface, "mtu", str(self.mtu)],
                    check=True,
                )
                isolate_tunnel_route(last_sa["inner_ip"])
                self.routed_inner_ip = last_sa["inner_ip"]
                self.runtime.connected(last_sa["inner_ip"], pcscf, self.iface)
                self.reconciler.apply(last_sa["inner_ip"], pcscf)
                self.was_established = True
                self.log(
                    f"tunnel connected inner={last_sa['inner_ip']} "
                    f"pcscf={pcscf or 'not-requested'} iface={self.iface}"
                )
                return
            time.sleep(0.2)
        raise TimeoutError(f"SWu tunnel did not become usable; last_sa={last_sa!r}")

    def open_control(self):
        path = self.runtime.rundir / "swu.ctl"
        try:
            path.unlink()
        except FileNotFoundError:
            pass
        os.mkfifo(path, 0o600)
        descriptor = os.open(path, os.O_RDWR | os.O_NONBLOCK)
        self.control = os.fdopen(descriptor, "r", encoding="ascii", buffering=1)

    def handle_control(self, line):
        try:
            action = normalize_control_action(line)
        except ValueError as error:
            self.log(f"ignored control command: {error}")
            return
        if action == "stop":
            self.stop_requested = True
            return
        payload = {"ike": CONNECTION}
        if action == "reauth":
            payload["reauth"] = "yes"
        elif action == "rekey-child":
            payload = {"child": CHILD}
        timeout = initiate_socket_timeout(self.env) if action == "reauth" else 10.0
        self.request("rekey", payload, socket_timeout=timeout)
        self.log(f"VICI control completed: {action}")

    def monitor(self):
        selector = selectors.DefaultSelector()
        selector.register(self.control, selectors.EVENT_READ)
        loss_grace = positive_float(
            self.env, "SWU_SA_LOSS_GRACE", "5", allow_zero=True
        )
        missing_since = None
        try:
            while not self.stop_requested:
                if self.charon.poll() is not None:
                    raise RuntimeError(f"charon exited unexpectedly ({self.charon.returncode})")
                for key, _ in selector.select(timeout=0.25):
                    while True:
                        line = key.fileobj.readline()
                        if not line:
                            break
                        self.handle_control(line)
                sa = self.current_sa()
                if not sa:
                    if self.was_established:
                        now = time.monotonic()
                        if missing_since is None:
                            missing_since = now
                            self.log(
                                "IKE/CHILD SA temporarily absent; allowing "
                                f"{loss_grace:g}s for rekey convergence"
                            )
                        elif sa_loss_expired(missing_since, now, loss_grace):
                            raise RuntimeError("established IKE/CHILD SA disappeared")
                    continue
                if missing_since is not None:
                    self.log("IKE/CHILD SA remained available after transient change")
                    missing_since = None
                self.was_established = True
                pcscf = self.read_pcscf()
                if sa["inner_ip"] != self.routed_inner_ip:
                    isolate_tunnel_route(sa["inner_ip"])
                    self.routed_inner_ip = sa["inner_ip"]
                self.runtime.connected(sa["inner_ip"], pcscf, self.iface)
                self.reconciler.apply(sa["inner_ip"], pcscf)
        finally:
            selector.close()

    def terminate(self, budget=8.0):
        deadline = time.monotonic() + budget
        if self.charon and self.charon.poll() is None:
            try:
                self.request("terminate", {
                    "ike": CONNECTION, "timeout": "1500", "force": "yes",
                }, socket_timeout=min(2.0, max(0.1, deadline - time.monotonic())))
            except Exception as error:
                self.log(f"VICI termination failed: {error}")
            self.charon.send_signal(signal.SIGTERM)
            try:
                self.charon.wait(timeout=max(0.1, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                self.charon.kill()
                self.charon.wait(timeout=1)

    def run(self):
        reason = "backend_stopped"
        failure = {
            "reason_code": reason,
            "reason_policy": "transient",
        }
        return_code = 0
        previous_handlers = {}

        def request_stop(signum, _frame):
            self.signal_received = signum
            self.stop_requested = True
            # A VICI stream may be waiting for a slow AKA provider. Closing the
            # active socket makes SIGTERM interrupt that wait immediately so the
            # adapter can persist tunnel_down within Docker's shutdown budget.
            if self.active_session is not None:
                self.close_session(self.active_session)

        for signum in (signal.SIGINT, signal.SIGTERM):
            previous_handlers[signum] = signal.signal(signum, request_stop)
        self.runtime.start()
        for path in (self.runtime.rundir / "pcscf", self.runtime.rundir / "swu.ctl"):
            try:
                path.unlink()
            except FileNotFoundError:
                pass
        try:
            if self.wait_auth_ready():
                self.start_charon()
                self.load_and_initiate()
                self.wait_connected()
                self.open_control()
                self.monitor()
            if self.signal_received is not None:
                reason = "signal"
            failure = {"reason_code": reason, "reason_policy": "transient"}
        except KeyboardInterrupt:
            reason = "signal"
            failure = {"reason_code": reason, "reason_policy": "transient"}
        except Exception as error:
            failure = classify_failure(error, self.env)
            reason = failure["reason_code"]
            return_code = 1
            self.log(f"backend failed: {error}")
        finally:
            # Persist the causal boundary before any best-effort peer teardown.
            # This makes tunnel_down durable even if charon ignores SIGTERM.
            self.runtime.disconnected(**failure)
            self.terminate()
            if self.control:
                self.control.close()
            for signum, handler in previous_handlers.items():
                signal.signal(signum, handler)
        return return_code


def main():
    try:
        return StrongSwanAdapter().run()
    except Exception as error:
        print(f"[strongswan-adapter] configuration failure: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
