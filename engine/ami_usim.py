#!/usr/bin/env python3
"""
ami_usim.py - Bridge Asterisk (ims_aka) SIP authentication to the physical USIM via PC/SC.

Derived from phcoder/asterisk-docker (jolly). Changes:
  - VERIFY CHV1 (PIN) after selecting ADF.USIM, before AUTHENTICATE (reference didn't).
  - Clean type-annotation bug from the original (undefined Hexstr/Optional names).
  - Emit status/heartbeat JSON to $VOWIFI_RUNDIR/usim_status.json for the manager FSM.

On Asterisk 'AuthRequest' it runs USIM AUTHENTICATE and returns RES/CK/IK (or AUTS on
sync failure). Triggers registration on FullyBooted and confirms dedicated bearers.
"""
import asyncio
import base64
import binascii
import configparser
import fcntl
import json
import os
import re
import subprocess
import sys
import tempfile
import time

from panoramisk import Manager
import requests
import event_outbox
try:
    from smartcard.System import readers
    from smartcard.util import toHexString, toBytes
    from smartcard.scard import SCardBeginTransaction, SCardEndTransaction, SCARD_LEAVE_CARD
except ImportError:
    readers = None
    toHexString = toBytes = None
    SCardBeginTransaction = SCardEndTransaction = None
    SCARD_LEAVE_CARD = None

RUNDIR = os.environ.get("VOWIFI_RUNDIR", "/run/vowifi")
USIM_PIN = os.environ.get("USIM_PIN", "")
ANDROID_QMI = os.environ.get("SWU_ANDROID_QMI", "").strip() == "1"
HTTP_AKA_URL = os.environ.get("SWU_HTTP_AKA_URL", "").strip()


def forward_sms_protocol_event(message):
    """Persist the patched Asterisk event without interpreting protocol evidence."""
    fields = [
        "Phase", "MessageID", "Destination", "Part", "Parts",
        "RPReference", "TPReference", "Status", "Cause", "ServiceCentreTime",
    ]
    values = ["" if getattr(message, field, None) is None else str(getattr(message, field)) for field in fields]
    if values[0] not in ("submitted", "local_failure", "rp_ack", "rp_error", "status_report"):
        print("Ignoring invalid VitiSMS event phase=%r" % values[0])
        return
    try:
        event_outbox.enqueue_event("sms_protocol", values)
    except Exception as error:
        # The event must not be silently downgraded to a best-effort POST. A
        # storage failure is visible in the engine log and leaves Asterisk's
        # protocol trace available for recovery.
        print("CRITICAL: could not persist VitiSMS event: %r" % error, flush=True)


def forward_dtmf_event(message):
    """Forward one source-side completed RFC 4733 tone to the manager."""
    if str(getattr(message, "Direction", "")).lower() != "received":
        return
    call_id = str(getattr(message, "Linkedid", "") or "").strip()
    channel = str(getattr(message, "Channel", "") or "").strip()
    unique_id = str(getattr(message, "Uniqueid", "") or "").strip()
    digit = str(getattr(message, "Digit", "") or "").strip().upper()
    duration = str(getattr(message, "DurationMs", "0") or "0").strip()
    if not call_id or not channel or not unique_id or digit not in "0123456789*#ABCD":
        print("Ignoring invalid DTMFEnd event", flush=True)
        return
    try:
        event_outbox.enqueue_event(
            "dtmf", [channel, unique_id, digit, duration], extra={"call_id": call_id}
        )
    except Exception as error:
        print("CRITICAL: could not persist DTMF event: %r" % error, flush=True)


# --- Reader binding by physical USB port -----------------------------------------------------
# Resolve a reader by its STABLE physical USB port path (USIM_READER_PORT, e.g. "3-2") so the SIP
# IMS-AKA path addresses the same physical reader as pin_keeper — even when pcscd flips
# two identical (serial-less) readers' enumeration order. Mirrors control/app/usbreader.py.
try:
    from smartcard.scard import (
        SCardEstablishContext as _SCEstablish, SCardConnect as _SCConnect,
        SCardGetAttrib as _SCGetAttrib, SCardDisconnect as _SCDisconnect,
        SCARD_SCOPE_USER as _SC_SCOPE_USER, SCARD_SHARE_DIRECT as _SC_SHARE_DIRECT,
        SCARD_LEAVE_CARD as _SC_LEAVE, SCARD_PROTOCOL_T0 as _SC_T0,
        SCARD_PROTOCOL_T1 as _SC_T1, SCARD_ATTR_CHANNEL_ID as _SC_CHANNEL_ID,
        SCARD_S_SUCCESS as _SC_OK,
    )
    _SC_PORT_OK = True
