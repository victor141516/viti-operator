#!/usr/bin/env python3
"""Deterministic, non-production Milenage AKA broker for the SWu lab."""

import base64
from binascii import unhexlify
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import threading
import time

from CryptoMobile.Milenage import Milenage


K = unhexlify(os.environ["LAB_AKA_K"])
OPC = unhexlify(os.environ["LAB_AKA_OPC"])
AUTS = bytes.fromhex("00112233445566778899AABBCCDD")
VALID_MODES = {"success", "unavailable", "timeout", "auts"}
STATE_LOCK = threading.Lock()
STATE = {
    "mode": os.environ.get("LAB_AKA_MODE", "success"),
    "delay_seconds": float(os.environ.get("LAB_AKA_DELAY_SECONDS", "2")),
    "authenticate_requests": 0,
}


def decode(value):
    # Match the production manager's base64.RawStdEncoding contract.  This is
    # intentionally strict so a padded strongSwan request cannot pass the lab
    # while being rejected by the real manager.
    if "=" in value:
        raise ValueError("AKA fields must use unpadded Base64")
    return base64.b64decode(value + "=" * ((-len(value)) % 4), validate=True)


def encode(value):
    return base64.b64encode(value).decode("ascii").rstrip("=")


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path == "/control":
            return self.control()
        if self.path != "/authenticate":
            self.send_error(404)
            return
        if self.headers.get("Authorization") != "Bearer lab-token":
            self.send_error(401)
            return
        with STATE_LOCK:
            mode = STATE["mode"]
            delay_seconds = STATE["delay_seconds"]
            STATE["authenticate_requests"] += 1
        if mode == "unavailable":
            self.send_error(503)
            return
        if mode == "timeout":
            time.sleep(delay_seconds)
        try:
            size = int(self.headers.get("Content-Length", "0"))
            request = json.loads(self.rfile.read(size))
            rand = decode(request["rand"])
            autn = decode(request["autn"])
            if len(rand) != 16 or len(autn) != 16:
                raise ValueError("RAND and AUTN must be 16 bytes")
            if mode == "auts":
                response = {"auts": encode(AUTS)}
            else:
                milenage = Milenage(bytes(16))
                milenage.set_opc(OPC)
                res, ck, ik, _ = milenage.f2345(K, rand)
                response = {"res": encode(res), "ck": encode(ck), "ik": encode(ik)}
            body = json.dumps(response, separators=(",", ":")).encode("utf-8")
        except (KeyError, ValueError, TypeError, json.JSONDecodeError):
            self.send_error(400)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path != "/state":
            self.send_error(404)
            return
        if self.headers.get("Authorization") != "Bearer lab-token":
            self.send_error(401)
            return
        with STATE_LOCK:
            body = json.dumps(STATE, separators=(",", ":")).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def control(self):
        if self.headers.get("Authorization") != "Bearer lab-token":
            self.send_error(401)
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            request = json.loads(self.rfile.read(size))
            mode = request["mode"]
            if mode not in VALID_MODES:
                raise ValueError("invalid mode")
            delay_seconds = float(request.get("delay_seconds", STATE["delay_seconds"]))
            if delay_seconds < 0:
                raise ValueError("negative delay")
        except (KeyError, ValueError, TypeError, json.JSONDecodeError):
            self.send_error(400)
            return
        with STATE_LOCK:
            STATE["mode"] = mode
            STATE["delay_seconds"] = delay_seconds
            if request.get("reset_count"):
                STATE["authenticate_requests"] = 0
            body = json.dumps(STATE, separators=(",", ":")).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format, *_args):
        return


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
