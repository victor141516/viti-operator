# Viti Operator architecture

This document describes the current implementation, its trust model, and the
protocol boundaries that must remain stable when the system evolves.

## Purpose and scope

Viti Operator exposes one physical mobile subscription to trusted web and
Android clients over a private Tailscale network. The system provides a shared
SMS history, shared call history, incoming-call fan-out, and multi-device audio
conferencing. The SIM may be attached to the server through an OpenStick modem
or remain in an Android phone that performs AKA calculations on demand.

Viti Operator is not a mobile network operator, an eSIM issuer, or a generic
SIP provider. It reuses a real carrier subscription and the carrier's standard
Wi-Fi Calling path. The carrier remains authoritative for numbering, routing,
billing, emergency-call policy, voicemail, and service availability.

## System map

```text
 Web browser ─┐                         ┌─ Carrier IMS ─ PSTN/mobile network
              ├─ HTTP + SIP/WebRTC ─ Manager ─ Asterisk ─ IMS/SIP
 Android app ─┘                       │      └─ RTP/media conference
                                     │
                            AKA provider selection
                              ├─ Android remote SIM agent
                              ├─ OpenStick over ADB/APDU
                              └─ external command adapter
                                     │
                                 physical SIM

 Asterisk/IMS ─ P-CSCF ─ userspace ESP ─ IKEv2/ePDG ─ carrier packet network
```

The manager is the control-plane authority. The engine is the carrier-facing
data plane. Clients never connect directly to the carrier.

## Components

### Go manager

The `viti-operator` process serves the embedded web application and the JSON
API, authenticates trusted clients, persists shared state, selects a SIM
authenticator, proxies SIP WebSocket traffic, talks to Asterisk over AMI, and
ingests authoritative engine events. It uses Go's standard `net/http` router
and stores state as one atomically replaced JSON file.

The manager deliberately remains healthy while the removable modem, SIM,
tunnel, or IMS registration is unavailable. `/api/v1/health` describes process
health; `/api/v1/status` describes line availability. This distinction lets
clients display and recover from a disconnected SIM without treating the
server as down.

### Carrier engine

The engine image comes from the bundled `engine/` directory. It
contains:

- a Python SWu client for IKEv2, EAP-AKA, NAT traversal, liveness, and userspace
  ESP;
- a TUN interface carrying the inner carrier-assigned IP connection;
- sysmocom Asterisk with PJSIP support for IMS-AKA, SMS, WebRTC, RTP, DTMF, and
  server-side conferences;
- hooks that send normalized call, SMS, and tunnel events to the manager.

The engine has `NET_ADMIN` and `/dev/net/tun`, but the manager does not. This
keeps carrier-network privileges in the smallest practical container.

### Web client

The web client is dependency-light HTML, CSS, and JavaScript embedded in the Go
binary. JsSIP 3.10.1 is bundled with esbuild for SIP over WebSocket and WebRTC
media. The browser uses the manager for history and status, while live call
signaling passes through `/sipws` to Asterisk.

### Android client

The native application uses Kotlin, Jetpack Compose, Material 3, coroutines,
DataStore, and AndroidX WebKit. It targets API 36 and supports API 24 and later.
The voice layer reuses the same bundled JsSIP implementation inside a controlled
WebView, which keeps call signaling behavior aligned with the web client.

The connection topology is also rendered from one shared server-owned web view.
Native Compose owns navigation, settings, notifications, and accessibility;
the embedded topology surface owns pan, pinch-to-zoom, hit testing, and detail
popovers.

Shizuku is optional but required for the remote-SIM routing controls that change
the allowed radio technologies and native IMS state. It does not turn an
ordinary application permission into unrestricted root access; the app calls a
small, explicit privileged service.

### coturn

Browsers use ICE to find a usable media path. Some WebViews deliberately hide
local host candidates and can therefore fail even when every device is on the
same tailnet. A private coturn instance supplies a deterministic relay
candidate. Only UDP 3479 is published on the selected Tailscale address; relay
ports remain on the Compose network between coturn and Asterisk.

### Loopback proxy

The loopback proxy exposes the application on `127.0.0.1` for the local desktop
without publishing the manager directly on every host interface. It injects
the configured API credential upstream. Remote clients use the Tailscale-bound
manager port instead.

## State ownership

The manager is the single source of truth for:

- SMS records and delivery state;
- call history and the current carrier-call lifecycle;
- the shared call-recording preference and recording-track metadata;
- known client identities and last-seen metadata;
- the selected remote SIM authentication agent;
- the desired IMS registration state.

State is written to `deploy/data/state.json` through a temporary file followed
by an atomic rename. Web and Android clients render the same records; neither
client maintains an independent authoritative history.

The event hub retains the most recent 256 events in memory. Long-polling clients
provide the last sequence they observed. Events accelerate refreshes, but
clients always recover by fetching current state, so missing an old event does
not corrupt history.

## Client identity and topology

Each client sends a heartbeat containing a stable client ID, kind, display
name, and SIM capability. Browser names are derived from the user agent; Android
uses device identity supplied by the OS. Names are not hardcoded in the
topology.

