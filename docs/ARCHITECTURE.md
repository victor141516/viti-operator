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

- strongSwan `charon` for IKEv2, EAP-AKA, NAT traversal, liveness, and
  reauthentication, with `kernel-libipsec` processing ESP in userspace;
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

#### Browser audio-device preferences

Audio-device choice is browser-local rather than manager-owned. The desired
microphone and speaker identities are stored in origin-scoped `localStorage`;
the effective routes are volatile and are recomputed from `enumerateDevices()`
at startup and after every serialized `devicechange` event. A remembered device
is resolved first by its exact identifier, then by one unique physical group,
and finally by one unique label. Ambiguous labels are never guessed.

If a desired device disappears, the client preserves that preference but moves
the effective route to the system default and reports the fallback in the UI.
It restores the desired route when the device reappears, including when the
browser assigned a new identifier that can be matched unambiguously. Selecting
a microphone during a call first acquires a candidate track and then uses
`RTCRtpSender.replaceTrack()`. The selection is committed only after replacement
succeeds for the same call; failed or superseded operations stop the candidate
track and retain the previous preference. If the call merely ended, the desired
choice is kept for the next call.

Speaker routing uses `HTMLMediaElement.setSinkId()` where available. Browsers
that also expose `MediaDevices.selectAudioOutput()` receive a user-activated
native speaker picker so private output devices can be authorized. Browsers
without programmatic output routing keep the system speaker and explain that
the browser or operating-system controls must be used. The application does not
claim a preferred device is active unless the corresponding browser operation
has succeeded.

The Calls surface taps the actual outbound sender track and inbound remote
stream with unconnected Web Audio analyser nodes. It renders logarithmic input
and output level meters without adding a second playback route or changing the
media. This makes a selected-but-muted, silent, or replaced track visible while
the call is active. Synthetic test audio requires both the `testTone` query and
a per-tab `sessionStorage` opt-in set by the end-to-end runner; a copied or stale
test URL alone can never replace a production microphone.

During an established call, the dial pad provides local dual-tone feedback for
the full press gesture. On release it emits one RFC 4733 event whose bounded
duration matches the press, so a long press does not become repeated digits.

### Android client

The native application uses Kotlin, Jetpack Compose, Material 3, coroutines,
DataStore, and AndroidX WebKit. It targets API 36 and supports API 24 and later.
The voice layer reuses the same bundled JsSIP implementation inside a controlled
WebView, which keeps call signaling behavior aligned with the web client.

`VoiceCallService` owns that hidden WebView, the SIP/WebRTC runtime, and the
current Android call leg. Compose activities only attach to the process-owned
state, so ordinary activity recreation, navigation, or detachment does not end
a call. This is not durable call restoration: if Android kills the application
process, the local SIP leg and its in-memory Telecom scope are lost and normal
server-side stale-call reconciliation applies.

On API 26 and later, when the device exposes `FEATURE_TELECOM`, the client
registers with AndroidX Core-Telecom as a self-managed VoIP application. It
does not request the default dialer role and cannot receive or control ordinary
SIM calls. Outgoing SIP dialing starts only after Telecom admits the call, so a
native call or another system concurrency decision is never bypassed with the
legacy path. Incoming SIP legs are admitted immediately; answering still
requires microphone permission, and a system or accessory answer without that
permission rejects only the matching Viti leg.

Every local leg has an immutable token shared by native callbacks, notification
actions, and the JavaScript bridge. Delayed actions and events for an older leg
are ignored. The leg remains in an explicit `ENDING` state until the
Core-Telecom call scope has actually finished; a failed disconnect remains
visible and retryable rather than freeing the slot prematurely. Provisioning
changes are deferred until the current leg has ended.

Telecom is the source of truth for current and available audio endpoints and
temporary system mute/inactive state. User mute is separate and affects only
that client's WebRTC sender. Core-Telecom routes earpiece, speaker, wired, and
Bluetooth endpoints and supplies supported headset answer/disconnect actions.
An accessory disconnect leaves this one conference participant; the existing
server rule ends the carrier leg only when the last local participant leaves.

