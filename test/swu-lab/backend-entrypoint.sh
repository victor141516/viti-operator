#!/bin/sh
set -eu

if [ -n "${LAB_NAT_GATEWAY:-}" ]; then
  ip route replace "${SWU_EPDG}/32" via "$LAB_NAT_GATEWAY"
fi

case "${SWU_BACKEND:-}" in
  python)
    exec python3 /usr/local/bin/tunnel_backend.py
    ;;
  strongswan)
    exec python3 /usr/local/bin/strongswan_adapter.py
    ;;
  *)
    echo "unsupported SWU_BACKEND: ${SWU_BACKEND:-unset}" >&2
    exit 2
    ;;
esac