A client is online for 40 seconds after its most recent heartbeat. It then
appears as inactive until its display window expires: 10 minutes for a browser
and 24 hours for Android. Expiry is visual only. Identity and last-seen metadata
remain in persistent known-client state, so hiding a stale device cannot alter
history or SIM authentication ownership. Older state files without a last-seen
timestamp hide those devices until their next heartbeat. The topology response
combines client presence with live SIM, ePDG, IMS, and carrier status. This
normalized model is rendered by both the web application and Android. Last-seen
timestamps are checkpointed at most once per minute per unchanged client to
avoid rewriting the complete state file on every heartbeat.

## SIM authentication arbitration

Authentication mode is dynamic:

1. The most recently claimed, online Android authentication agent wins.
2. If that agent releases the role or goes offline, the configured local
   fallback is selected.
3. The normal fallback is OpenStick over ADB; an external JSON command adapter
   can be configured instead.
4. If no provider is available, line status becomes unavailable while the
   manager and histories remain usable.

The same selected provider handles challenges originating from both the SWu
EAP-AKA exchange and IMS SIP AKA. Role changes are persisted and published to
all clients. A phone that loses ownership stops its remote agent automatically.

### Remote Android AKA flow

```text
 Engine -> POST /api/v1/authenticate -> manager broker
 Phone  -> long poll pending challenge
 Phone  -> Android telephony ICC authentication -> USIM
 Phone  -> POST challenge response -> manager broker -> engine
```

The request contains RAND and AUTN; a successful response contains RES, CK,
and IK. Synchronization failure may return AUTS. Timeouts are bounded so an
offline phone cannot stall the carrier state machine forever.

This design remotely invokes the SIM; it does not copy Ki, OPc, or another
long-term secret out of the card.

## Carrier registration protocols

### SWu and ePDG

The engine discovers the carrier ePDG from the subscription's MCC/MNC using the
3GPP public naming convention. It negotiates IKEv2 over UDP 500/4500 and uses
EAP-AKA with the selected SIM provider. Successful IKE_AUTH creates IPsec
security associations and a carrier-assigned inner connection through the TUN
interface. NAT-T keepalives, liveness probes, fragmentation, reauthentication,
and tunnel supervision are handled by the SWu client.

### IMS

Asterisk registers a PJSIP endpoint with the carrier IMS through the P-CSCF
learned from the ePDG exchange. IMS registration may trigger a separate SIP AKA
challenge, which uses the same SIM provider. The stored `enabled` preference is
reconciled after engine or tunnel restarts.

Disabling server IMS registration is rejected while a call is open. When a
phone returns the line to native service, the Android app first unregisters the
server, then restores the phone's saved network mask and native IMS setting.

## Calling architecture

### Signaling

Web and Android clients register a trusted local SIP identity with Asterisk over
SIP WebSocket. The manager proxies `/sipws`; it does not interpret SIP call
signaling. Carrier-facing signaling uses SIP over the ePDG tunnel.

Outgoing calls are initiated by the active client through JsSIP. Incoming
carrier calls fan out to every registered client. A server-side mutex permits
only one carrier call at a time.

### First answer and conference joining

The first client to answer establishes the local side of an Asterisk conference
and cancels unanswered branches. Other clients then see **Join** and dial the
local conference extension. Joining is not limited to three devices; capacity
is governed by Asterisk resources and configured media limits.

Each local participant receives a mix of the carrier and all other local
participants, excluding its own audio. The carrier receives the combined local
side. Hanging up removes only that participant. The carrier leg ends when the
last local participant leaves.

Future call transfer between devices is therefore mostly a product operation:
join the destination, verify media, then leave the source. A dedicated atomic
handoff UI has not yet been implemented.

### Media

Client media uses WebRTC: ICE for path selection, DTLS-SRTP for key agreement
and encrypted RTP, and Opus or another browser-negotiated codec on the local
leg. Asterisk terminates WebRTC media, mixes conferences, and transcodes to the
codec negotiated with the carrier, commonly AMR or AMR-WB. DTMF is transported
as RFC 4733 telephone events.

`180 Ringing` causes the shared client voice component to generate local
ringback. `183 Session Progress` may carry carrier early media; in that case the
client stops generated ringback and plays the received stream. Any answer,
failure, cancellation, or hangup stops ringback.

### Call recordings

Call recording is server-owned and independent from diagnostic capture. Web and
Android edit one persisted setting; the engine checks it when a new carrier or
client conference channel starts. When enabled, Asterisk writes lossless 16-bit
PCM WAV tracks to the shared recordings volume: the carrier channel's receive
feed is the other party, and each local client channel's receive feed is that
device's microphone. The client propagates a stable client ID and a per-channel
participant ID so the manager can label tracks with the connected device name.

Call history stores track metadata and relative storage keys. Playback requests
select one or more track IDs. The manager aligns their source timestamps against
the call's common start and end, inserts silence outside each participant's real
presence, sums and clips PCM samples, and returns a standard WAV stream; the same
endpoint can mark the response as a download. This keeps late joins and early
departures at the same position even when only one track is selected. All tracks
are selected initially in both UIs, and any number of local participant tracks
can be independently deselected. Both clients expose seeking, elapsed and total
time, and cyclic 1×/1.5×/2× playback controls.