API 24-25 devices, and newer devices that do not expose Telecom, use a legacy
capability path based on audio focus, device observation, confirmed Bluetooth
SCO state, and a media session for headset buttons. Audio-focus transitions are
serialized and versioned: an obsolete loss or gain cannot overwrite a newer
state. If a current focus loss cannot suppress WebRTC media, the client ends its
local branch instead of claiming that audio is private. Exact headset behavior
on this fallback remains dependent on what the Android build and accessory
actually expose.

The service stays foreground with a quiet `specialUse` notification while it
keeps private VoIP signaling ready. During a call it declares `phoneCall` as
well. This lifetime is distinct from the remote-SIM authentication foreground
service.

#### Incoming-call presentation

`VoiceCallService` and `VoiceCallRuntime` remain the only source of truth for
an Android call. An incoming `CallStyle` notification and the private
`IncomingCallActivity` are projections of the current immutable leg snapshot;
neither can create or restore a call. The Activity binds only to an already
running service, without `BIND_AUTO_CREATE`, and closes unless the supplied leg
token still identifies an incoming leg in `RINGING` state.

The Activity is a single-task lock-screen surface with Answer and Decline
controls. Each notification PendingIntent contains the opaque leg token and is
invalidated when the current token changes. Activity recreation may preserve a
pending microphone-permission or answer intent in Android saved-instance state,
but only for that exact token. It rebinds and revalidates the service snapshot
before consuming the intent. A replacement call starts with clean presentation
state, so a delayed permission result or stale notification cannot answer it.

Answer, reject, and hang-up operations are serialized per leg across Compose,
notification, Telecom, and accessory surfaces. Duplicate operations are
ignored. A leave operation may supersede an answer suspended in SIP or Telecom;
if that answer is accepted late, the runtime rolls it back instead of exposing
the old leg as active.

For every still-ringing incoming leg the notification carries Android's
sanctioned full-screen intent. Android decides whether policy and current
device state permit the lock-screen Activity or require the expanded heads-up
fallback. Android 14 and later expose the separate full-screen-intent grant;
global notification access and the actual `Incoming calls` channel importance
are independent capabilities. The Connection screen reports these observed
values without promising a particular OEM presentation. The Viti ringtone is
owned separately by the service and stops when that local leg leaves `RINGING`.

The manager and conference remain authoritative across devices. All eligible
clients ring until the backend selects an answer or ends their fork. Losing
ringing legs receive their normal terminal event and cannot answer afterward;
clients may subsequently use the existing Join path for the active conference.

#### Android Auto projection

`VitiCarAppService` adds the projected Android Auto `CALLING` category without
changing the application's Telecom role. Its templated screen contains only a
driving-safe directory built from recent calls and synchronized contacts; it
does not expose a free-form dialer, messaging surface, navigation product, or
custom in-call dashboard. The screen asks the current host for
`CONTENT_LIMIT_TYPE_LIST` and never emits more rows than that host permits. The
use of the modern `Header` model makes Car App API 5 the declared minimum.

The car session is a volatile adapter. It binds to `VoiceCallService` and
observes the same immutable leg snapshot as Compose, while Core-Telecom remains
the source of truth for admission, native concurrency, audio endpoints, mute,
answer, reject, and disconnect callbacks. Once a call exists, Android Auto owns
the system in-call presentation. Destroying or recreating a car session only
unbinds this adapter; it never stops the foreground call service or leaves a
conference participant.

A successful bind alone is not treated as proof that the process can survive
the car session. If Android rejects the foreground-service start and no already
started service exists, dialing remains disabled and the car screen directs the
user to open Viti on the phone when safe. SIP provisioning is owned and retried
by `VoiceCallService`, not by an Activity or car session. Repeated identical
responses are idempotent, a settings change cancels its obsolete request, and a
changed response is deferred while a leg is active.

Microphone permission is required to place or answer a call. Nearby-device
permission is not a general call gate: without it, supported USB, speaker, or
earpiece routes can still work, while Bluetooth discovery and control may be
degraded. Actual route availability and accessory buttons are reported by the
Android Auto host, Telecom, the vehicle/OEM, and the connected accessory; Viti
does not infer them from physical attachment alone.

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
- per-device contact snapshots, their source revisions, and the merged contact view;
- the selected remote SIM authentication agent;
- the desired IMS registration state.

