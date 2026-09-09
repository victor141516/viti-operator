#!/bin/sh
set -eu
case "${VITI_DIAGNOSTICS_ENABLED:-0}" in
  1|true|TRUE|yes|YES|on|ON) ;;
  *) exec tail -f /dev/null ;;
esac
mkdir -p /diagnostics/pcap
name="${CAPTURE_NAME:-network}"
# Full payload, all interfaces, immediate writes, one file per hour. No filter
# is intentional during development: this is the reconstructable wire record.
exec tcpdump -i any -n -s 0 -U -G 3600 \
  -w "/diagnostics/pcap/${name}-%Y%m%d-%H%M%S.pcap"
