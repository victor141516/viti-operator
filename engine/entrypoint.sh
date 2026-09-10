#!/bin/bash
# Engine entrypoint: render config -> hold PIN -> bring up the ePDG (SWu) tunnel with the
# pure-Python IKEv2/IPsec implementation (swu_ike.py) -> discover P-CSCF -> start Asterisk
# (IMS registration + voice/SMS).
#
# SWu tunnel: swu_ike.py (fasferraz/SWu-IKEv2, patched) is the sole ePDG tunnel path. It does
# IKEv2 + EAP-AKA (verifying the SIM PIN in its own PC/SC connection), userspace ESP over a
# tun device named "ipsec0" (so pjsip's bind_interface is unchanged), assigns the IPv6 inner
# address, and requests the IPv6 P-CSCF. A supervisor restarts it on exit; because every fresh
# start re-runs EAP-AKA WITH the PIN verify, the tunnel self-heals after a rekey/reauth teardown.
#
# PC/SC: this container is a pcscd CLIENT — it talks to the HOST pcscd via the bind-mounted
# /run/pcscd socket. The pcsc-lite client library is pinned to the same version as the host
# pcscd (see Dockerfile PCSC_VERSION) so the client/server protocol always matches.
set -u

export VOWIFI_RUNDIR="${VOWIFI_RUNDIR:-/run/vowifi}"
mkdir -p "$VOWIFI_RUNDIR" /logs /etc/asterisk

log() { echo "[entrypoint] $*"; }

# --- 1. Render configs from /config/instance.json --------------------------------
log "rendering configs..."
python3 /usr/local/bin/render.py || { log "render failed"; exit 1; }
# shellcheck disable=SC1091
set -a; . "$VOWIFI_RUNDIR/engine.env"; set +a
export USIM_PIN USIM_READER USIM_READER_INDEX USIM_READER_PORT USIM_IMSI VOWIFI_ID MANAGER_URL VOWIFI_RUNDIR
export SWU_BACKEND="${SWU_BACKEND:-python}"
export SWU_SOURCE SWU_EPDG SWU_APN SWU_MCC SWU_MNC SWU_IMEI SWU_IMEISV SWU_CHILD_REKEY_MINUTES SWU_IKE_REKEY_MINUTES SWU_IDR_MODE SWU_CP_MODE SWU_CP_MODE_ORDER SWU_USE_REAUTH_ID SWU_BACKEND
export SWU_HTTP_AKA_URL SWU_HTTP_AKA_TOKEN

# Delivery reports are irreplaceable protocol evidence. They are committed to
# the bind-mounted runtime outbox by ami_usim and retried here across manager or
# container restarts until a 2xx acknowledgement is received.
log "starting durable engine event outbox..."
python3 -u /usr/local/bin/event_outbox.py &

# --- 2. Start PIN keeper and wait for the SIM to be usable ------------------------
# pin_keeper holds CHV1 verified for ami_usim's SIP IMS-AKA. swu_ike verifies the PIN itself
# in its own connection for EAP-AKA, so both auth paths work on PIN-enabled SIMs.
if [ -n "${SWU_HTTP_AKA_URL:-}" ]; then
    log "SIM authentication delegated to Viti Operator"
    KEEPER_PID=""
else
    log "starting pin_keeper (reader=$USIM_READER)..."
    python3 -u /usr/local/bin/pin_keeper.py &
    KEEPER_PID=$!
fi

wait_pin() {
    for _ in $(seq 1 30); do
        st=$(python3 -c "import json;print(json.load(open('$VOWIFI_RUNDIR/pin_status.json'))['state'])" 2>/dev/null || echo "")
        case "$st" in
            VERIFIED|PIN_DISABLED) log "PIN state: $st"; return 0 ;;
            WRONG_PIN|PIN_BLOCKED) log "PIN problem: $st - continuing (manager will surface)"; return 1 ;;
        esac
        sleep 1
    done
    log "PIN keeper did not reach VERIFIED in time - continuing anyway"
    return 1
}
if [ -z "${SWU_HTTP_AKA_URL:-}" ]; then
    wait_pin || true
fi

# --- 3. Bring up the SWu (python IKEv2/IPsec) tunnel, supervised ------------------
log "starting SWu tunnel backend=$SWU_BACKEND (epdg=$SWU_EPDG apn=$SWU_APN reader=$USIM_READER_INDEX port=${USIM_READER_PORT:-none})..."
if [[ "${VOWIFI_DIAGNOSTICS_ENABLED:-0}" =~ ^(1|true|yes|on)$ ]]; then
  : > "$VOWIFI_RUNDIR/charon.log"
  SWU_LOG_TARGET="$VOWIFI_RUNDIR/charon.log"