State is written to `deploy/data/state.json` through a temporary file followed
by an atomic rename. Web and Android clients render the same records; neither
client maintains an independent authoritative history.

The event hub assigns one random stream generation per manager process and
retains the most recent 256 globally sequenced events in memory. Long-polling
clients provide the last sequence they observed. Events accelerate refreshes,
but clients always recover by fetching current state, so missing an old event
does not corrupt history.

Trusted local integrations use one persistent manager WebSocket each. A stable
service name owns at most one live socket and advertises automatically granted
call capabilities plus capability-specific timeouts. The connection receives
the same complete event stream as every other integration; capabilities govern
commands, not visibility. A reconnect supplies its previous stream generation
and sequence. The manager atomically installs the live subscription while
capturing replay state, preventing a gap between history and live delivery; a
new generation or an evicted gap produces a current-state snapshot instead.

Each integration has a bounded event queue and ping/pong lease. A blocked
writer loses its connection rather than delaying producers or other services.
Command IDs are deduplicated in memory per service, with semantic request
content preventing an ID from being reused for a different action. Since the
deployment owns one physical line, call commands share one serialization lock.
Neither replay nor command deduplication is durable across manager restart.

An integration with `call.media` negotiates a call-scoped peer connection over
that same control socket. `media.attach` carries the SDP offer and returns both
`media.session.created` and a completed command result containing the SDP
answer; `media.ice` carries later remote candidates. The manager terminates
ICE/DTLS/SRTP itself and relays Opus RTP to a dedicated Asterisk `UnicastRTP`
channel in the existing ConfBridge. Bots therefore never receive the shared
SIP identity or dialplan access. The Compose deployment exposes one fixed UDP
ICE mux port regardless of the number of services, while the manager-to-engine
RTP sockets remain ephemeral and private to the Docker network.

Each service has at most one media session per call. A replacement, the owning
WebSocket generation ending, or the call ending closes only the matching media
leg. Service participants use the normal ConfBridge mix-minus: they hear all
other participants and every injected audio/DTMF event reaches the conference.
Their logical join/leave and WebRTC connect/disconnect events are distinct, and
they never enter the human-participant ownership registry. Recording labels use
the stable service name, but SDP, ICE, socket IDs, and transport generations are
never persisted.

## Contact synchronization and resolution

Android contact access is opt-in in two distinct steps: the user accepts an
in-app disclosure that names and phone numbers will be uploaded to the private
Viti server, and Android grants `READ_CONTACTS`. A permission retained from an
older local-only version does not constitute synchronization consent. The app
persists the separate consent decision, uploads the initial address book, and
observes the Contacts provider for later changes.

Each phone replaces its own complete source snapshot. Records retain the stable
client ID and name, source contact ID, regional context, alternative names,
number labels, original number text, and normalized E.164 value when it can be
determined. Per-source monotonic revisions make retries idempotent and prevent a
slow older request from overwriting a newer snapshot. A failed atomic state-file
write also rolls back the in-memory snapshot and revision.

The manager merges records only through a shared normalized phone identity;
equal names alone never merge people. Ambiguous numbers remain attached to their
original source record. Removing a record from one phone therefore preserves any
contribution from another phone. The merged view exposes all contributing
devices and enriches shared SMS and call history without replacing the original
telephone number.

The merged view is cached. A persisted global contact version changes only when
contact content changes and is returned by both `/status` and `/contacts`.
Android compares the lightweight status version with its per-server cache and
downloads the complete merged address book only on first load, after switching
servers, or when the version changes. `/contacts` returns content and version as
one atomic snapshot so a concurrent update cannot be cached under the wrong
version.

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

Android heartbeats can carry an optional privacy-bounded cellular observation
for the exact default-data subscription used by remote AKA. The observation may
contain home and serving operator names/MCC/MNC, Android service registration,
roaming, connected data technology, and Android's normalized signal level/dBm.
It never contains a phone number, IMSI, ICCID, SIM slot, cell identity, or
location. `READ_PHONE_STATE` gates protected service and access details; absent
permission, unsupported APIs, unknown values, and stale measurements remain
distinct instead of being inferred as healthy.

