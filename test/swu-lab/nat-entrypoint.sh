#!/bin/sh
set -eu

client_subnet="${LAB_CLIENT_SUBNET:-172.30.39.0/24}"
operator_subnet="${LAB_OPERATOR_SUBNET:-172.30.40.0/24}"

iptables -t nat -A POSTROUTING \
  -s "$client_subnet" -d "$operator_subnet" -j MASQUERADE
iptables -A FORWARD \
  -s "$client_subnet" -d "$operator_subnet" -j ACCEPT
iptables -A FORWARD \
  -s "$operator_subnet" -d "$client_subnet" \
  -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

touch /run/nat-ready
exec tail -f /dev/null
