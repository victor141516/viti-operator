# Manager API

The manager serves JSON under `/api/v1`, the web application at `/`, a trusted
integration WebSocket at `/api/v1/integrations/ws`, a SIP WebSocket proxy at
`/sipws`, and one engine-only webhook at `/api/engine/event`.

## Authentication and conventions

Except for health and static assets, client endpoints require either:

```http
Authorization: Bearer <VITI_API_KEY>
```

or `X-API-Key: <VITI_API_KEY>`. Engine events instead require
`X-Viti-Engine-Token`. JSON errors use:

```json
{"error":"human-readable English message"}
```

Stored and carrier-routed phone destinations use E.164 form. The normalization
endpoint is the only public-call API that accepts national or formatted input.
Timestamps are UTC RFC 3339 values. Unknown JSON fields are rejected and request
bodies have endpoint-specific size limits.

## Process and line state

### `GET /api/v1/health`

Unauthenticated liveness probe. A successful manager returns `200` with
`{"ok":true}` even when the SIM or carrier path is unavailable.

### `GET /api/v1/status`

Returns `ready`, `sim_ready`, `tunnel_ready`, `ims_ready`, `contacts_version`,
`provider`, `detail`, and `updated_at`. `ready` means the established SWu and
IMS carrier path is available; `sim_ready` independently indicates whether the
next AKA challenge can be answered. Consequently, `ready` may remain true while
`sim_ready` is false. `contacts_version` is the persisted version of the merged contact
content; a client that already holds this version need not download `/contacts`.

### `GET /api/v1/topology`

Returns the normalized SIM/authenticator, manager, ePDG, IMS, carrier, and known
client graph. Node states are `healthy`, `warning`, `failed`, `peer`, or `idle`.
This is the presentation-neutral source used by both clients. Offline web
clients remain visible for 10 minutes and offline Android clients for 24 hours;
the visibility filter does not delete their persistent identity records.

The response identifies the selected authentication source independently from
the server's ePDG tunnel and IMS registration. A remote Android SIM and its
matching client node are derived from one atomic presence snapshot. An Android
client may include volatile `cellular` details. `cellular_status` distinguishes
`observed`, `stale`, `not_observed`, and `unknown`; each nested field also has
an `availability` value of `available`, `unknown`, `permission_denied`,
`unsupported`, or server-derived `stale`. A current report can contain an
expired nested measurement. Expired values retain only their measurement time,
not the old operator, registration, technology, or signal value.

The carrier node exposes MCC/MNC only when they can be parsed from the configured
IMS realm. Cellular serving-network evidence reported by Android is separate
from ePDG/IMS runtime evidence and does not prove that server traffic uses the
phone's cellular radio.

### `POST /api/v1/clients/heartbeat`

Registers presence and device metadata:

```json
{
  "id": "stable-client-id",
  "kind": "web",
  "name": "Chrome on Windows",
  "has_sim": false,
  "auth_agent_id": "",
  "auth_active": false
}
```

Android uses `kind: "android"` and supplies an authentication agent ID when it
can serve SIM challenges. `auth_active` refreshes the active agent lease. A
valid heartbeat returns `204 No Content`.

Android may also send a complete optional `cellular` snapshot for the same
default-data subscription used by the SIM authentication engine:

```json
{
  "generation": "process-uuid",
  "generation_started_at": "2026-09-10T12:00:00Z",
  "sequence": 7,
  "measured_at": "2026-09-10T12:00:03Z",
  "subscription_role": "default_data",
  "home_network": {"availability":"available","operator":"O2","mcc":"214","mnc":"07","measured_at":"2026-09-10T12:00:03Z"},
  "serving_network": {"availability":"unknown"},
  "service": {"availability":"available","registration":"in_service","roaming":false,"measured_at":"2026-09-10T12:00:02Z"},
  "data_access": {"availability":"available","technology":"LTE","measured_at":"2026-09-10T12:00:02Z"},
  "signal": {"availability":"available","level":3,"dbm":-91,"measured_at":"2026-09-10T12:00:02Z"}
}
```

