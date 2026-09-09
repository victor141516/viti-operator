# Operations guide

## Prerequisites

- Windows or Linux host with Docker Compose;
- Tailscale connected on the host;
- `/dev/net/tun` available to Docker;
- a compatible carrier SIM with Wi-Fi Calling provisioned;
- one supported SIM path: OpenStick over ADB, Android remote agent, external
  command adapter, or the optional PC/SC override;
- Android SDK/JDK 17 only when building the native app locally.

The engine source and its upstream license are included in `engine/`.

## Configuration files

Copy these templates and replace their example values:

- `.env.example` to `.env`;
- `deploy/viti.example.json` to `deploy/viti.json`;
- `deploy/instance.example.json` to `deploy/instance.json`.

Runtime files and secrets must not be committed. Important environment values:

| Variable | Purpose |
|---|---|
| `VITI_BIND_IP` | Host Tailscale address used for all remote port publication |
| `VITI_HTTP_PORT` | Remote web/API port, default 18787 |
| `VITI_LOCAL_HTTP_PORT` | Loopback proxy port, default 18788 |
| `VITI_API_KEY` | Trusted-client API credential |
| `VITI_ENGINE_EVENT_TOKEN` | Engine webhook credential |
| `VITI_AMI_SECRET` | Asterisk Manager Interface credential |
| `VITI_VOICE_PASSWORD` | Local SIP and TURN credential source |
| `VITI_LINE_NUMBER` | Physical line in E.164 format |
| `VITI_IMS_REALM` | Carrier IMS realm |
| `VITI_ADB_SERIAL` | Stable OpenStick ADB serial |
| `VITI_ADB_SERVER_SOCKET` | Restricted host ADB bridge address |
| `VITI_DIAGNOSTICS_DIR` | Absolute diagnostic directory outside the repo |
| `VITI_DIAGNOSTICS_ENABLED` | Explicit opt-in for unredacted high-volume diagnostics; default `0` |
| `VITI_DOCKER_SUBNET` | Explicit non-overlapping private Compose subnet |

## Build and start

```powershell
docker compose up --build -d
docker compose ps
```

The remote UI is `http://<VITI_BIND_IP>:<VITI_HTTP_PORT>`. The local desktop UI
is `http://127.0.0.1:<VITI_LOCAL_HTTP_PORT>`.

All application processes listen on all interfaces inside their containers.
Host exposure is restricted by the `ports` mappings in `compose.yml`.

## Build individual artifacts

### Go and web

```powershell
npm --prefix web-src ci
npm --prefix web-src test
npm --prefix web-src run build
go test ./...
go build ./cmd/viti-operator
```

The generated `internal/app/web/voice.bundle.js` is embedded by Go. Rebuild it
after changing `web-src/voice.js`.

### Android APK

```powershell
Set-Location android
.\gradlew.bat testDebugUnitTest assembleDebug
```

Output:

```text
android/app/build/outputs/apk/debug/app-debug.apk
```

The Android build copies the same generated JsSIP bundle into application
assets before compilation.

## Service responsibilities and ports

| Service | Host exposure | Notes |
|---|---|---|
| `manager` | Tailscale TCP 18787 by default | Web, JSON API, event long polling, SIP WebSocket proxy |
| `loopback-proxy` | 127.0.0.1 TCP 18788 by default | Local desktop convenience endpoint |
| `turn` | Tailscale UDP 3479 | Private ICE relay listener |
| `engine` | Tailscale UDP 5060, TCP 5061, UDP 10000–10059 | Optional native SIP and media; carrier tunnel stays internal |
| `capture-manager` | none | Shares manager network namespace and writes PCAP |
| `capture-engine` | none | Shares engine network namespace and writes PCAP |

AMI, engine WebSocket, and TURN relay ports are private to the Compose network.

## Windows OpenStick ADB bridge

