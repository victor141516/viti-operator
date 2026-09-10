#!/usr/bin/env python3
"""TCP and UDP echo endpoints reachable only through the lab CHILD SA."""

import selectors
import socket


ADDRESS = "2001:db8:39:200::10"
PORT = 18080


def main():
    selector = selectors.DefaultSelector()

    tcp = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
    tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    tcp.bind((ADDRESS, PORT))
    tcp.listen()
    tcp.setblocking(False)
    selector.register(tcp, selectors.EVENT_READ, "listener")

    udp = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
    udp.bind((ADDRESS, PORT))
    udp.setblocking(False)
    selector.register(udp, selectors.EVENT_READ, "udp")

    print(f"LAB_ECHO_READY=[{ADDRESS}]:{PORT}", flush=True)
    while True:
        for key, _ in selector.select():
            sock = key.fileobj
            if key.data == "listener":
                conn, _ = sock.accept()
                conn.setblocking(False)
                selector.register(conn, selectors.EVENT_READ, "tcp")
            elif key.data == "udp":
                data, peer = sock.recvfrom(65535)
                sock.sendto(data, peer)
            else:
                data = sock.recv(65535)
                if data:
                    sock.sendall(data)
                else:
                    selector.unregister(sock)
                    sock.close()


if __name__ == "__main__":
    main()