The Android process owns one subscription-bound telephony listener. API 31 and
later use `TelephonyCallback`; API 24–30 use `PhoneStateListener`. Subscription
or permission changes increment a binding revision, clear samples from the old
binding, and ignore late callbacks carrying the retired revision. Snapshot
capture and sequence allocation are serialized, so concurrent UI and remote
agent heartbeats cannot publish an older capture with a newer sequence.

The manager validates complete snapshots before changing presence. Sequence is
monotonic within one random process generation; the first newly received
generation retires the preceding generation, and delayed requests from retired
generations are rejected without trusting the phone's wall clock. These samples
and generation tombstones are intentionally volatile. Manager restart removes
them, while persisted client identity and authentication assignment remain.
Compatible clients that omit the new field do not erase a recent sample; reuse
of an ID by a different client kind does.

Report freshness uses server receipt time. Available nested measurements also
expire independently after five minutes using their age relative to the report
plus elapsed server time. The topology then removes the expired value, retains
its last measurement timestamp, and marks that field `stale`. The authentication
selection, SIM-holder flags, and SIM node are produced from one atomic copy of
assignment and presence state.

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
A challenge is pinned to the provider selected when that challenge is created;
a concurrent ownership change cannot redirect it halfway through. No cached or
fabricated AKA result substitutes for a fresh SIM calculation.

Authentication availability is not evidence about an established carrier
session. Losing the selected phone, unplugging OpenStick, or removing its SIM
makes overall line readiness false and blocks operations that may need fresh AKA,
but it does not overwrite the last observed ePDG, IMS, or call state. An already
established call continues until the carrier or engine supplies actual terminal
transport evidence.

Cellular observations describe only the selected phone subscription. They do
not participate in authentication arbitration and do not imply that the
server's carrier path uses that phone radio. The ePDG and IMS nodes continue to
come from the server telephony runtime. An OpenStick fallback is reported as an
APDU/SIM reader because the manager does not observe its cellular radio.

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
3GPP public naming convention. strongSwan `charon` negotiates IKEv2 over UDP
500/4500 and a custom EAP-AKA plugin forwards RAND/AUTN to the manager's selected
SIM provider. Successful IKE_AUTH creates security associations; the
`kernel-libipsec` plugin processes ESP in userspace and carries the
carrier-assigned IPv6 connection through `ipsec0`. The deployment does not use
kernel XFRM/IPsec.

Only traffic sourced from the assigned inner IPv6 address uses strongSwan's
route table. The adapter removes any broad `from all` rule created during
interface setup and installs a source-specific rule, preventing LAN, Docker,
Tailscale, manager, and WebRTC traffic from being captured by the carrier
tunnel. The cleanup drains every duplicate priority/table-220 rule and verifies
that exactly one source-specific rule remains. This is repeated after an
in-process charon recovery because that recovery keeps the existing network
namespace.

The VICI adapter normalizes strongSwan state into the manager's tunnel contract.
It also reconciles configuration attributes after attach and reauthentication:
a changed P-CSCF re-renders PJSIP, reloads `res_pjsip`, and restores the desired
registration; a changed inner address restarts Asterisk so its `ipsec0` transport
binds the new address. Startup clears persisted reconciliation markers, so an
attach that finishes after Asterisk starts is applied rather than mistaken for
already-consumed state. When that late first attach changes both values, the
engine renders the IMS objects first and then performs one Asterisk restart so
configuration and transport become visible atomically. The two applied values
are one canonical observation in `tunnel.applied.json`; a single durable atomic
replace advances both only after the complete transaction succeeds. Initial
attach is recorded through the same format only after its P-CSCF render succeeds.
Normal SA renewals do neither.

VICI control has operation-specific deadlines. Status requests remain short,
while initiation and reauthentication may outlive the synchronous AKA broker
timeout. The production entrypoint installs its shutdown trap before starting
workers or entering PIN, IKE, or P-CSCF waits, and every adapter startup wait is
cancellation-aware. During container shutdown the adapter first persists
`tunnel_down`, then publishes `DOWN`, closes any in-flight VICI request, and
tears down charon within a bounded budget. PID 1 waits for that state and the
exact adapter process to finish. This keeps the durable lifecycle contract
inside Docker's stop grace period even when the authentication provider is slow
or unavailable.