`has_sim` must agree with `subscription_role`: `true` means `default_data` and
`false` means `none`. Snapshots are ordered by `(generation, sequence)`. The
server accepts a newly received process generation, retires the previous one,
and rejects later deliveries from retired generations without comparing client
wall clocks. Omitting `cellular` preserves the last compatible observation for
older clients; changing the same client ID between Android and web clears it.
Cellular observations and retired-generation ordering are memory-only and are
discarded on manager restart.

## Authentication-agent assignment

### `GET /api/v1/sim-auth/assignment`

Returns the current `client_id`, `agent_id`, and assignment timestamp. Empty IDs
mean that the server should use its local fallback.

### `PUT /api/v1/sim-auth/assignment`

Claims or releases the role. Claiming a new agent atomically replaces the old
one. Releasing requires the current client identity so one phone cannot
accidentally release a newer owner's claim.

```json
{"client_id":"phone-id","agent_id":"phone-auth-agent","enabled":true}
```

Send the same identities with `enabled: false` to release the current phone's
role.

## IMS registration

### `GET /api/v1/ims-registration`

Returns the persisted desired state and current engine observation:

```json
{"enabled":true,"registered":true,"detail":"..."}
```

### `PUT /api/v1/ims-registration`

Requests registration or unregistration:

```json
{"enabled":false}
```

Unregistration returns `409` while a call is open and `501` for a telephony
adapter that cannot control registration.

## Incremental client history

### `GET /api/v1/history?cursor=<opaque-checkpoint>`

Authenticated, `Cache-Control: no-store`. Omit the cursor for a complete baseline.
The response is one version-1 `history.sync` batch:

```json
{
  "type": "history.sync",
  "version": 1,
  "mode": "delta",
  "base_cursor": "history-example:10",
  "cursor": "history-example:11",
  "messages": [],
  "calls": [],
  "deleted_messages": [],
  "deleted_calls": [],
  "contacts_version": 0
}
```

Treat cursors as opaque. `delta` contains complete replacement rows for changed
IDs, including modifications to older records; unchanged rows are absent.
Apply the batch only against its `base_cursor`, then commit rows and `cursor`
together. Deletions are ID tombstones. Repeated checks at the current cursor
return empty arrays and the same cursor. Calls use the existing call-list view,
including screening summaries rather than detailed opaque pipeline results.

An absent, malformed, future, expired, or previous-process cursor returns
`mode: "full"`. Invalid supplied cursors also include `reason: "cursor_invalid"`.
A full batch replaces both cached histories; its base cursor need not match.
The server retains at most 256 change batches and 4 MiB of replay row data in
memory. Eviction or restart causes a full replacement, not silent data loss.

### `GET /api/v1/history/ws?cursor=<opaque-checkpoint>`

Authenticated WebSocket using the same API-key headers, without bot capabilities,
SIP credentials, or media. It sends an initial `history.sync` batch and then
pushes batches only when the stored public history changes. Subscription and
snapshot ordering ensure mutations cannot fall between the baseline and live
delivery. Slow consumers reconnect and replay from their last committed cursor.

To check again when opening the app, send:

```json
{"type":"sync","cursor":"history-example:11"}
```

Send an empty cursor to request a full replacement after detecting divergence.
The server also sends small `history.invalidate` messages with a `resource`
name when status, contacts, recording settings, SIM assignment or bot inventory
configuration changes. These are metadata invalidations, not history rows or
replay checkpoints; reconnecting clients refresh visible metadata once.
Server pings every 30 seconds and a 75-second pong deadline bound dead sockets.
Application requests are limited to 4 KiB; arbitrary commands are unsupported.

The legacy `/messages`, `/calls` and `/events` contracts remain available for
existing web/integration consumers. Android no longer polls the full histories.

### `GET /api/v1/calls/active?trace_id=<trace-id>`

Authenticated, no-store, compact `{"active":true}` or `{"active":false}` response.
True requires a matching non-ended call in the `active` state. Native media
recovery uses this endpoint instead of downloading every call record.

## General event feed

### `GET /api/v1/events?after=<sequence>`