except Exception:                        # pragma: no cover
    _SC_PORT_OK = False


def _reader_bus_dev(reader_name):
    if not _SC_PORT_OK:
        return None
    hcard = None
    try:
        hr, hctx = _SCEstablish(_SC_SCOPE_USER)
        if hr != _SC_OK:
            return None
        hr, hcard, _p = _SCConnect(hctx, reader_name, _SC_SHARE_DIRECT, _SC_T0 | _SC_T1)
        if hr != _SC_OK:
            return None
        hr, val = _SCGetAttrib(hcard, _SC_CHANNEL_ID)
        if hr != _SC_OK or not val or len(val) < 4:
            return None
        v = val[0] | (val[1] << 8) | (val[2] << 16) | (val[3] << 24)
        if (v >> 16) != 0x0020:
            return None
        return (v >> 8) & 0xff, v & 0xff
    except Exception:
        return None
    finally:
        if hcard is not None:
            try:
                _SCDisconnect(hcard, _SC_LEAVE)
            except Exception:
                pass


def _usb_port_path(bus, devnum):
    import glob as _glob
    try:
        entries = _glob.glob("/sys/bus/usb/devices/*/")
    except Exception:
        return None
    for d in entries:
        try:
            with open(d + "busnum") as f:
                b = int(f.read())
            with open(d + "devnum") as f:
                n = int(f.read())
        except Exception:
            continue
        if b == bus and n == devnum:
            return os.path.basename(d.rstrip("/"))
    return None


def index_for_port(port):
    if not port:
        return None
    try:
        rlist = readers()
    except Exception:
        return None
    for i, r in enumerate(rlist):
        bd = _reader_bus_dev(str(r))
        if bd and _usb_port_path(bd[0], bd[1]) == port:
            return i
    return None


def _hcard(conn):
    obj = conn
    for _ in range(5):
        if hasattr(obj, "hcard"):
            return obj.hcard
        if hasattr(obj, "component") and obj.component is not None:
            obj = obj.component
            continue
        break
    return None


class _Tx:
    """Best-effort PC/SC transaction: exclusive card access for the auth sequence, so it
    cannot interleave with pin_keeper APDUs on the shared card."""
    def __init__(self, conn):
        self.conn = conn
        self.hcard = None

    def __enter__(self):
        self.hcard = _hcard(self.conn)
        if self.hcard is not None:
            try:
                SCardBeginTransaction(self.hcard)
            except Exception:
                self.hcard = None
        return self.conn

    def __exit__(self, *a):
        if self.hcard is not None:
            try:
                SCardEndTransaction(self.hcard, SCARD_LEAVE_CARD)
            except Exception:
                pass


def write_status(**kw):
    os.makedirs(RUNDIR, exist_ok=True)
    kw["ts"] = int(time.time())
    tmp = os.path.join(RUNDIR, "usim_status.json.tmp")
    with open(tmp, "w") as f:
        json.dump(kw, f)
    os.replace(tmp, os.path.join(RUNDIR, "usim_status.json"))


def swap_nibbles(s):
    return "".join([x + y for x, y in zip(s[1::2], s[0::2])])


def dec_imsi(ef):
    if len(ef) < 4:
        return None
    l = int(ef[0:2], 16) * 2 - 1
    swapped = swap_nibbles(ef[2:]).rstrip("f")
    if len(swapped) < 1:
        return None
    return swapped[1:]


# 3GPP USIM AID prefix. EF_DIR record 1 is NOT always the USIM (China Telecom cards
# list CSIM first), so scan the records and pick the USIM by AID.
USIM_AID_PREFIX = "A0000000871002"


def _usim_aid_from_dir(connection):
    """Scan EF_DIR records for the USIM AID; prefer 3GPP USIM, fall back to the first
    application. EF.DIR must be selectable from the current DF. Returns (len, hex) or None."""
    data, sw1, sw2 = connection.transmit(toBytes("00a40004022f0000"))  # SELECT EF.DIR
    if sw1 != 0x61:
        return None
    fcp, sw1, sw2 = connection.transmit(toBytes("00C00000") + [sw2])
    if sw1 != 0x90 or len(fcp) < 8:
        return None
    record_length = fcp[7]
    first = None
    for rec in range(1, 11):
        data, sw1, sw2 = connection.transmit(toBytes("00b2") + [rec, 0x04, record_length])
        if sw1 != 0x90 or len(data) < 5 or data[0] != 0x61 or data[2] != 0x4F:
            break
        aid_length = data[3]
        aid = "".join("%02X" % b for b in data[4:4 + aid_length])
        if len(aid) < aid_length * 2:
            break
        if aid.startswith(USIM_AID_PREFIX):
            return aid_length, aid
        if first is None:
            first = (aid_length, aid)
    return first


