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
| `manager` | Tailscale TCP 18787 and UDP 8790 by default | Web, JSON API, integration/SIP WebSockets, and the trusted-service WebRTC ICE mux |
| `loopback-proxy` | 127.0.0.1 TCP 18788 by default | Local desktop convenience endpoint |
| `turn` | Tailscale UDP 3479 | Private ICE relay listener |
| `engine` | Tailscale UDP 5060, TCP 5061, UDP 10000–10059 | Optional native SIP and media; carrier tunnel stays internal |
| `capture-manager` | none | Shares manager network namespace and writes PCAP |
| `capture-engine` | none | Shares engine network namespace and writes PCAP |

AMI, engine WebSocket, and TURN relay ports are private to the Compose network.
Per-service RTP bridge sockets are also private and ephemeral between `manager`
and `engine`; do not publish them. `VITI_SERVICE_MEDIA_ENABLED=0` disables the
integration media endpoint. When enabled, `VITI_SERVICE_MEDIA_ADVERTISE` must be
the host/VLAN address reachable by the trusted service containers and UDP 8790
must be allowed on that trusted path.

### Trusted service media

Open `/api/v1/integrations/ws`, register a stable name with `call.media`, and
send `media.attach` with the target `call_id`, an Opus-capable `sdp_offer`, and
`purpose` set to `auxiliary` or `controller`. Apply the `sdp_answer` returned in
`media.session.created`; send subsequent candidates with `media.ice`. The full
answer already contains the manager candidates gathered before the response.

Use `call.dtmf` with the call and optional session ID to inject one RFC 4733
digit. The resulting source-attributed `call.dtmf` event is broadcast to every
integration. Replacing an attachment for the same stable service/call closes
the old peer connection. Reconnecting the control WebSocket always requires a
new SDP exchange; transport state is deliberately not recoverable.

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

Tap or hover a SIM, phone, carrier, ePDG, or IMS node to inspect its evidence
and observation time. Map labels stay intentionally short. For a selected remote
Android SIM, the detail panel can show home/serving networks, service state,
connected radio technology, and signal only when Android supplied those values.
`Not reported`, `Phone permission not granted`, `Not supported`, `Measurement
expired`, and an offline/stale report are different states. A current report
does not make an older nested radio measurement current.

On Android, grant **Phone** permission from the Connection screen if protected
service and data-access details are desired. Denying it does not prevent every
device from reporting public operator or signal information, and it does not by
itself disable remote AKA. The app observes only the default-data subscription
used for AKA and deliberately does not upload subscriber identifiers, phone
numbers, SIM slots, cell identity, or location.

Carrier identity parsed from the configured IMS realm and Android's serving
network are separate evidence. Likewise, a green phone cellular-service row
does not establish that the server ePDG tunnel or IMS registration is healthy;
use those dedicated nodes for the actual server carrier path. OpenStick exposes
SIM/APDU readiness only, not modem radio coverage.

The Android remote authentication service must remain allowed to run in the
background. On vendor Android builds, disable battery optimization for Viti
Operator if the agent stops polling while the screen is off.

## Android service-call integration

Viti calls are self-managed VoIP calls. The application never asks to become
the default phone application and does not take over ordinary carrier calls.
On API 26+ devices with Android Telecom, the system admits each Viti call and
applies native-call concurrency policy. A rejected admission is final for that
attempt; Viti does not bypass it through its Android 7 fallback.

Grant microphone access before answering or placing calls. Notification access
is strongly recommended so the foreground-service and CallStyle controls remain
visible. On Android 12+, grant nearby-device/Bluetooth access when prompted if
Bluetooth route discovery or control is required. The application declares
`MANAGE_OWN_CALLS`, foreground-service `specialUse`/`phoneCall`, audio-settings,
and microphone permissions; it does not declare or request the dialer role.

Open **Connection → Incoming call alerts** to inspect three separate Android
capabilities:

- global notification access;
- the current importance of the **Incoming calls** notification channel;
- the full-screen call-alert grant exposed by Android 14 and later.