Long-polls for events newer than the supplied sequence. Common event types are
`status.updated`, `contacts.updated`, `message.updated`, `call.updated`, and
`recording.updated`. `carrier.outage.alerted` reports that the manager has sent
the one-per-outage local `Viti Server down` call alert. Each event uses the
versioned envelope documented below.
`at` and `data` remain compatibility aliases for `occurred_at` and `payload`.
The in-memory buffer is bounded, so clients must refresh the corresponding
resource after a wake-up instead of treating events as permanent storage.

## Trusted service integrations

### `GET /api/v1/integrations/ws`

Upgrades to the bidirectional control WebSocket. It uses the normal manager API
key when one is configured. This endpoint is intended only for trusted services
on the same private host or VLAN; it does not add per-service credentials.

The first client message must identify one stable service name and its requested
capabilities. Capability options are preserved, including service-selected
timeouts; `-1` may represent no processing deadline where the capability using
that option permits it.

```json
{
  "type": "service.hello",
  "version": 1,
  "name": "local-spam-filter",
  "capabilities": {
    "call.screen": {"decision_timeout_ms": 2000, "processing_timeout_ms": -1},
    "call.hangup": {}
  },
  "resume": {"stream_id": "stream-...", "sequence": 42}
}
```

Names contain 1–64 letters, digits, dots, underscores, or hyphens. A second
live connection with the same name is rejected; the name becomes available as
soon as the first socket disconnects or fails its heartbeat. Protocol version 1
recognizes `call.screen`, `call.media`, `call.hangup`, `call.dtmf`, and
`call.start`, and automatically grants every advertised capability.

The server first sends `service.ready`, including `auxiliary_active`. A service
that advertises `call.media` starts with auxiliary access disabled on every new
WebSocket connection. Until it is enabled through the bot-management API, a
pure auxiliary service receives no replay, snapshot, live domain events, or
accepted commands. It still receives WebSocket pings and activation control
messages. A service that also advertises `call.screen` keeps the event stream
needed for screening independently of auxiliary activation.

For an event-enabled service, if `resume` names the current process
generation and its gap remains in the 256-event buffer, retained events follow
in sequence. Otherwise, the server sends `system.snapshot` containing current
calls, messages, known clients, connected services, contacts and their version,
recording settings, bot configuration, SIM-auth assignment, IMS registration,
and complete runtime status. Live events then use:

```json
{
  "type": "call.updated",
  "version": 1,
  "event_id": "stream-...-43",
  "stream_id": "stream-...",
  "sequence": 43,
  "occurred_at": "2026-09-11T10:00:00Z",
  "call_id": "call-...",
  "payload": {}
}
```

`stream_id` is new on every manager-process generation. `sequence` is global
within that generation and preserves live socket order. Compatible fields may
be added without changing `version`; an incompatible message shape requires a
new version. Events are not a durable outbox. A service that cannot drain its
bounded queue is disconnected so it cannot delay other consumers.

All event-enabled services receive the same domain events regardless of
capability. Presence uses `service.connected` and `service.disconnected`.
Auxiliary access changes are published as `service.activation.updated`. Logical call membership
uses `call.participant.joined` and `call.participant.left`; individual media
generations use `call.media.connected` and `call.media.disconnected`.

Commands travel over the same socket. Version 1 implements `call.start`,
`call.hangup`, `media.attach`, `media.ice`, `call.dtmf`, and
`screening.decision`.

```json
{"type":"media.attach","version":1,"command_id":"media-17","call_id":"call-...","payload":{"sdp_offer":"v=0\\r\\n...","purpose":"auxiliary"}}
{"type":"media.ice","version":1,"command_id":"ice-17","payload":{"session_id":"media-...","candidate":"candidate:...","sdp_mid":"0","sdp_mline_index":0}}
{"type":"call.dtmf","version":1,"command_id":"tone-17","call_id":"call-...","payload":{"session_id":"media-...","digit":"#","duration_ms":250}}
{"type":"call.start","version":1,"command_id":"dial-17","payload":{"to":"+34123456789","sdp_offer":"v=0\\r\\n..."}}
{"type":"call.hangup","version":1,"command_id":"stop-17","call_id":"call-..."}
{"type":"screening.decision","version":1,"command_id":"screen-17","call_id":"call-...","payload":{"revision":2,"action":"next","output":{"voice_detected":true}}}
```