def make_connection_index(reader_index):
    r = readers()
    if reader_index >= len(r):
        return None
    connection = r[reader_index].createConnection()
    connection.connect()
    connection.transmit(toBytes("00a40004023f0000"))
    got = _usim_aid_from_dir(connection)
    if got is None:
        print("Failed to find USIM AID in EF.DIR")
        return None
    aid_length, aid = got
    print(f"Using aid={aid}")
    data, sw1, sw2 = connection.transmit(toBytes("00a40404") + [aid_length] + toBytes(aid))
    if sw1 != 0x61:
        print("Failed to select AID")
        return None
    return connection


def make_connection_name(reader_name):
    if isinstance(reader_name, str) and reader_name.startswith("imsi:"):
        target_imsi = reader_name[5:]
        for idx in range(len(readers())):
            connection = make_connection_index(idx)
            if connection is None:
                continue
            data, sw1, sw2 = connection.transmit(toBytes("00a40004026f0700"))
            if sw1 != 0x61:
                continue
            data, sw1, sw2 = connection.transmit(toBytes("00b0000009"))
            if (sw1, sw2) != (0x90, 0x00):
                continue
            imsi = dec_imsi(bytes(data).hex())
            print(f"Found IMSI {imsi}")
            if imsi == target_imsi:
                print(f"Found target imsi on reader {idx}")
                # re-select ADF.USIM after reading IMSI (IMSI read left EF selected)
                make_reselect_adf(connection)
                return connection
        print(f"IMSI {target_imsi} not found")
        return None
    return make_connection_index(int(reader_name))


def make_reselect_adf(connection):
    connection.transmit(toBytes("00a40004023f0000"))
    got = _usim_aid_from_dir(connection)
    if got is None:
        return
    aid_length, aid = got
    connection.transmit(toBytes("00a40404") + [aid_length] + toBytes(aid))


def select_adf_usim(connection):
    """SELECT MF -> EF.DIR -> USIM AID -> ADF.USIM. Returns True on success."""
    connection.transmit(toBytes("00a40004023f0000"))
    got = _usim_aid_from_dir(connection)
    if got is None:
        return False
    aid_length, aid = got
    data, sw1, sw2 = connection.transmit(toBytes("00a40404") + [aid_length] + toBytes(aid))
    return sw1 == 0x61


def open_usim(reader_spec):
    """Return an open connection positioned at ADF.USIM. For a single reader we use it
    directly (IMSI can't be read before PIN). For imsi:<IMSI> with multiple readers we
    verify PIN then match IMSI. Selection/verify happen under a transaction by the caller."""
    rlist = readers()
    if not rlist:
        return None
    # Highest priority: the stable USB-port binding. Open that physical reader directly so we
    # don't probe/burn PIN tries on the wrong card when indices flipped.
    port = os.environ.get("USIM_READER_PORT", "").strip()
    if port and len(rlist) > 1:
        pidx = index_for_port(port)
        if pidx is not None and pidx < len(rlist):
            try:
                conn = rlist[pidx].createConnection()
                conn.connect()
                return conn
            except Exception:
                pass
    if isinstance(reader_spec, str) and reader_spec.startswith("imsi:") and len(rlist) > 1:
        target = reader_spec[5:]
        for r in rlist:
            try:
                conn = r.createConnection()
                conn.connect()
            except Exception:
                continue
            with _Tx(conn):
                if select_adf_usim(conn) and verify_pin(conn):
                    conn.transmit(toBytes("00a40004026f0700"))
                    d, s1, s2 = conn.transmit(toBytes("00b0000009"))
                    if s1 == 0x90 and dec_imsi(bytes(d).hex()) == target:
                        return conn
            try:
                conn.disconnect()
            except Exception:
                pass
        return None
    # single reader, or explicit index
    idx = 0
    if isinstance(reader_spec, str) and reader_spec.isdigit():
        idx = int(reader_spec)
    if idx >= len(rlist):
        idx = 0
    conn = rlist[idx].createConnection()
    conn.connect()
    return conn