Use the actions on that card to open the corresponding Android settings. A
high-priority call channel and notification access are needed for a prominent
visible alert. The full-screen grant makes the call eligible for lock-screen
presentation; it does not force Android or the device manufacturer to launch
the Activity in every foreground or unlocked state. When full-screen use is
not permitted, Android normally presents the same call as an expanded heads-up
notification. The service-owned ringtone is independent of notification sound
and continues only while the local leg is ringing.

The quiet **Call service** notification means the process-owned SIP service is
ready in the background. An active call uses the high-priority **Incoming
calls** channel and exposes answer/reject/end actions supported by that Android
version. Android notification-channel sound choices are persistent system
settings: if incoming calls are visible but silent, open Viti's notification
settings and verify that the Incoming calls channel has sound and vibration
enabled.

Audio routes shown during a call come from Telecom on supported devices. A
route change is reported as successful only after Android accepts it. Wired and
Bluetooth device changes update the list dynamically. On the API 24-25 fallback,
Bluetooth is shown as current only after SCO actually connects; the exact
mapping of headset play/call/end buttons depends on the vendor and accessory.

If a native call makes a Viti leg inactive, Viti suppresses that client's
WebRTC media without changing its user-mute preference. Rapid focus loss/gain
events are serialized so the newest state wins. If current media suppression
fails, Viti leaves that local conference branch rather than risking overlapping
audio. Other joined Viti clients and the carrier call continue under the normal
last-participant rule.

Activity recreation does not end a call. Force-stopping the application or an
Android process kill does: the local SIP leg is memory-only and is not restored
after process creation. Confirm server-side call state before starting another
call; stale history is reconciled by the manager/engine recovery flow.

The incoming-call Activity also survives ordinary configuration recreation. A
microphone permission or queued Answer operation is restored only for the same
leg token and is executed only after rebinding to the existing service and
confirming that the leg is still incoming and ringing. Recreating the Activity
does not itself answer, reject, recreate, or prolong a call.

When an end or reject reaches the service but Android does not close its Telecom
scope, the UI remains in **Ending** and offers the same operation again. Do not
clear application data to remove this state: retry once, inspect Android Telecom
and Viti diagnostics, and let the actual scope completion free the local slot.

### Android Auto projected calling

The projected calling surface requires all of the following:

- a phone running Android 9 or later with a compatible, current Android Auto
  host;
- Android Telecom support on that phone and Car App API 5 or later on the host;
- Viti microphone permission, a reachable Viti server, successful SIP
  registration, and no conflicting local call.