else
  # swu_ike emits per-packet IKE decode dumps. Keep those available for an
  # explicit diagnostic run without continuously persisting them by default.
  rm -f "$VOWIFI_RUNDIR/charon.log"
  SWU_LOG_TARGET=/dev/null
fi
rm -f "$VOWIFI_RUNDIR/swu.ctl" "$VOWIFI_RUNDIR/swu_status.json"

# swu_ike is very chatty (per-packet IKE decode dumps). Full output goes either to
# the opt-in diagnostic file or through an operational-line filter. The latter keeps
# low-volume [swu_ike] lifecycle/rekey evidence in both Docker logs and a bounded file,
# instead of discarding the exact reason for every supervised reconnect.
(
  backoff=4
  while true; do
    log "swu_ike starting"
    SWU_OPERATIONAL_LOG=/logs/swu-operational.log
    if [ -f "$SWU_OPERATIONAL_LOG" ] && [ "$(stat -c %s "$SWU_OPERATIONAL_LOG" 2>/dev/null || echo 0)" -gt 1048576 ]; then
      tail -c 524288 "$SWU_OPERATIONAL_LOG" > "$SWU_OPERATIONAL_LOG.tmp"
      mv "$SWU_OPERATIONAL_LOG.tmp" "$SWU_OPERATIONAL_LOG"
    fi
    python3 -u /usr/local/bin/tunnel_backend.py 2>&1 | if [ "$SWU_LOG_TARGET" = "/dev/null" ]; then
          awk '/^\[swu_ike\]/ { print; fflush() }' | tee -a "$SWU_OPERATIONAL_LOG"
        else
          tee -a "$SWU_LOG_TARGET" | awk '/^\[swu_ike\]/ { print; fflush() }' | tee -a "$SWU_OPERATIONAL_LOG"
        fi
    rc=${PIPESTATUS[0]}
    log "swu_ike exited (rc=$rc); reconnecting in ${backoff}s"
    sleep "$backoff"; backoff=$((backoff*2)); [ "$backoff" -gt 60 ] && backoff=60
  done
) &
SWU_PID=$!

# --- 4. Wait for the tunnel, then (re)render pjsip with the discovered P-CSCF ------
log "waiting for SWu tunnel to establish..."
for _ in $(seq 1 90); do
  st=$(python3 -c "import json;print(json.load(open('$VOWIFI_RUNDIR/swu_status.json'))['state'])" 2>/dev/null || echo "")
  [ "$st" = "CONNECTED" ] && { log "SWu tunnel CONNECTED"; break; }
  sleep 1
done

log "waiting for P-CSCF discovery..."
for _ in $(seq 1 30); do
  [ -s "$VOWIFI_RUNDIR/pcscf" ] && break
  sleep 1
done
addr=$(cat "$VOWIFI_RUNDIR/pcscf" 2>/dev/null)
if [ -n "$addr" ]; then
  log "discovered P-CSCF: $addr"
  python3 /usr/local/bin/render.py || true   # re-render pjsip.conf with pcscf
  # Seed the applied-marker so swu_ike's in-process P-CSCF watcher only re-renders + reloads
  # Asterisk on a LATER change (reconnect/reauth), not redundantly right after this render.
  printf '%s' "$addr" > "$VOWIFI_RUNDIR/pcscf.applied"
else
  log "no P-CSCF discovered yet - continuing (manager will surface tunnel state)"
fi

# PJSIP resolves bind_interface=ipsec0 when its transport starts. Remember the initial inner
# address so swu_ike can detect a different one on a later reconnect and restart that transport.
inner=$(python3 -c "import json;print(json.load(open('$VOWIFI_RUNDIR/swu_status.json')).get('inner_ip',''))" 2>/dev/null || echo "")
if [ -n "$inner" ]; then
  printf '%s' "$inner" > "$VOWIFI_RUNDIR/inner_ip.applied"
fi

# --- 5. Start USIM<->AMI bridge and Asterisk -------------------------------------
log "starting ami_usim bridge..."
mkdir -p /logs /recordings
python3 -u /usr/local/bin/ami_usim.py /usr/local/etc/ami_usim.ini &

if [[ "${VOWIFI_DIAGNOSTICS_ENABLED:-0}" =~ ^(1|true|yes|on)$ ]]; then
  # Full SIP and RTP payload tracing is intentionally opt-in.
  ( for _ in $(seq 1 40); do
      sleep 3
      if asterisk -rx "pjsip set logger on" 2>/dev/null | grep -qi "enabled"; then
        asterisk -rx "rtp set debug on" >/dev/null 2>&1 || true
        break
      fi
    done ) &
fi

log "starting Asterisk..."
exec asterisk -f
