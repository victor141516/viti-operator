#!/usr/bin/env python3
"""Deterministic, non-production Milenage AKA broker for the SWu lab."""

import base64
from binascii import unhexlify
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os

from CryptoMobile.Milenage import Milenage


K = unhexlify(os.environ["LAB_AKA_K"])
OPC = unhexlify(os.environ["LAB_AKA_OPC"])


def decode(value):
    return base64.b64decode(value + "=" * ((-len(value)) % 4), validate=True)


def encode(value):
    return base64.b64encode(value).decode("ascii").rstrip("=")


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/authenticate":
            self.send_error(404)
            return
        if self.headers.get("Authorization") != "Bearer lab-token":
            self.send_error(401)
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            request = json.loads(self.rfile.read(size))
            rand = decode(request["rand"])
            autn = decode(request["autn"])
            if len(rand) != 16 or len(autn) != 16:
                raise ValueError("RAND and AUTN must be 16 bytes")
            milenage = Milenage(bytes(16))
            milenage.set_opc(OPC)
            res, ck, ik, _ = milenage.f2345(K, rand)
            body = json.dumps({
                "res": encode(res),
                "ck": encode(ck),
                "ik": encode(ik),
            }, separators=(",", ":")).encode("utf-8")
        except (KeyError, ValueError, TypeError, json.JSONDecodeError):
            self.send_error(400)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format, *_args):
        return


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