## SMS architecture

The manager accepts an outgoing SMS, writes it immediately with state
`sending`, and returns HTTP 202. A single worker lock serializes the carrier's
IMS MESSAGE channel while allowing web and Android clients to submit
concurrently. The engine sends SIP MESSAGE toward IMS. Resulting states are:

- `sending`: locally queued or being submitted;
- `submitted`: sent but no conclusive RP response arrived before timeout;
- `accepted`: acknowledged by the carrier submission path;
- `delivered`: independently verified by the configured receive path;
- `failed`: rejected locally or by the carrier;
- `received`: inbound and persisted.

Inbound engine events include their original timestamp and are deduplicated so
a webhook retry cannot create a second conversation row.

## Failure and recovery model

- Removing the modem changes line readiness but does not stop the manager.
- The adaptive authenticator probes for the configured ADB serial and resumes
  when it returns.
- Tunnel and engine lifecycle events update clients through the event hub.
- Open call rows older than the recovery threshold are finalized as
  `interrupted` after an unclean restart.
- An engine outage interrupts open calls so a stale row cannot permanently lock
  all clients.
- Client histories are recovered from disk, not reconstructed from transient
  browser or Android state.

## Security and trust boundaries

The present deployment assumes every device on the chosen tailnet that knows
the API key is trusted.

- Public client endpoints accept `Authorization: Bearer` or `X-API-Key`.
- Engine events use a different `X-Viti-Engine-Token` secret.
- AMI and local WebRTC credentials are independent generated secrets.
- Host ports are bound to the configured Tailscale address, except the explicit
  loopback proxy.
- ADB access on Windows is relayed by a narrow private bridge rather than a
  wildcard `adb -a` listener.
- Secrets and runtime configuration are gitignored.

The current web endpoint is HTTP inside Tailscale. Tailscale encrypts packets
between peers, but HTTP does not provide browser-origin authenticity by itself.
Do not expose these ports to an untrusted LAN or the public Internet. A future
multi-user or Internet-facing deployment would require TLS, per-user identity,
authorization, audit retention, rate limits, and secret rotation.

Diagnostic mode is disabled unless `VITI_DIAGNOSTICS_ENABLED` is explicitly
true. A configured directory alone never enables writers. Diagnostic mode is
intentionally unsafe for privacy: it may store credentials,
phone numbers, message content, decoded audio, signaling, and packet captures.
Its directory must remain outside the repository and should be deleted or
protected after testing.

## Technology decisions

| Decision | Reason | Consequence |
|---|---|---|
| Go singleton manager | Small static binary, simple concurrency, reliable embedded assets | Horizontal scaling needs an external store/event bus |
| Asterisk carrier engine | Mature SIP, IMS patches, media translation, and conferencing | Engine image is specialized and privileged |
| Shared JsSIP voice layer | Web and Android follow the same SIP/WebRTC behavior | Android voice depends on a controlled WebView |
| Server-owned history | Every client sees one consistent timeline | Server storage must be backed up |
| Lossless per-participant WAV recording | Selected sides can be remixed without transcoding or losing evidence | Recording storage grows with call duration and participant count |
| Dynamic SIM agent | The SIM can remain in a phone or move to USB without restart | Only one agent may own AKA at a time |
| Tailscale-only publication | Avoids a public ingress and simplifies peer reachability | Clients must join the tailnet |
| Private TURN relay | Makes WebView ICE behavior deterministic | Adds one always-on media component |
| Compose service split | Privileges and failure domains stay explicit | Deployment spans the manager and bundled engine build contexts |
| Opt-in full diagnostic capture | A failed call can be reconstructed after the fact | High disk use and sensitive artifacts while explicitly enabled |

## Repository layout

```text
android/                 Native Android application and tests
cmd/                     Manager, loopback proxy, and Windows ADB bridge CLIs
deploy/                  Example runtime configuration and persistent data mount
diagnostics/             Packet-capture sidecar image and scripts
docs/                    Maintained architecture, API, and operations reference
internal/app/            Go domain logic, adapters, API, persistence, embedded web UI
test/                    Browser end-to-end scenarios
web-src/                 JsSIP source and esbuild manifest
compose.yml              Production-oriented local/Tailscale service graph
compose.pcsc.yml         Optional host PC/SC reader override
```

## Standards and protocol references

The implementation relies on these protocol families:

- 3GPP TS 24.302: non-3GPP access and ePDG procedures;
- 3GPP TS 24.229: IMS SIP procedures;
- RFC 7296: IKEv2;
- RFC 4187: EAP-AKA;
- RFC 7383: IKEv2 fragmentation;
- RFC 3261: SIP;
- RFC 5764 and the WebRTC specifications: DTLS-SRTP media security;
- RFC 8445: ICE;
- RFC 5766: TURN;
- RFC 4733: RTP telephone events.

Carrier behavior can be stricter or more specialized than the base standards.
Engine logs and packet captures remain the final evidence for a particular
subscription.