def verify_pin(connection):
    """Verify CHV1 if a PIN is configured. Idempotent: skips if already verified (9000)."""
    if not USIM_PIN or USIM_PIN.lower() in ("none", "disabled", ""):
        return True
    d, s1, s2 = connection.transmit(toBytes("0020000100"))
    if (s1, s2) == (0x90, 0x00):
        return True  # already verified in this card session
    if s1 == 0x63 and (s2 & 0x0F) < 2:
        print(f"Refusing PIN verify: only {s2 & 0x0F} tries left", flush=True)
        return False
    body = [ord(c) for c in USIM_PIN] + [0xFF] * (8 - len(USIM_PIN))
    d, s1, s2 = connection.transmit(toBytes("00200001") + [0x08] + body)
    if (s1, s2) == (0x90, 0x00):
        return True
    print(f"PIN verify failed sw={s1:02x}{s2:02x}", flush=True)
    return False


def read_res_ck_ik(reader_spec, rand, autn):
    res = ck = ik = auts = None
    conn = open_usim(reader_spec)
    if conn is None:
        write_status(state="NO_CARD")
        return res, ck, ik, auts
    try:
        with _Tx(conn):
            if not select_adf_usim(conn):
                write_status(state="NO_CARD", detail="ADF.USIM select failed")
                return res, ck, ik, auts
            if not verify_pin(conn):
                write_status(state="PIN_FAIL")
                return res, ck, ik, auts
            data, sw1, sw2 = conn.transmit(
                toBytes("008800812210" + rand.upper() + "10" + autn.upper()))
            if sw1 == 0x61:
                data, sw1, sw2 = conn.transmit(toBytes("00C00000") + [sw2])
                result = toHexString(data).replace(" ", "")
                print("Authentication result=<redacted>", flush=True)
                rc = result[0:2]
                if rc == "DB":  # success
                    res_length = data[1]
                    res = result[4:(4 + res_length * 2)]
                    ck_length = data[2 + res_length]
                    ck = result[(6 + res_length * 2):(6 + res_length * 2 + ck_length * 2)]
                    ik_length = data[2 + res_length + 1 + ck_length]
                    ik = result[(8 + res_length * 2 + ck_length * 2):
                                (8 + res_length * 2 + ck_length * 2 + ik_length * 2)]
                    write_status(state="AUTH_OK")
                elif rc == "DC":  # sync failure -> AUTS
                    auts = result[4:32]
                    write_status(state="AUTH_SYNC")
            else:
                print(f"Authentication failed sw={sw1:02x}{sw2:02x}", flush=True)
                write_status(state="AUTH_FAIL", detail=f"sw={sw1:02x}{sw2:02x}")
    finally:
        try:
            conn.disconnect()
        except Exception:
            pass
    return res, ck, ik, auts