The engine refreshes two independent security-association lifetimes. It renews
the ESP CHILD SA every 30 minutes and proactively renews the parent IKE SA every
45 minutes. This avoids depending on carrier-specific renewal timing while
preserving the inner address, SIP dialogs, and active media. A lost or rejected
proactive renewal retains the current association and retries instead of
tearing down a working tunnel. A carrier-forced tunnel deletion remains a real
data-plane boundary and cannot preserve an existing carrier call.

Every SWu process creates a random generation and monotonically increasing event
sequence. `tunnel_start`, `tunnel_up`, and `tunnel_down` attempt a synchronous
commit to the bind-mounted event outbox before network delivery. Failure to
persist `tunnel_start` aborts the process before tunnel setup; a failed
`tunnel_up` or `tunnel_down` commit is logged, and its call site may continue or
terminate without direct HTTP fallback. The manager persists the current
generation, sequence, readiness, retired generations, and most recent causal
down boundary in the same state file as call history. Duplicate, retired,
out-of-order, and older cross-generation claims are ignored. The down boundary
survives a later up and manager restart so a delayed call hook from an older data
plane cannot reopen a terminated call.

The production image uses a digest-pinned Fedora base, the immutable Fedora
release repository, checksum-pinned Python source archives, and commit-pinned
native telecom sources. This prevents routine repository updates from silently
changing the packaged SWu stack between clean builds.

### IMS

Asterisk registers a PJSIP endpoint with the carrier IMS through the P-CSCF
learned from the ePDG exchange. IMS registration may trigger a separate SIP AKA
challenge, which uses the same SIM provider. The stored `enabled` preference is
reconciled after engine or tunnel restarts.

Disabling server IMS registration is rejected while a call is open. When a
phone returns the line to native service, the Android app first unregisters the
server, then restores the phone's saved network mask and native IMS setting.

## Calling architecture

### Destination normalization and session country

Every public carrier destination crosses a canonical E.164 boundary before it
is sent or persisted. The clients keep the country evidence volatile and detect
it once per fresh UI session. A web page reload or a new Android task/car session
creates new evidence; navigation and Android Activity recreation reuse the
existing session result. No country preference is stored, and the manager never
uses its own host location as a substitute for the user.

Android first requests a fresh service-state observation for the selected
default-data subscription. A current in-service `networkCountryIso` is the
strong source. Initial callback registration is polled for at most two seconds;
if it remains pending, detection fails closed rather than committing weaker
evidence for the session. If phone-state permission is denied, cellular service
is unsupported, or the subscription is not in service, the app may use the same
conservative fallback as the browser. That fallback requires the direct public
IP country to agree with a uniquely mapped system timezone, or with the locale
region only when the timezone is not country-specific. A VPN/Tailscale exit-node
conflict, missing signal, or other disagreement yields no country.

Canonical `+` E.164 input bypasses country detection and remains usable against
an older manager. Formatted `+` input, national trunk prefixes, and recognized
international-dialing prefixes go through the manager normalizer backed by the
phone-number metadata library. Emergency, short, service, extension, pause/wait,
and URI inputs are rejected. The private `*43` echo and `*44` conference-join
extensions use separate explicit paths and never enter public normalization.

Web, Compose, and Android Auto serialize destination preparation and revalidate
line/call state before issuing the SIP INVITE. The manager also normalizes the
non-interactive call API before persistence. Asterisk is the final defense: its
public outbound route accepts only `+` followed by 8–15 digits with a non-zero
first digit, and rejects everything else before lifecycle notification or
carrier dialing. Carrier interpretation of national input—especially while
roaming—is therefore not assumed.

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

Microphone mute is local to each web or Android participant. The shared JsSIP
voice layer disables only that client's outbound audio sender, so the muted
client continues receiving conference audio and cannot mute another device.
Mute state is reapplied if WebRTC replaces the outbound sender during the same
call, and it resets when that client leaves or starts a new call. JsSIP remains
responsible for restoring media on unmute so SIP hold state is respected.