Outside a local developer/DHU setup, distribute the build through a Google Play
**Internal testing** or **Closed testing** track while Android Auto
communication calling remains a beta/labs category. Do not put a build
containing the `CALLING` category in Open testing or Production: Google
currently rejects those tracks. See the official
[Android Auto calling guide](https://developer.android.com/training/cars/communication/calling),
[communication-app status](https://developer.android.com/design/ui/cars/guides/app-types/communications),
and [car app quality checklist](https://developer.android.com/docs/quality-guidelines/car-app-quality)
before distributing a new build.

The app presents recent calls and synchronized contacts, bounded by the list
limit reported by the host. Selecting one requests a normal Viti service call.
Android Auto then replaces the template with its system in-call UI; Viti cannot
customize or promise every control shown there. Answer, reject, participant
departure, mute/inactive state, and endpoint changes flow through Core-Telecom.
Each answer, reject, active, and inactive remote-control callback has an
application timeout below Android's five-second deadline.

Android Auto may connect to the phone through USB or wirelessly. That transport
does not by itself identify the audio endpoint. Telecom and the vehicle host
decide whether audio uses car Bluetooth, USB, speaker, earpiece, or another
reported endpoint. `BLUETOOTH_CONNECT` is recommended for complete Bluetooth
route discovery and control on Android 12+, but denying it does not block calls
that can use USB, speaker, or earpiece. Validate both connection transitions on
the actual target vehicle or the Desktop Head Unit (DHU); automated model tests
cannot establish OEM audio routing or accessory-button behavior.

This phone module supports **Android Auto projection only**. It is not packaged
as an installable Android Automotive OS application and does not claim AAOS
compatibility. A vehicle with embedded Android Automotive may still expose a
projected phone call only when its OEM-provided host supports that behavior.

For a release candidate, run:

```sh
cd android
./gradlew testDebugUnitTest compileDebugAndroidTestKotlin verifyDebugCarManifest lintDebug assembleRelease
```

Then perform a DHU or real-vehicle smoke test covering outgoing call, incoming
answer, incoming reject, one participant leaving, Bluetooth-to-USB and
USB-to-Bluetooth changes, car-session disconnect/reconnect, and foreground cold
start. A compiled instrumentation APK is not evidence that those physical paths
were exercised.

## National dialing and country evidence

The web and Android clients detect a country once per fresh application session.
They do not expose a country setting and do not persist the result. Reloading the
web application, reopening the Android task after it was destroyed, or creating
a new Android Auto session performs a new detection; ordinary navigation and
Activity recreation do not.

Android prefers the serving country of the selected default-data subscription
when the OS supplies a fresh in-service observation. The optional **Cellular
network details** permission improves that signal, but denying it does not block
the conservative fallback. The browser and Android fallback use a direct HTTPS
request to `https://api.country.is/` and require its country to agree with the
device timezone or, for non-country-specific timezones, the locale region. The
request omits Viti credentials and referrer data, disables caching and redirects,
and accepts at most 1 KiB of JSON. The external service still observes the
client's public source IP; self-host or replace this dependency before deployment
if that disclosure is unacceptable.

VPNs and Tailscale exit nodes can make IP location disagree with the physical
country. That disagreement is intentionally an error, not an instruction to dial
using either country. If detection is unavailable, still initializing, or
ambiguous, enter the complete international number beginning with `+`. Explicit
canonical E.164 input never waits for country detection.

Viti parses national trunk prefixes and recognized IDD prefixes with established
numbering metadata and submits canonical E.164 to IMS. It does not rely on a
carrier to interpret national input or infer the user's current country while
roaming. Emergency numbers, short/service codes, extensions, and pause/wait
sequences are outside the supported dialing scope. Use the ordinary carrier
dialer for emergency service.

Automated normalization tests do not place calls. A carrier smoke test is still
a real billed network operation and must use an explicitly approved destination.
To diagnose a rejection without calling, send the intended input and transient
country to `POST /api/v1/calls/normalize` and inspect the returned E.164 value.

## Browser audio devices

Open **Calls → Call audio** to select this browser's microphone and, when
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

The input and output meters are active only while the corresponding call stream
exists. Input measures the track attached to the WebRTC sender, not merely the
device selected by the operating system; output measures the remote stream
attached to the call audio element. A moving input meter with a flat output
therefore has a different meaning from the inverse and helps localize one-way
audio before inspecting packet traces.

During a connected call, press and hold a dial-pad key to hear local DTMF
feedback. Releasing it sends one RFC 4733 digit with the measured press duration.
Pointer, touch, Space, and Enter activation follow the same lifecycle.

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

Even when high-volume diagnostics are disabled, the engine retains low-volume
SWu lifecycle and renewal lines in
`${VITI_DIAGNOSTICS_DIR}/engine/swu-operational.log`. The file is capped by
keeping the most recent 512 KiB whenever it exceeds 1 MiB. It contains the
reason for supervised tunnel recovery without retaining packet-by-packet IKE
dumps. Full `charon.log` and PCAP capture still require the explicit diagnostic
switch.

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

For an approximately hourly IMS interruption, inspect
`${VITI_DIAGNOSTICS_DIR}/engine/swu-operational.log` for strongSwan SA state,
VICI lifecycle events, and `IKE_SA rekey complete`. Healthy lifetime
maintenance completes without restarting Asterisk. The default proactive
periods are 30 minutes for the ESP CHILD SA and 45 minutes for the parent IKE SA;
`0` disables either timer through `SWU_CHILD_REKEY_MINUTES` or
`SWU_IKE_REKEY_MINUTES` in the rendered engine environment.

The adapter intentionally gives attach and reauthentication VICI operations
more time than the configured AKA request timeout; ordinary status operations
retain a short deadline. The entrypoint accepts termination throughout startup,
including while `IKE_AUTH` is blocked on AKA. On shutdown it records
`tunnel_down` durably before publishing `DOWN`, waits for the exact adapter
process, and bounds the remaining VICI/charon cleanup. A container that stops
within its configured grace period should therefore leave an exact
`tunnel_start`, `tunnel_up`, `tunnel_down` sequence even if AKA was stalled.
If charon crashes while the container remains alive, the supervisor starts a
fresh adapter generation in the same network namespace. The adapter removes all
stale table-220 rules before installing and verifying the single IMS-source
rule; unrelated IPv6 traffic must never use `ipsec0` during this recovery.

### Modem was unplugged

Leave the manager running. Reconnect the same ADB serial. The local fallback is
probed and selected automatically when no phone owns the SIM role, but it is
advertised as ready only when ADB reports `device`, the SIM state is exactly
`READY`, and the bridge package, executable helper, and content provider all
respond.

An established carrier call survives loss of the authentication source. New
operations that may require fresh AKA remain blocked. A real tunnel-down event,
a newer engine-process boundary, a carrier hangup, or a later authentication
challenge that cannot be answered may still end the carrier session. Reconnecting
the modem restores future AKA service; the manager never fabricates or replays
authentication material.

### Client network changed or local audio disappeared

Do not end the conference merely because the web or Android media leg vanished.
The marked Asterisk carrier channel is the authoritative live call. The client
should show **Media reconnecting**, register SIP again when reachable, and join
the existing conference through `*44` with a fresh connection generation. This
recovery has no local timeout; the remote party or carrier may still hang up.

1. Confirm the call remains open in `GET /api/v1/calls` and that the marked
   carrier is still present with `asterisk -rx "confbridge list"`.
2. Check manager traces for `participant_left`, the client's repeated recovery,
   and a later admitted `participant_joined` with the same trace ID but a new
   connection ID.
3. If admission received no successful synchronous response, expect a durable
   `participant_join_aborted` event and, while delivery is pending, its outbox
   file. Do not remove it; replay must compensate manager state before the client
   tries another generation.
4. A remote `call_result`, confirmed current-generation tunnel down, or newer
   engine-process boundary is terminal. Clients must stop retrying that trace.
5. When the user presses End, the client retries
   `POST /api/v1/calls/participants/leave` with the same operation ID until it
   succeeds or the call is already terminal. Do not substitute a raw SIP BYE:
   transport loss intentionally preserves logical membership.

The manager's participation registry is volatile. Restarting it during an active
call may preserve the history row and Asterisk carrier channel, but does not
promise the same logical ownership, predecessor fencing, or leave idempotency.
Collect logs and finish the call before a planned manager or engine restart.

### UI says another device has a call

Check Asterisk channels, the durable engine event outbox, the persisted tunnel
checkpoint, and manager call history. A manager restart deliberately preserves
an open call row because record age is not evidence that the carrier session
ended. A confirmed current tunnel-down event or a newer engine process boundary
interrupts calls from the superseded data plane automatically. Export the trace
before manual intervention.

### Call lifecycle event remains queued

When local outbox storage is writable, `call_in`, `call_out`, `call_active`, and
`call_result` are written synchronously under
`${VITI_DIAGNOSTICS_DIR}/engine/runtime/event-outbox` before delivery is
attempted. A file is removed only after the manager returns a successful 2xx
response.

If creating, writing, syncing, or renaming the local artifact fails, the notifier
falls back to direct best-effort HTTP delivery. In that exceptional case no
durable artifact may exist, so an empty outbox is not by itself proof that every
event was delivered. Inspect the engine log for an outbox error before drawing
that conclusion.

A 409 response for a private `call_active` or `call_result` event with an empty
peer means its matching start event is not durable yet, so the outbox retains and
retries it. A truly orphaned event can therefore remain queued indefinitely, but
it does not block later files. After confirming that no matching start event can
return, move only that artifact out of the outbox for forensic retention; do not
delete or clear the entire queue.

### Audio is intermittent

During a local call, expand **Live media quality** in the web or Android call
surface. It reports only that client's WebRTC leg. **Incoming loss** and
**Incoming jitter** cover RTP received by that device; **Outgoing loss** is
available only when RTCP from the media peer exposes it. **Media RTT** is the
RTCP round trip to the media peer, while **ICE path RTT** is the selected ICE
pair's STUN round trip. All RTT and jitter values are milliseconds. Incoming and
outgoing counter-based packet-loss percentages show their own sampling interval.
When outgoing loss comes from the peer's latest RTCP `fractionLost`, the UI says
that its exact interval is unavailable. None of these values measure the
carrier/PSTN leg.

The route label is based on the selected candidate pair, not configured server
intent. **TURN relay · UDP** means the client observed a relay candidate using
UDP; TURN can also use other transports and is not synonymous with TCP. During
ICE recovery the panel says **Media reconnecting** and removes the prior values.
**Collecting measurements**, **Measurements unavailable**, or **Live statistics
unavailable** are valid outcomes and should not be interpreted as zero loss.

Use the correlated trace to compare SIP state, ICE state, WebRTC statistics,
RTP packet capture, and decoded WAV directions. A signaling success does not
prove media reachability. Check TURN allocation and the Tailscale path before
changing the carrier tunnel.

### Android call controls or routes are missing

1. Check the Android version and whether the device exposes the Telecom feature.
   API 24-25 and feature-less builds intentionally use the legacy capability path.
2. Verify microphone, notifications, and nearby-device/Bluetooth permissions.
3. For Bluetooth, wait for the accessory's SCO connection; mere pairing does
   not prove that call audio is active.
4. If a native call is present, end it or wait for Android to reactivate Viti.
   Viti intentionally follows the system concurrency decision.
5. If the UI remains in **Ending**, retry the displayed end/reject action and
   inspect `dumpsys telecom` plus Viti diagnostics before restarting the app.
6. For Android Auto, confirm the phone is Android 9+, the host supports Car App
   API 5+, and the build came from an eligible Internal or Closed testing track.
   Sideloading alone does not prove that a production host will list a beta
   calling application.

### Android incoming call is audible but not visible

1. Open **Connection → Incoming call alerts** and grant global notifications.
2. Open the **Incoming calls** channel and restore high-priority alerts if the
   card reports that the channel is blocked or reduced.
3. On Android 14+, grant full-screen call alerts if lock-screen presentation is
   desired. Without it, use the Android heads-up notification to answer or
   decline.
4. Disable battery restrictions for Viti Operator if the foreground call
   service is being killed by the device vendor.
5. Test on the intended locked and unlocked device. A compiled instrumentation
   test cannot prove an OEM's full-screen or heads-up policy.

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
- Client network loss is recoverable without a local timeout, but seamless
  logical-participant recovery across manager or engine restart is not promised.
- One active SIM authentication agent; local USB is fallback, not concurrent.
- Trusted-tailnet authentication model; no per-user roles.
- HTTP on the tailnet; no application-layer TLS termination in this Compose
  project.
- Android remote routing depends on Shizuku and device/vendor telephony support.
- Android service-call integration uses self-managed Telecom on API 26+ only
  when the device exposes it; the API 24-25 fallback and headset button mapping
  remain vendor/accessory dependent.
- Android activity recreation preserves a call, but process death does not
  restore the local SIP/WebRTC leg. Saved incoming-alert state is token-scoped
  and never recreates the call.
- Full-screen incoming-call presentation depends on notification access, the
  user-controlled call channel, Android 14's full-screen grant where present,
  current device state, and OEM policy. The supported fallback is Android's
  CallStyle heads-up notification.
- Android Auto calling is a projected, beta/labs capability requiring Android
  9+, Car App API 5+, and Google Play Internal or Closed testing. Open testing,
  Production, and an installable Android Automotive OS client are not supported.
- Android Auto audio routes and accessory controls depend on the host,
  vehicle/OEM, connection mode, permissions, and accessory; they require DHU or
  real-vehicle validation for each supported combination.
- Carrier Wi-Fi Calling provisioning and policy remain outside the project's
  control.
- National dialing depends on volatile current-country evidence. VPN/exit-node
  disagreement or unavailable evidence deliberately requires explicit E.164;
  the server never guesses from its own location or the SIM home country.
- The default web/Android fallback contacts `api.country.is`, which observes the
  client's public source IP. Country data is not persisted by Viti.
- Destination SMS delivery reports depend on carrier/SMSC support and may remain
  unconfirmed; the service never equates acceptance with a read receipt.