def android_qmi_res_ck_ik(rand, autn):
    """Ask the Qualcomm modem's USIM service for one AKA response.

    Exchange files are short-lived. Key material is returned only to the local
    AMI client and is deliberately never written to the bridge log.
    """
    adb = os.environ.get("SWU_ADB", "").strip()
    adb_serial = os.environ.get("SWU_ADB_SERIAL", "").strip()
    exchange_dir = os.environ.get("SWU_ANDROID_EXCHANGE_DIR", "").strip()
    if not adb or not exchange_dir:
        raise RuntimeError("SWU_ADB and SWU_ANDROID_EXCHANGE_DIR are required")
    os.makedirs(exchange_dir, mode=0o700, exist_ok=True)
    lock_handle = open(os.path.join(exchange_dir, "android-qmi.lock"), "a+b")
    fcntl.flock(lock_handle, fcntl.LOCK_EX)
    fd, challenge_path = tempfile.mkstemp(prefix="ims-aka-", suffix=".bin", dir=exchange_dir)
    remote_challenge = "/data/local/tmp/aka_challenge.bin"
    remote_result = "/data/local/tmp/aka_result"

    def run_adb(args, timeout=35):
        command = [adb]
        if adb_serial:
            command += ["-s", adb_serial]
        completed = subprocess.run(command + args, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, timeout=timeout)
        if completed.returncode != 0:
            raise RuntimeError("ADB operation failed")
        return completed.stdout

    def windows_path(path):
        return subprocess.check_output(["wslpath", "-w", path],
                                       stderr=subprocess.DEVNULL).decode().strip()

    try:
        challenge = binascii.unhexlify(rand) + binascii.unhexlify(autn)
        if len(challenge) != 32:
            raise ValueError("AKA challenge must contain 16-byte RAND and AUTN")
        with os.fdopen(fd, "wb") as output:
            output.write(challenge)
        os.chmod(challenge_path, 0o600)
        del challenge

        run_adb(["shell", "rm -f %s %s" % (remote_challenge, remote_result)])
        run_adb(["push", windows_path(challenge_path), remote_challenge])
        run_adb(["shell", "chmod 644 %s; touch %s; chmod 666 %s" %
                 (remote_challenge, remote_result, remote_result)])
        material = None
        # Asterisk allows only a short IMS-AKA response window.  Retry the
        # activity itself once instead of polling a failed launch for minutes.
        for _attempt in range(2):
            run_adb(["shell", "am force-stop lab.codex.simidentity; "
                     "am start -n lab.codex.simidentity/.ReadIdentityActivity"], timeout=10)
            for _ in range(5):
                status = run_adb(["shell", "content query --uri "
                                  "content://lab.codex.simidentity.aka/material"], timeout=5)
                match = re.search(br"material=([A-Za-z0-9+/=]+)", status)
                if match:
                    material = base64.b64decode(match.group(1), validate=True)
                    break
                time.sleep(0.2)
            if material is not None:
                break
        if material is None:
            raise RuntimeError("Qualcomm AKA material timed out")
        run_adb(["shell", "am force-stop lab.codex.simidentity"], timeout=10)

        if len(material) >= 7 and material[:4] == b"AKA1":
            res_len, ck_len, ik_len = material[4], material[5], material[6]
            if not (4 <= res_len <= 16 and ck_len == 16 and ik_len == 16):
                raise RuntimeError("Qualcomm AKA material has invalid lengths")
            if len(material) != 7 + res_len + ck_len + ik_len:
                raise RuntimeError("Qualcomm AKA material has invalid size")
            offset = 7
            res = material[offset:offset + res_len]
            offset += res_len
            ck = material[offset:offset + ck_len]
            offset += ck_len
            ik = material[offset:offset + ik_len]
        elif len(material) >= 6 and material[:4] == b"AKAR":
            digest_len, data_len = material[4], material[5]
            if len(material) != 6 + digest_len + data_len:
                raise RuntimeError("Qualcomm opaque AKA material has invalid size")
            res = material[6:6 + digest_len]
            encoded_keys = material[6 + digest_len:]
            keys = base64.b64decode(encoded_keys + b"=" * ((-len(encoded_keys)) % 4),
                                    validate=True)
            if not (4 <= len(res) <= 16 and len(keys) == 32):
                raise RuntimeError("Qualcomm opaque AKA key lengths are invalid")
            ck, ik = keys[:16], keys[16:]
        else:
            raise RuntimeError("Qualcomm AKA material was not produced")
        return (binascii.hexlify(res).decode().upper(),
                binascii.hexlify(ck).decode().upper(),
                binascii.hexlify(ik).decode().upper(), None)
    finally:
        try:
            run_adb(["shell", "rm -f %s %s /data/local/tmp/subscriber_id" %
                     (remote_challenge, remote_result)], timeout=10)
        except Exception:
            pass
        try:
            os.remove(challenge_path)
        except OSError:
            pass
        fcntl.flock(lock_handle, fcntl.LOCK_UN)
        lock_handle.close()


