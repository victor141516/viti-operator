#!/bin/sh
set -eu

mkdir -p /run/strongswan

/usr/libexec/ipsec/charon &
charon_pid=$!

cleanup() {
  if [ -n "${echo_pid:-}" ]; then
    kill -TERM "$echo_pid" 2>/dev/null || true
    wait "$echo_pid" 2>/dev/null || true
  fi
  kill -TERM "$charon_pid" 2>/dev/null || true
  wait "$charon_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

for _ in $(seq 1 100); do
  [ -S /var/run/charon.vici ] && break
  [ -S /run/charon.vici ] && break
  kill -0 "$charon_pid" 2>/dev/null || exit 1
  sleep 0.1
done

ip -6 addr replace 2001:db8:39:200::10/64 dev ipsec0
python3 /opt/swu-lab/echo_server.py &
echo_pid=$!

swanctl --load-all
wait "$charon_pid"