`media.attach` accepts only an active carrier call and returns
`media.session.created` with the SDP answer before its completed command result.
The full-duplex offer must contain Opus, G.722, PCMU, or PCMA. One service can
own one media session per call; attaching again replaces its prior generation.
Later ICE candidates remain bound to the stable service name and exact control
connection that created the session. A service must allocate new `command_id`
values for `media.attach` and `media.ice` after reconnecting; replaying either
command from an older connection is rejected with
`error_code: "stale_connection_generation"`.

When service media is enabled, `call.start` requires the service to advertise
both `call.start` and `call.media`. The manager sends `media.session.created`
and waits up to 15 seconds for the WebRTC connection before dialing the carrier.
`media.ice` can run concurrently during that wait. Disconnecting an initiating
service ends its carrier call unless a human participant has already joined.
`call.dtmf` injects one tone through that service's conference channel; the
authoritative, source-attributed `call.dtmf` event is then broadcast to every
integration.

For incoming screening, `call.incoming` is broadcast once and each current
pipeline stage produces a `screening.offer` containing `target_service`,
`stage_index`, `revision`, `context`, `deadline`, call metadata (including
`contact_id` and `contact_name` when resolved), and prior stage results. Every
service sees the offer, but only its target can authoritatively send
`take_control`, `next`, `ring_clients`, or `delegate`. Any connected service
with `call.hangup` can send `hangup`, which has global priority. Output is opaque
JSON limited to 64 KiB per update and increments the context revision. A service
may reply early for a later stage; the manager uses that reply only if its exact
revision is still current.

`take_control` reserves the current stage. The target then sends
`media.attach` with `purpose: "controller"`; Viti answers the carrier only after
that WebRTC media is connected. The same `screening.decision` envelope finishes
an active controlling stage. Decision/media/processing timeouts, service loss,
or exhaustion fail open to the next stage or human ringing. A
`processing_timeout_ms` of `-1` has no deadline.

`delegate` is valid only for the current controlling service. It persists the
opaque output and broadcasts `screening.delegated` with `call_id`, `trace_id`,
`service_name`, `stage_index`, `revision`, and `output`. The controller remains
attached and the manager does not interpret values such as `target_service`;
another event-enabled service may independently decide to attach. The
controller may later issue `ring_clients` at the returned revision as a
fail-open fallback. Completing or abandoning that delegated stage broadcasts
`screening.delegation.ended`; targeted consumers must cancel pending and active
attachments for the call when they receive it.

The server sends `command.result` with `accepted` before executing a valid
command and then `completed`, `rejected`, or `failed`. A repeated command ID
with identical content returns its previous final in-memory result; reuse with
different content is rejected. Call-affecting commands are serialized for the
single line, and a new hang-up command against a terminal call returns
`error_code: "already_ended"`. Idempotency records and event replay disappear
on manager restart.

### `GET /api/v1/bots`

Returns the current bot-management view:

```json
{
  "configuration": {
    "revision": 4,
    "screening_pipeline": ["spam-filter", "voice-check"]
  },
  "bots": [{
    "name": "realtime-assistant",
    "connection_id": "integration-...",
    "capabilities": {"call.media": {}},
    "connected_at": "2026-09-12T10:00:00Z",
    "auxiliary_active": false
  }]
}
```

`bots` contains live WebSocket connections only. Disconnected services are not
retained. Stable names that remain in `screening_pipeline` are configuration
references and may therefore be displayed as offline stages.

### `PUT /api/v1/bots/pipeline`

Atomically replaces the ordered pipeline for future incoming calls:

```json
{"revision":4,"screening_pipeline":["voice-check","spam-filter"]}
```

`revision` must match the current configuration revision. A conflicting update
returns `409` with the current complete bot-management view. Repeating an
already-applied identical pipeline is idempotent. Names must satisfy the service
name grammar and duplicates are rejected. Calls already in screening retain
their original pipeline snapshot.

### `PUT /api/v1/bots/{name}/activation`

Enables or disables the auxiliary role for one exact live connection:

```json
{"connection_id":"integration-...","enabled":true}
```

The connection ID prevents a stale browser operation from changing a newer
connection with the same service name. Activation sends
`service.access.updated` followed by a fresh `system.snapshot`, allowing the
bot to discover and join a currently active call. Deactivation stops events,
rejects auxiliary commands, and closes non-controller WebRTC media immediately.
Activation is never persisted and every reconnect starts inactive.

## Contacts

### `GET /api/v1/contacts?q=<query>`

Returns an atomic `{"contacts":[...],"version":N}` snapshot. Search is optional
and matches contact names or telephone numbers. Each merged contact contains a
stable derived `id`, primary `name`, all `names`, all original/normalized
`numbers`, and every contributing source in `sources`. The returned `version`
always describes the exact contact content in that response.

### `PUT /api/v1/contacts/sources/{client}`

Replaces the complete address-book contribution for one trusted Android client:

```json
{
  "client_name": "OnePlus CPH2581",
  "region": "ES",
  "snapshot_revision": 1725890000000,
  "contacts": [
    {
      "source_contact_id": "42",
      "names": ["Alice Example", "Example, Alice"],
      "numbers": [
        {"original": "612 34 56 78", "normalized": "+34612345678", "label": "Mobile"}
      ]
    }
  ]
}
```

`snapshot_revision` must increase for that client. Replayed or older revisions
are accepted as no-ops, making retries safe. Legacy clients without a revision
may create a source only until a versioned snapshot for that client exists.
Successful replacements return `204`. A content change also publishes
`contacts.updated`; a revision-only retry does not.

## Messages

### `GET /api/v1/messages`

Returns `{"messages":[...]}` in descending creation order. Records include
`id`, `direction`, `from`, `to`, `text`, `protocol_debug`, `state`, `delivery_state`,
`delivery_report_requested`, `delivery_report_deadline`, `status_detail`,
`failure_code`, `failure_reason`, `parts`, `created_at`, and `updated_at`.
When the peer resolves to a synchronized contact, `contact_id` and
`contact_name` are included while the original number remains unchanged.

For outgoing messages, the fields mean:

| `state` | Evidence |
|---|---|
| `sending` | Persisted locally; submission is running. |
| `submitted` | Handed to the IMS transport; carrier acceptance is not proven. |
| `accepted` | Every segment has carrier/service-centre evidence, but delivery is not fully confirmed. |
| `delivered` | Every segment has a successful GSM status report. |
| `unknown` | Submission outcome was interrupted before it could be persisted. |
| `failed` | Local transport, RP, or terminal TP failure. |

`delivery_state` is independently `pending`, `delivered`, `failed`, or
`unknown`. `status_detail` explains the strongest current evidence. Exact RP or
TP failures are exposed in `failure_code` and `failure_reason`. Each entry in
`parts` contains its multipart index/count, engine instance, recipient, RP/TP
references, state, protocol codes, submission/service-centre timestamps, and
last update time.

`delivered` means the destination device was reported as reached. It is never a
read receipt. Carrier acceptance, HTTP success, and SIP transaction success are
not presented as delivery. Carriers that omit or delay status reports produce
an honest `unknown` state after the reporting window.

### `POST /api/v1/messages`

```json
{"to":"+34123456789","text":"Hello"}
```

The destination must be E.164 and text must contain 1–1000 Unicode characters.
The manager persists a `sending` record before returning `202`; delivery then
continues asynchronously. The response only confirms local acceptance of the
request. All connected clients observe later persisted changes through the
shared event stream and normal history refresh.

### `POST /api/v1/messages/debug/preview`

Builds the carrier payload used by the web protocol editor without sending it.
Shift-clicking **Send** in the web client opens this editor. The request accepts
the ordinary `to` and `text` fields plus optional SMSC, encoding, TP-PID,
TP-DCS, validity period, status-report flag, RP/TP/concatenation references,
SIP request URI, From URI, P-Preferred-Identity, and
P-Access-Network-Info overrides. Omitted fields use the configured carrier
defaults.