def http_res_ck_ik(rand, autn):
    """Resolve IMS-AKA through the same provider API used by the SWu tunnel.

    This enables a remote Android SIM or any manager-side provider to serve
    both EAP-AKA and SIP IMS-AKA without exposing key material in logs or
    process arguments.
    """
    token = os.environ.get("SWU_HTTP_AKA_TOKEN", "").strip()

    def raw_b64(hex_value):
        return base64.b64encode(binascii.unhexlify(hex_value)).decode("ascii").rstrip("=")

    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    try:
        response = requests.post(
            HTTP_AKA_URL,
            headers=headers,
            json={"rand": raw_b64(rand), "autn": raw_b64(autn)},
            timeout=40,
        )
    except Exception as error:
        raise RuntimeError("Viti Operator AKA request failed") from error
    if response.status_code != 200:
        raise RuntimeError("Viti Operator AKA returned HTTP %d" % response.status_code)
    try:
        payload = response.json()
    except Exception as error:
        raise RuntimeError("Viti Operator AKA returned invalid JSON") from error

    def decode_field(name):
        value = payload.get(name, "")
        if not value:
            return b""
        try:
            return base64.b64decode(value + "=" * ((-len(value)) % 4), validate=True)
        except Exception as error:
            raise RuntimeError("Viti Operator AKA returned invalid %s" % name.upper()) from error

    auts = decode_field("auts")
    if auts:
        if len(auts) != 14:
            raise RuntimeError("Viti Operator AKA returned invalid AUTS length")
        write_status(state="AUTH_SYNC", provider="http")
        return None, None, None, binascii.hexlify(auts).decode().upper()
    res, ck, ik = decode_field("res"), decode_field("ck"), decode_field("ik")
    if not (4 <= len(res) <= 16 and len(ck) == 16 and len(ik) == 16):
        raise RuntimeError("Viti Operator AKA returned invalid key lengths")
    write_status(state="AUTH_OK", provider="http")
    return (
        binascii.hexlify(res).decode().upper(),
        binascii.hexlify(ck).decode().upper(),
        binascii.hexlify(ik).decode().upper(),
        None,
    )


def main():
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)
    if len(sys.argv) != 2:
        print(f"Usage: python {sys.argv[0]} <ini-file>")
        sys.exit(1)
    config = configparser.ConfigParser()
    config.read(sys.argv[1])
    cfg_endpoint = config.sections()[0]
    cfg_reader = config.get(cfg_endpoint, "reader")
    cfg_host = config.get(cfg_endpoint, "host")
    cfg_username = config.get(cfg_endpoint, "username")
    cfg_secret = config.get(cfg_endpoint, "secret")
    print(f"Endpoint={cfg_endpoint} reader={cfg_reader} host={cfg_host} user={cfg_username}")
    write_status(state="STARTING")

    manager = Manager(loop=asyncio.get_event_loop(), host=cfg_host,
                      username=cfg_username, secret=cfg_secret)

    @manager.register_event("FullyBooted")
    def on_booted(manager, message):
        print("Asterisk ready, triggering registration...")
        manager.send_action({"Action": "PJSIPRegister", "Registration": cfg_endpoint})

    @manager.register_event("AuthRequest")
    def on_auth(manager, message):
        algo = message.Algorithm
        rand = message.RAND
        autn = message.AUTN
        print(f"AuthRequest: Algorithm={algo} challenge=<redacted>")
        if HTTP_AKA_URL:
            res, ck, ik, auts = http_res_ck_ik(rand, autn)
        elif ANDROID_QMI:
            res, ck, ik, auts = android_qmi_res_ck_ik(rand, autn)
        else:
            res, ck, ik, auts = read_res_ck_ik(cfg_reader, rand, autn)
        if res is not None:
            manager.send_action({"Action": "AuthResponse", "Registration": cfg_endpoint,
                                 "RES": res, "CK": ck, "IK": ik})
        elif auts is not None:
            manager.send_action({"Action": "AuthResponse", "Registration": cfg_endpoint,
                                 "AUTS": auts})
        else:
            manager.send_action({"Action": "AuthResponse", "Registration": cfg_endpoint})
        print("AuthResponse sent: material=<redacted>")

    @manager.register_event("VitiSMS")
    def on_sms_protocol(manager, message):
        forward_sms_protocol_event(message)

    @manager.register_event("DTMFEnd")
    def on_dtmf_end(manager, message):
        forward_dtmf_event(message)

    @manager.register_event("Newchannel")
    def on_newchannel(manager, message):
        context = message.Context
        channel = message.Channel
        if context == cfg_endpoint:
            # The P-CSCF sends its reliable 183 roughly two seconds after the
            # outbound channel is created.  Signalling the bearer after only
            # 500 ms races that PRACK transaction: Asterisk sends UPDATE first,
            # receives 491 Request Pending, and never completes preconditions.
            # Schedule this outside the callback so IMS-AKA handling remains
            # responsive while we wait for PRACK/200 to settle.
            def confirm_bearer():
                manager.send_action({"Action": "DedicatedBearerStatus", "Channel": channel,
                                     "Status": "Up"})
                print(f"DedicatedBearerStatus sent: Channel={channel}")

            loop.call_later(4.0, confirm_bearer)

    manager.connect()
    try:
        manager.loop.run_forever()
    except KeyboardInterrupt:
        manager.loop.close()


if __name__ == "__main__":
    main()
