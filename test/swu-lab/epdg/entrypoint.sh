#!/bin/sh
set -eu

mkdir -p /run/strongswan

/usr/libexec/strongswan/charon &
charon_pid=$!

cleanup() {
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

swanctl --load-all
wait "$charon_pid"