Future call transfer between devices is therefore mostly a product operation:
join the destination, verify media, then leave the source. A dedicated atomic
handoff UI has not yet been implemented.

### Logical participation and media recovery

A SIP/WebRTC channel is transport, not user intent. When Wi-Fi, mobile data,
Tailscale, ICE, or SIP WebSocket temporarily loses a local media leg, Asterisk
emits `participant_left` but keeps the marked carrier channel in ConfBridge. The
manager marks that connection generation disconnected while retaining its stable
logical `client_id`. It does not start a grace timer and does not infer an explicit
hangup when every local channel is absent.

Web and Android retain the call trace, last admitted connection generation, and
client-local mute preference. After signaling returns they create a fresh
`connection_id` and dial the private `*44` route, which joins the existing carrier
conference rather than placing another carrier call. Another trusted client may
also join while the original client has no media. A carrier `call_result`, real
terminal data-plane boundary, or missing active call makes recovery terminal and
prevents resurrection.

User intent follows a separate, idempotent manager operation. A leave request
names the stable client, current connection, optional last admitted connection,
and a unique operation ID. The registry removes logical ownership atomically
before AMI control. Non-final departure removes only the exact local channel;
final departure closes further admission before ending the conference. If the
HTTP response or AMI operation fails, the client repeats the same operation ID so
the retry cannot target a replacement leg.

Each `connection_id` belongs to exactly one Asterisk admission attempt and one
channel. The manager serializes admission under a mutex, rejects stale or reused
generations, and retains exact superseded channels until AMI confirms their
removal. Before entering ConfBridge, the dialplan makes a synchronous
`participant_joined` request. Only a `204` admits the channel. Admission is never
replayed from the event outbox because its original channel would no longer be
waiting.

The response can still be lost after the manager commits admission. In that
ambiguous case Asterisk stays out of ConfBridge and durably emits
`participant_join_aborted`. The manager keeps abort tombstones and a predecessor
chain until a real `participant_left` proves which generation entered the
conference. Delayed, duplicate, reversed, or consecutively interleaved aborts
therefore restore the last non-aborted logical owner; a same-ID channel collision
cannot revoke the valid channel. Confirmed participant departure and terminal call
cleanup prune this rollback evidence.

This logical-membership registry, its generations, tombstones, pending AMI drops,
and leave-operation cache are intentionally process-local. Persistent call history
may survive a manager restart, but the exact participation and recovery guarantees
in this section do not. Server-restart survival requires a separate persisted or
Asterisk-reconstructed control-plane design.

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

#### Live client quality evidence

Each active web or Android participant samples its own `RTCPeerConnection`
statistics. The sample is volatile and belongs to one immutable call-leg token
and one peer-connection instance; it is not written to call history. A replaced
peer connection cancels the previous sampler, and terminal events discard any
late asynchronous result. Android additionally rejects samples for another leg
or with a non-increasing sequence number.

Incoming packet loss is calculated over consecutive counters for each continuing
audio RTP report and is then aggregated. Outgoing loss uses the equivalent
linked `remote-inbound-rtp` counters when the browser exposes them; otherwise it
uses the peer's latest RTCP `fractionLost`, whose exact interval is not exposed.
A first sample, idle interval, counter reset, signed loss correction, added or
removed SSRC, or selected ICE-pair change starts a new counter baseline instead
of producing an invented percentage. Incoming loss and jitter describe RTP
received by the local client. Outgoing loss is shown only when the media peer
supplies a linked RTCP report. Jitter and RTT values defined in seconds by
WebRTC statistics are converted to milliseconds for display. Incoming and
outgoing counter intervals remain separate; an unavailable RTCP interval is
reported as such.

Media RTT comes from a linked remote-inbound RTCP report when available. The
fallback ICE-path RTT is the selected candidate pair's current STUN RTT. Neither
measurement includes the carrier, PSTN, or called party. The route is resolved
from the audio RTP transport's selected candidate pair; a unique legacy selected
pair is accepted only when transport linkage is unavailable. The UI says
**TURN relay** only when a selected candidate is observed as `relay`, otherwise
**ICE direct**, and displays the observed relay/protocol value such as UDP. TURN
does not imply TCP.

