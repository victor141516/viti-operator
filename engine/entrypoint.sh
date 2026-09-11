#!/bin/bash
# Engine entrypoint: render config -> hold PIN -> start strongSwan SWu -> discover
# P-CSCF -> start Asterisk (IMS registration + voice/SMS).
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
export SWU_SOURCE SWU_EPDG SWU_APN SWU_MCC SWU_MNC SWU_CHILD_REKEY_MINUTES SWU_IKE_REKEY_MINUTES SWU_IDR_MODE
export SWU_HTTP_AKA_URL SWU_HTTP_AKA_TOKEN

# Install an unset-safe shutdown path before starting workers or entering any
# potentially long PIN, IKE, or P-CSCF wait. Docker may stop the container at
# any point in startup.
OUTBOX_PID=""
KEEPER_PID=""
SWU_PID=""
AMI_PID=""
ASTERISK_PID=""
SHUTDOWN_STARTED=0

shutdown_engine() {
  [ "$SHUTDOWN_STARTED" = "1" ] && return
  SHUTDOWN_STARTED=1
  trap - TERM INT
  log "stopping engine services..."
  adapter_pid=$(cat "$VOWIFI_RUNDIR/swu_adapter.pid" 2>/dev/null || true)
  [ -n "$SWU_PID" ] && kill -TERM "$SWU_PID" 2>/dev/null || true
  # Also signal the adapter directly. The supervisor is a subshell and the
  # adapter is its grandchild, so waiting for only the supervisor is not a
  # sufficient persistence boundary.
  [ -n "$adapter_pid" ] && kill -TERM "$adapter_pid" 2>/dev/null || true
  if [ -n "$adapter_pid" ]; then
    for _ in $(seq 1 120); do
      st=$(python3 -c "import json;print(json.load(open('$VOWIFI_RUNDIR/swu_status.json'))['state'])" 2>/dev/null || echo "")
      process_state=$(awk '{print $3}' "/proc/$adapter_pid/stat" 2>/dev/null || echo "gone")
      # RuntimeContract publishes DOWN only after its tunnel_down event is
      # durable. Require both that boundary and completion of this exact
      # adapter process before allowing PID 1 to leave the namespace.
      if [ "$st" = "DOWN" ] && { [ "$process_state" = "gone" ] || [ "$process_state" = "Z" ]; }; then
        break
      fi
      sleep 0.1
    done
  fi
  [ -n "$SWU_PID" ] && wait "$SWU_PID" 2>/dev/null || true
  # Give the durable worker one final chance to deliver tunnel_down before exit.
  python3 /usr/local/bin/event_outbox.py --flush-once >/dev/null 2>&1 || true
  for pid in "$ASTERISK_PID" "$AMI_PID" "$OUTBOX_PID" "$KEEPER_PID"; do
    [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "$ASTERISK_PID" "$AMI_PID" "$OUTBOX_PID" "$KEEPER_PID"; do
    [ -n "$pid" ] && wait "$pid" 2>/dev/null || true
  done
}

handle_shutdown_signal() {
  shutdown_engine
  exit 143
}

trap handle_shutdown_signal TERM INT

# Delivery reports are irreplaceable protocol evidence. They are committed to
# the bind-mounted runtime outbox by ami_usim and retried here across manager or
# container restarts until a 2xx acknowledgement is received.
log "starting durable engine event outbox..."
python3 -u /usr/local/bin/event_outbox.py &
OUTBOX_PID=$!

# --- 2. Start PIN keeper and wait for the SIM to be usable ------------------------
# pin_keeper holds CHV1 verified for ami_usim's SIP IMS-AKA. SWu EAP-AKA is
# delegated through the same manager authentication provider when configured.
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

# --- 3. Bring up the strongSwan SWu tunnel, supervised ----------------------------
log "starting strongSwan SWu tunnel (epdg=$SWU_EPDG apn=$SWU_APN reader=$USIM_READER_INDEX port=${USIM_READER_PORT:-none})..."
if [[ "${VOWIFI_DIAGNOSTICS_ENABLED:-0}" =~ ^(1|true|yes|on)$ ]]; then
  : > "$VOWIFI_RUNDIR/charon.log"
  SWU_LOG_TARGET="$VOWIFI_RUNDIR/charon.log"
else
  # Full charon diagnostics are opt-in; normal operation keeps only bounded,
  # normalized lifecycle output.
  rm -f "$VOWIFI_RUNDIR/charon.log"
  SWU_LOG_TARGET=/dev/null
fi
rm -f "$VOWIFI_RUNDIR/swu.ctl" "$VOWIFI_RUNDIR/swu_status.json" \
  "$VOWIFI_RUNDIR/pcscf.applied" "$VOWIFI_RUNDIR/inner_ip.applied" \
  "$VOWIFI_RUNDIR/tunnel.applied.json" "$VOWIFI_RUNDIR/swu_adapter.pid"

# Full adapter/charon output goes to the opt-in diagnostic file. Normal operation
# retains only normalized lifecycle evidence in Docker logs and a bounded file.
(
  stop_supervisor=0
  active_pid=""
  stop_adapter() {
    stop_supervisor=1
    # The active child is either the adapter or the reconnect delay. Signaling
    # it keeps container shutdown bounded even when the adapter already failed.
    [ -n "$active_pid" ] && kill -TERM "$active_pid" 2>/dev/null || true
  }
  trap stop_adapter TERM INT
  backoff=4
  while [ "$stop_supervisor" = "0" ]; do
    log "strongSwan SWu adapter starting"
    SWU_OPERATIONAL_LOG=/logs/swu-operational.log
    if [ -f "$SWU_OPERATIONAL_LOG" ] && [ "$(stat -c %s "$SWU_OPERATIONAL_LOG" 2>/dev/null || echo 0)" -gt 1048576 ]; then
      tail -c 524288 "$SWU_OPERATIONAL_LOG" > "$SWU_OPERATIONAL_LOG.tmp"
      mv "$SWU_OPERATIONAL_LOG.tmp" "$SWU_OPERATIONAL_LOG"
    fi
    if [ "$SWU_LOG_TARGET" = "/dev/null" ]; then
      python3 -u /usr/local/bin/strongswan_adapter.py \
        > >(awk '/^\[strongswan-adapter\]/ { print; fflush() }' | tee -a "$SWU_OPERATIONAL_LOG") 2>&1 &
    else
      python3 -u /usr/local/bin/strongswan_adapter.py \
        > >(tee -a "$SWU_LOG_TARGET" | awk '/^\[strongswan-adapter\]/ { print; fflush() }' | tee -a "$SWU_OPERATIONAL_LOG") 2>&1 &
    fi
    active_pid=$!
    printf '%s' "$active_pid" > "$VOWIFI_RUNDIR/swu_adapter.pid"
    if wait "$active_pid"; then
      rc=0
    else
      rc=$?
    fi
    rm -f "$VOWIFI_RUNDIR/swu_adapter.pid"
    active_pid=""
    [ "$stop_supervisor" = "1" ] && break
    retry_delay=$(python3 -c \
      'import sys; from tunnel_contract import supervisor_retry_delay; print(supervisor_retry_delay(sys.argv[1], sys.argv[2]))' \
      "$VOWIFI_RUNDIR" "$backoff" 2>/dev/null || printf '%s' "$backoff")
    log "strongSwan SWu adapter exited (rc=$rc); reconnecting in ${retry_delay}s"
    sleep "$retry_delay" & active_pid=$!
    wait "$active_pid" 2>/dev/null || true
    active_pid=""
    backoff=$((backoff*2)); [ "$backoff" -gt 60 ] && backoff=60
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
inner=$(python3 -c "import json;print(json.load(open('$VOWIFI_RUNDIR/swu_status.json')).get('inner_ip',''))" 2>/dev/null || echo "")
if [ -n "$addr" ]; then
  log "discovered P-CSCF: $addr"
  if ! python3 /usr/local/bin/render.py; then
    log "render failed while applying the initial P-CSCF"
    shutdown_engine
    exit 1
  fi
else
  log "no P-CSCF discovered yet - continuing (manager will surface tunnel state)"
fi

# Commit the initial tunnel endpoints as one atomic observation, and only after
# the P-CSCF render succeeded. A later reconnect can now distinguish a real
# change from the values Asterisk is about to consume at startup.
if [ -n "$inner" ] || [ -n "$addr" ]; then
  if ! PYTHONPATH=/usr/local/bin python3 -c 'import sys; from tunnel_reconcile import TunnelReconciler; TunnelReconciler(sys.argv[1]).record_initial(sys.argv[2], sys.argv[3])' "$VOWIFI_RUNDIR" "$inner" "$addr"; then
    log "failed to persist the initial tunnel observation"
    shutdown_engine
    exit 1
  fi
fi

# --- 5. Start USIM<->AMI bridge and Asterisk -------------------------------------
log "starting ami_usim bridge..."
mkdir -p /logs /recordings
python3 -u /usr/local/bin/ami_usim.py /usr/local/etc/ami_usim.ini &
AMI_PID=$!

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
asterisk -f &
ASTERISK_PID=$!
set +e
wait "$ASTERISK_PID"
ASTERISK_RC=$?
set -e
shutdown_engine
exit "$ASTERISK_RC"