First prepare the supported stock-Android stick using the
[component release ZIP](https://github.com/victor141516/viti-operator/releases/tag/openstick-android-bridge-v1.0.0)
and its [installer guide](../tools/openstick-android-bridge/README.md).
This is a separate prerequisite from the host-side ADB transport described below.
The installer verifies the hardware/firmware allowlist, installs the recovered
APK/helpers, and prints the serial for `VITI_ADB_SERIAL`. It does not install
Linux OpenStick, firmware, Docker, or the host ADB bridge. Its original payload
source remains unavailable; this limitation is documented in the release.

Do not run `adb -a`; it listens broadly and is especially unsafe while the
Windows firewall is disabled. Build the restricted bridge:

```powershell
go build -o viti-adb-bridge.exe ./cmd/viti-adb-bridge
```

Start it on one private WSL-facing host address:

```powershell
$wslAddress = Get-NetIPAddress -AddressFamily IPv4 |
  Where-Object InterfaceAlias -Like '*WSL*' |
  Select-Object -ExpandProperty IPAddress -First 1
.\viti-adb-bridge.exe -listen "${wslAddress}:5038"
```

Set `VITI_ADB_SERVER_SOCKET=tcp:<address>:5038`. The bridge rejects wildcard and
public listeners and forwards only to the loopback ADB server.

## Normal operation

Check containers and line state separately:

```powershell
docker compose ps
curl.exe http://127.0.0.1:18788/api/v1/health
```

Use the Connection screen for SIM, ePDG, IMS, carrier, and client presence.
Process health can be green while line health is red; that is expected when the
SIM is absent.

The Android remote authentication service must remain allowed to run in the
background. On vendor Android builds, disable battery optimization for Viti
Operator if the agent stops polling while the screen is off.

## Browser audio devices

Open **Connection → Call audio** to select this browser's microphone and, when
supported, its speaker. Choices are stored only in that browser origin, so an
HTTP loopback URL, an HTTPS deployment, and another host or port each have
independent preferences. Audio capture and device enumeration require a secure
browser context; loopback origins qualify, while a remote plain-HTTP origin may
not. Use an HTTPS endpoint when a browser does not permit microphone access on
the remote Tailscale URL.

Choose **Allow audio device access** when device names are hidden. The browser
may show a microphone permission prompt; the temporary permission stream is
stopped immediately after enumeration. A separate **Choose a speaker…** action
appears only when the browser supplies its native output picker. If speaker
selection is unsupported, route output with the browser or operating-system
audio controls instead.

Unplugging the selected microphone or Bluetooth headset deliberately falls back
to the system default without deleting the desired preference. The UI states
that fallback and restores the device after it reconnects when its identifier,
physical group, or unique label can be matched safely. Duplicate labels remain
on the default route rather than selecting an arbitrary device. A failed live
microphone switch leaves the previous call track and saved choice intact.

## Contact synchronization

Contact synchronization is disabled until the user chooses **Sync contacts**
and grants Android's contacts permission. The app then uploads names and phone
numbers to the configured Viti manager and observes subsequent address-book
changes. Revoking `READ_CONTACTS` stops further reads; already synchronized
server data remains until that phone can publish a later source snapshot or the
manager state is deliberately replaced.

Contact source records and revision metadata are application data in
`deploy/data/state.json`. They are included in normal state backups and shared
with every client authorized to use that manager. Treat the state file as
sensitive: it can contain contact names and telephone numbers in addition to SMS
and call history. Do not commit or distribute it.

## Updates

Before updating, keep a copy of:

- `.env`;
- `deploy/viti.json`;
- `deploy/instance.json`;
- `deploy/data/state.json`.

Then rebuild and recreate:

```powershell
docker compose build manager engine
docker compose up -d --force-recreate
```

Recreating containers preserves bind-mounted state and diagnostics. Changing
the manager does not require reprovisioning the SIM.

## Diagnostics

Set `VITI_DIAGNOSTICS_ENABLED=1` only for an explicit diagnostic run and set
`VITI_DIAGNOSTICS_DIR` to an absolute directory outside the repository. The
directory alone does not enable capture. While the switch is on, the stack records:

- manager JSONL session events;
- engine event-delivery results;
- Asterisk and SWu logs;
- manager and engine PCAP files.

Export one correlated report:

Use the correlated trace ID to inspect files in the configured diagnostics directory.
Local trace-export utilities and captured evidence are not distributed.

Diagnostic data is unredacted, sensitive, and unbounded. Disable it and recreate
the services when it is not needed:

```powershell
# .env
VITI_DIAGNOSTICS_ENABLED=0
docker compose up -d --force-recreate manager engine capture-manager capture-engine
```

Call recordings are independent of diagnostics. The shared client switch is
off by default and is persisted in `deploy/data/state.json`. Audio is stored in
`deploy/data/recordings`; back up or retain that directory according to product
policy if users rely on call history playback.

## Safe verification

Run local, carrier-free checks first:

```powershell
go test ./...
node .\test\e2e-web-echo.mjs
node .\test\e2e-web-conference.mjs
```

Carrier tests place real calls or send real SMS and require explicitly supplied test destinations.
Provide private phone numbers through environment variables or secure prompts;
do not add them to source-controlled configuration.

## Recovery playbook

### Manager is unavailable

1. Run `docker compose ps`.
2. Inspect `docker compose logs --tail 200 manager loopback-proxy`.
3. Verify the Tailscale address still exists on the host.
4. Recreate only the affected service after preserving its logs.

### Manager works but the line is unavailable

1. Open Connection and identify the first failed segment.
2. If SIM authentication failed, confirm the assigned Android agent is online
   or reconnect the configured OpenStick serial.
3. If ePDG failed, inspect SWu/IKE logs and UDP 500/4500 reachability.
4. If IMS failed, inspect Asterisk registration and SIP AKA logs.
5. Do not repeatedly reflash the modem: the stock-Android SIM bridge is only
   the local authentication transport and is not the carrier IMS stack.

### Modem was unplugged

Leave the manager running. Reconnect the same ADB serial. The local fallback is
probed and selected automatically when no phone owns the SIM role.

### UI says another device has a call

Check Asterisk channels and manager call history. Stale calls are reconciled on
restart, but an actually active conference must not be destroyed merely to
clear the UI. Export its trace before intervention.

### Audio is intermittent

Use the correlated trace to compare SIP state, ICE state, WebRTC statistics,
RTP packet capture, and decoded WAV directions. A signaling success does not
prove media reachability. Check TURN allocation and the Tailscale path before
changing the carrier tunnel.

### SMS remains submitted or delivery is unconfirmed

Carrier acceptance and destination delivery are different states. RP-ACK only
proves that the carrier message service accepted a segment. A successful TP
status report is required for `delivered`; carriers that do not provide one
leave the message pending until the 24-hour reporting window ends as
`unknown`. This is expected and is not a read receipt failure.

Pending engine evidence is stored below the bind-mounted
`${VITI_DIAGNOSTICS_DIR}/engine/runtime/event-outbox`. This outbox operates even
when diagnostic capture is disabled. Do not delete it while messages are in
flight. Engine/container recreation preserves it. A non-empty directory can
mean that the manager is unreachable or that an early protocol response is
waiting for its `submitted` reference mapping. Inspect manager and engine logs
before intervening; the worker retries automatically and deletes an event only
after a 2xx manager response.

After a manager restart, any locally persisted `sending` row whose asynchronous
worker was lost is changed to `unknown` immediately and durably. The manager
does not resend it because doing so could duplicate a real SMS. A later queued
status report can still update the row if its persisted RP/TP evidence identifies
exactly one message. An ambiguous reused reference is intentionally not guessed;
the affected messages eventually remain or become unconfirmed.

## Backup and retention

The minimum backup is `deploy/data/state.json`, `deploy/data/recordings`, and
the private configuration files. Product call recordings are application data;
PCAP and verbose traces are disposable diagnostic artifacts. Establish a
backup and disk-capacity plan before enabling either feature for long periods.
Recordings are retained indefinitely; diagnostic artifacts are separate.

## Known constraints

- One physical carrier line and one carrier call at a time.
- One active SIM authentication agent; local USB is fallback, not concurrent.
- Trusted-tailnet authentication model; no per-user roles.
- HTTP on the tailnet; no application-layer TLS termination in this Compose
  project.
- Android remote routing depends on Shizuku and device/vendor telephony support.
- Carrier Wi-Fi Calling provisioning and policy remain outside the project's
  control.
- Destination SMS delivery reports depend on carrier/SMSC support and may remain
  unconfirmed; the service never equates acceptance with a read receipt.