The response returns the normalized request, the selected encoding, and one
hexadecimal RP-DATA frame per segment. It also describes the SIP MESSAGE
envelope. `Via`, `Route`, `Security-Verify`, `Call-ID`, `CSeq`, `Contact`,
`Content-Length`, and `User-Agent` remain runtime-generated and are explicitly
reported as locked fields.

Set `mode` to `raw` and provide `raw_rpdus` to validate one or more complete
hexadecimal RP-DATA frames. Whitespace and colons inside the hexadecimal input
are ignored. Previewing has no carrier side effect.

### `POST /api/v1/messages/debug/send`

Accepts the same contract, validates it again, persists a message marked
`protocol_debug`, and returns `202`. For structured requests the manager
rebuilds the RPDU server-side. For raw requests Asterisk sends the validated
bytes exactly as supplied, one SIP MESSAGE per frame. The normal RP-ACK,
RP-ERROR, and delivery-report correlation still updates the shared message
record.

This endpoint requires the Asterisk AMI provider and real IMS availability.
It is intended for trusted debugging only: malformed values can be rejected by
the carrier and successful submissions may be billable SMS traffic. It never
exposes API, AMI, SIM AKA, or subscriber authentication secrets.

## Calls

### `POST /api/v1/calls/normalize`

Normalizes one public call destination without starting or persisting a call:

```json
{"number":"020 7946 0018","country":"GB"}
```

The successful response is canonical E.164:

```json
{"number":"+442079460018"}
```

`country` is an uppercase or lowercase ISO 3166-1 alpha-2 region supplied from
the current client session. It is required for national numbers and recognized
international-dialing prefixes, but ignored when `number` begins with `+`.
The server has no default country and never substitutes its own location.
Invalid numbers, emergency numbers, short codes, service codes, extensions,
pause/wait sequences, and `tel:` URIs return `400` rather than being guessed.

The endpoint requires normal client authentication. It does not receive the
IP address returned by the web country lookup, and calling it has no telephony
side effect.

### `GET /api/v1/calls`

Returns `{"calls":[...]}` in descending start order. Records include local ID,
end-to-end trace ID, engine call ID, direction, peer, lifecycle state, and start,
answer, update, and end timestamps. Resolved peers also include `contact_id` and
`contact_name`; `peer` continues to contain the telephone number.

Recorded calls also include `recording_tracks`. Each track has a stable `id`,
`role` (`remote` or `client`), human-readable device `label`, WAV content type,
source timestamp, call-relative `offset_ms`, actual WAV `duration_ms`, and
client/participant identifiers when applicable. For completed legacy records,
the manager derives missing timing from the WAV header and persists only those
track fields atomically; concurrent call or participant updates are preserved.

Calls processed by the screening pipeline include only a compact `screening`
summary in this collection response.

### `POST /api/v1/calls/test-incoming`

Schedules a local-only incoming call through the real Asterisk-to-client call
path. The optional delay gives the initiating user time to background the app
or lock the phone:

```json
{"delay_ms":2000}
```

`delay_ms` must be between 250 and 10000 and defaults to 2000. A successful
request returns `202`; a second pending test or an active call returns `409`.
The endpoint is available only with the Asterisk AMI gateway and returns `501`
for other providers. It never reaches the carrier and bypasses screening.

### `GET /api/v1/calls/{id}`

Returns one complete call record, including the snapshotted pipeline order and
the append-only per-stage screening history. The history stores service names,
context revisions, effective timeouts, actions, opaque JSON output, outcomes,
and timestamps, but never SDP, ICE, or other media transport state. Unknown IDs
return `404`.

### `GET /api/v1/recording`

Returns the server-owned preference used for new calls:

```json
{"enabled":true}
```

### `PUT /api/v1/recording`

Updates the shared preference. The change is persisted and published as a
`recording.updated` event, so every connected client converges on the same switch.

### `POST /api/v1/calls/participants`

Associates a live Asterisk participant ID with the stable client identity and
display name. Clients call this after media becomes active so microphone tracks
are labelled with the actual browser or Android device.

### `POST /api/v1/calls/participants/leave`

Records an explicit user departure independently from SIP/WebRTC transport loss:

```json
{
  "trace_id": "2bbcad44-550b-4d52-aef9-0d7f7fa78721",
  "client_id": "browser-7a9b",
  "connection_id": "connection-current",
  "previous_connection_id": "connection-admitted",
  "operation_id": "leave-10bd"
}
```

`client_id` is the stable logical device identity. `connection_id` identifies
one SIP/WebRTC attempt and must not be reused. During an in-flight recovery,
`previous_connection_id` optionally identifies the last admitted generation so
the user can still leave before the replacement admission completes.

The successful response contains the same `operation_id` and `carrier_ended`.
The latter is true only when this operation removed the last logical participant.
If the carrier call has already ended, the response also contains
`"already_ended":true`. An active-call request returns `409` if neither supplied
generation owns that participant or if an operation ID is reused with different
arguments. Gateway/AMI failure returns `502`; retry the exact same body and
operation ID. Completed retries are idempotent and do not act on a later leg.

### `GET /api/v1/calls/{id}/recording?tracks=<id,id>`

Streams a lossless PCM WAV containing the selected tracks aligned and mixed by
the manager on the call's common start/end timeline. Silence preserves the real
position of a participant that joins late or leaves early, including when that
participant is the only selected track. Omitting `tracks` selects every available
track. Add `download=1` for a `Content-Disposition: attachment` response.
Unknown, incomplete, or still-open track files return a non-success response
rather than partial audio.

### `POST /api/v1/calls`

This endpoint is available to non-interactive telephony adapters. The current
Asterisk/WebRTC adapter requires clients to place interactive calls through SIP
WebSocket, so normal web and Android callers do not use it. Its request accepts
`{"to":"+442079460018"}` for existing adapters, or an optional transient
`country` beside a non-canonical `to`. The manager applies the same normalizer
before persistence or gateway dispatch. Missing/ambiguous country evidence is
rejected; legacy canonical E.164 requests remain compatible.

### `POST /api/v1/calls/{id}/hangup`

Requests termination through the configured gateway and finalizes the matching
history row. Live WebRTC clients normally send SIP BYE directly; the endpoint
exists for manager-controlled adapters and recovery tooling.

### `GET /api/v1/voice`

Returns ephemeral client provisioning: SIP WebSocket URL, domain, local SIP
username/password, and ICE servers. If no explicit public URL is configured,
the manager derives it from the HTTP request host and scheme.

### `GET /sipws`

WebSocket upgrade endpoint proxied to the engine's Asterisk HTTP/WebSocket
listener. The subprotocol is SIP. This is not a JSON endpoint.

## Remote SIM authentication

### `POST /api/v1/authenticate`

Engine-facing AKA request authenticated with the API key:

```json
{"rand":"ABEiM0RVZneImaq7zN3u_w","autn":"qrvM3e7_ABEiM0RVZneImQ"}
```

RAND and AUTN are 16-byte, unpadded Base64 values. A successful response
contains unpadded Base64 `res`, `ck`, and `ik`. A synchronization response may
contain `auts`. The manager dispatches to the active Android agent or local
fallback.

### `GET /api/v1/auth/agents/{agent}/challenges`

Long-polls the next pending AKA challenge for one Android foreground agent.
Only the assigned agent receives work.

### `POST /api/v1/auth/agents/{agent}/challenges/{id}/response`

Completes a challenge with a successful AKA result, synchronization failure,
or an error. A response is accepted only once and only for the matching agent.

## Diagnostics

### `POST /api/v1/diagnostics/events`

Accepts timestamped client diagnostic events. Web and Android use this for UI
actions, SIP/SDP observations, ICE transitions, WebRTC statistics, media state,
and errors. The endpoint writes only while `VITI_DIAGNOSTICS_ENABLED` is
explicitly true. A diagnostic directory by itself leaves it as a no-op.

## Engine webhook

### `POST /api/engine/event`

Authenticated with `X-Viti-Engine-Token`, not the client API key. The payload
contains `instance`, `event`, optional call/trace IDs, argument strings, and an
optional nanosecond source timestamp. Supported authoritative lifecycle events
include:

- `tunnel_start`, `tunnel_up`, `tunnel_down`;
- `sms_in`;
- `call_in`, `call_out`, `call_active`, `call_result`;
- `participant_joined`, a synchronous fail-closed admission request carrying
  `[client_id, participant_id, connection_id, channel]`;
- `participant_join_aborted`, the durable compensation when Asterisk did not
  receive a successful admission response;
- `participant_left`, a durable observation that one admitted media channel
  disappeared unexpectedly or normally, but not evidence of explicit user intent;
- `recording_track`, which attaches a validated recording storage key and
  source timestamp to the correlated call.

Tunnel events carry a process generation and monotonic sequence. An unseen
generation may claim the current data plane only when its source timestamp is
newer than the persisted checkpoint. The latest causal down boundary remains
persisted after a later up event and across manager restart.

The manager atomically correlates call ID and engine instance, applies lifecycle
or tunnel reconciliation, and replaces the state file. Source timestamps, not
HTTP arrival order, determine lifecycle chronology. When local outbox storage is
writable, all four call lifecycle hooks are committed before delivery is
attempted, making network and manager failures retry-safe. A local outbox write
failure instead uses a direct best-effort HTTP fallback.

A private `call_active` or `call_result` event may omit its peer when the call ID
already exists. If its matching start event is not yet durable, the manager
returns 409 and the outbox retains it for retry. Informational engine events are
accepted without creating history rows.

`participant_joined` is deliberately not queued or replayed: its Asterisk
channel is blocked waiting for the response, and only a synchronous `204` may
enter ConfBridge. A missing, transport-error, or non-2xx response fails closed;
Asterisk then durably queues `participant_join_aborted`. The manager correlates
client, connection generation, exact Asterisk channel, source timestamp, and
predecessor history so delayed abort/leave events cannot revoke a newer leg.
`410` rejects a terminal, reused, colliding, or otherwise stale admission; an
exact replay of the still-current generation remains idempotent.

### `POST /api/engine/screen`

Engine-only long-poll authenticated with `X-Viti-Engine-Token`. The engine sends
its `instance` and carrier `call_id`; the manager returns `ring_clients`,
`take_control`, `hangup`, or `pending`. A pending response is a two-second
liveness heartbeat, not a pipeline timeout. The engine repeats the request with
a five-second socket deadline, allowing arbitrarily long aggregate service
deadlines while a wedged manager path still fails open to human ringing.

### `GET /api/engine/recording`

Engine-only endpoint authenticated with `X-Viti-Engine-Token`. Asterisk checks
this shared setting when each carrier or local participant channel starts and
fails closed if the manager cannot be reached.

## Restricted Android signaling probe

`POST /api/v1/diagnostics/events` also accepts `source: "android-signaling"`,
`kind: "signaling.snapshot"`, and an allowlisted `data` object even when the
full development recorder is disabled. This probe is separate from unredacted
diagnostic capture. Invalid client hashes or runtime session UUIDs return 400.
Unknown fields, raw causes, and arbitrary string values are not retained.

`GET /api/v1/diagnostics/voice` requires the normal client API authentication
and returns `{"snapshots": [...]}` with `Cache-Control: no-store`. An optional
`client_hash` query selects one pseudonymous client. The in-memory buffer holds
at most 256 snapshots across clients; records older than six hours are omitted.
A manager restart clears it. There is no diagnostic download on a public port.

Snapshots contain server receipt time, a hashed client ID, a random service
runtime session, a fixed build marker, native tick/JavaScript dispatch counts,
service uptime, age of the last JavaScript acknowledgement and signaling event,
remaining SIP lease, process importance, WebView attachment/visibility/readiness,
boolean API/registration/wake-lock states, and native socket frame counters/state.
The `native-voice-20261007` build uses native dispatch counters and reports
`webview_owned: false`; JavaScript/attachment fields apply only to older builds.
Only fixed signaling categories are accepted. There are no phone numbers, audio, SIP payloads, network addresses,
credentials, or free-form exception messages.

## Compatibility policy

JSON field names and enum values are wire-level contracts and remain English,
lowercase, and stable. New optional fields may be added. Clients must ignore
unknown response fields and must not infer line health from HTTP liveness alone.