The normal quality payload contains only timestamps, sequence numbers, numeric
measurements, candidate types, and protocol labels; its event wrapper retains
the usual call/client correlation identifiers. Candidate addresses, ports,
candidate or RTCStats IDs, SDP, raw reports, and audio levels remain outside this
UI path. Connecting, reconnecting, unsupported, and failed-statistics states
clear current measurements rather than freezing an earlier successful sample.
Ambiguous route evidence suppresses only the route label, so valid RTP metrics
can remain available. Route evidence may also appear while numeric measurements
are still collecting.

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
time, and cyclic 1×/1.5×/2× playback controls. During playback and immediately
after seeking, each track row is marked as present, not yet joined, or already
departed from its actual offset and WAV duration. Inactive rows are only a visual
timeline state: their selected-mix checkbox is never changed automatically.

## SMS architecture

The manager accepts an outgoing SMS, writes it immediately with state
`sending`, and returns HTTP 202. A single worker lock serializes the carrier's
IMS MESSAGE channel while allowing web and Android clients to submit
concurrently. The engine sends SIP MESSAGE toward IMS and requests a GSM status
report for every segment. Local HTTP or SIP success never means destination
delivery.

The message `state` describes the furthest evidenced lifecycle stage, while
`delivery_state` describes destination confirmation. Resulting message states
are:

- `sending`: locally queued or being submitted;
- `submitted`: handed to the IMS transport, without carrier acceptance yet;
- `accepted`: every segment has carrier acknowledgement or later service-centre
  evidence, but destination delivery is not fully confirmed;
- `delivered`: every segment has a successful GSM status report;
- `unknown`: the manager restarted before recording the submission result;
- `failed`: rejected locally or by the carrier;
- `received`: inbound and persisted.

`delivery_state` is `pending`, `delivered`, `failed`, or `unknown`. A successful
RP-ACK proves acceptance by the carrier message service, not delivery. TP-ST
`0x00` proves delivery to the destination device but is not a read receipt.
Temporary TP-ST values remain pending; terminal failures preserve their exact
status/cause and a human-readable explanation. If no conclusive report arrives
within 24 hours, the manager records `unknown` rather than claiming success.

### Durable evidence and correlation

The patched Asterisk module emits the manager message ID, multipart index,
engine instance, RP reference, TP message reference, TP status/cause, and the
service-centre timestamp when the protocol supplies it. `ami_usim` commits each
event with `fsync` and atomic rename to the bind-mounted
`/run/vowifi/event-outbox` before returning from the AMI callback. A worker keeps
retrying network errors and non-2xx responses and removes a file only after the
manager returns 2xx. A response that arrives before its `submitted` mapping gets
HTTP 409; the worker continues with later files and retries the early response
after the mapping exists.

Asterisk's track table is deliberately treated as volatile. The manager is the
persistent correlation authority:

- `submitted` establishes an immutable RP/TP mapping for one message segment;
- RP-ACK and RP-ERROR use only the RP reference, stable engine instance, and
  original event time within a 15-minute correlation window; inferred message
  ID, destination, TP reference, and multipart index are not trusted;
- status reports use only the TP reference, engine instance, protocol recipient,
  and service-centre submission timestamp (within 15 minutes of the persisted
  submission); reports without a decodable service-centre timestamp use the
  original event time within the 24-hour reporting window;
- reused references that match more than one persisted owner are rejected as
  ambiguous instead of being assigned to the newest message;
- multipart responses select their segment by RP for RP responses and by TP for
  status reports. Aggregate acceptance or delivery requires evidence for every
  expected segment.

These rules remain valid across manager, engine, and Asterisk restarts. If the
manager restarts while a row is still `sending`, it immediately and durably
changes that row to `state=unknown`, `delivery_state=unknown`; it never resends
the message automatically. A later durably queued protocol event may still
promote the record when it can be correlated safely.

Inbound engine events include their original timestamp and are deduplicated so
a webhook retry cannot create a second conversation row.

## Failure and recovery model

- Unexpected loss of a local SIP/WebRTC leg retains logical participation and
  the carrier conference indefinitely from the application's perspective. A
  reconnect creates a new media generation and joins the same call; it never
  redials the remote number. Explicit leave and transport loss are separate paths.
- Participant admission is synchronous and fail-closed. A lost response is
  compensated by a durable abort event; connection/channel fencing, timestamps,
  predecessor history, and tombstones make duplicate or reordered compensation
  converge without stale ownership or duplicate conference legs.
- Removing the modem or losing a remote SIM agent changes authentication and
  overall line readiness but does not stop the manager or terminate an
  established call. New outgoing calls and outgoing SMS submissions remain
  blocked until fresh AKA is available again. Incoming traffic may continue
  while the existing tunnel and IMS registration remain valid.
- The adaptive authenticator probes for the configured ADB serial and resumes
  when it returns. OpenStick is advertised only after ADB reports `device`, the
  SIM property is exactly `READY`, and the bridge package, executable helper,
  and content provider all respond. Readiness probes are serialized and briefly
  cached to avoid overloading the Windows ADB bridge. A transport timeout keeps
  a successful observation for at most 30 seconds; definitive evidence such as
  an absent SIM or missing device still fails immediately.
- Manager restart restores open call rows and the durable tunnel checkpoint;
  record age alone is not proof that a carrier call ended. It does not restore
  volatile logical-participant generations, pending exact-channel controls, or
  idempotent leave operations, so seamless media recovery across that restart is
  outside the current guarantee.
- A confirmed current-generation tunnel down or a newer engine-process start
  interrupts calls that existed on that data plane. A SIM-health failure alone
  does not. Normal IKE lifetime maintenance is make-before-break and therefore
  does not create a tunnel-down boundary.
- Engine call lookup, lifecycle mutation, comparison with the persisted down
  boundary, and state-file replacement are one atomic store operation. A flush
  failure rolls memory back before HTTP returns an error.
- When local outbox storage is writable, Asterisk call lifecycle hooks
  synchronously commit `call_in`, `call_out`, `call_active`, and `call_result`
  with stable call and trace IDs plus their source timestamp before attempting
  delivery. Network or manager failures are retried; a retry cannot duplicate,
  regress, or resurrect the call. Local outbox write failure falls back to a
  direct best-effort HTTP delivery and therefore does not provide this durability
  guarantee.
- Independent hook processes may finish out of order. Source timestamps refine
  the earliest start/answer and latest update, while the durable down boundary
  determines whether the call belongs to a failed data plane.
- Client histories are recovered from disk, not reconstructed from transient
  browser or Android state.
- Android activity recreation reattaches to the process-owned call service and
  does not recreate the SIP leg. Pending incoming-call permission state is
  restored only for the same immutable leg token.
- Android process death does not restore a local call. The server and engine
  remain authoritative for carrier/conference cleanup and history recovery.
- Telecom disconnect failures keep the exact leg in a retryable `ENDING` state;
  late callbacks cannot attach to a replacement leg.
- Legacy audio-focus commands are serialized. Obsolete completions are ignored,
  while failure to suppress media for a current focus loss terminates only that
  client branch.

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
| strongSwan userspace IPsec | Mature IKEv2/rekey state machine while keeping ESP on a TUN device | Engine requires `NET_ADMIN` and `/dev/net/tun` |
| Shared JsSIP voice layer | Web and Android follow the same SIP/WebRTC behavior | Android voice depends on a controlled WebView |
| Self-managed Android Telecom | Native concurrency, endpoints and headset actions without becoming the default dialer | API 24-25 and devices without Telecom use a vendor-dependent legacy audio path |
| Process-owned Android call service | Activity recreation does not destroy a call | Android process death still loses the local SIP leg |
| Token-validated incoming-call projection | Lock-screen and heads-up actions cannot revive or mutate a stale leg | Final full-screen behavior remains Android/OEM policy |
| Projected Android Auto calling | A bounded contact/recent directory starts Viti calls and the system supplies the in-call UI | Beta/labs distribution and real audio/control behavior depend on Google Play, the host, vehicle/OEM, and accessory |
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
