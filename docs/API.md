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
`provider`, `detail`, and `updated_at`. `ready` means the complete line is
available. `contacts_version` is the persisted version of the merged contact
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

## Events

### `GET /api/v1/events?after=<sequence>`

Long-polls for events newer than the supplied sequence. Common event types are
`status.updated`, `contacts.updated`, `message.updated`, `call.updated`, and
`recording.updated`. Each event uses the versioned envelope documented below.
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

The server first sends `service.ready`. If `resume` names the current process
generation and its gap remains in the 256-event buffer, retained events follow
in sequence. Otherwise, the server sends `system.snapshot` containing current
calls, messages, known clients, and connected services. Live events then use:

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

All connected services receive all events regardless of capability. Presence
uses `service.connected` and `service.disconnected`. Logical call membership
uses `call.participant.joined` and `call.participant.left`; individual media
generations use `call.media.connected` and `call.media.disconnected`.

Commands travel over the same socket. Version 1 implements outgoing
`call.start` and `call.hangup`; the other advertised capabilities are protocol
extension points for the media and screening APIs.

```json
{"type":"call.start","version":1,"command_id":"dial-17","payload":{"to":"+34123456789"}}
{"type":"call.hangup","version":1,"command_id":"stop-17","call_id":"call-..."}
```

The server sends `command.result` with `accepted` before executing a valid
command and then `completed`, `rejected`, or `failed`. A repeated command ID
with identical content returns its previous final in-memory result; reuse with
different content is rejected. Call-affecting commands are serialized for the
single line, and a new hang-up command against a terminal call returns
`error_code: "already_ended"`. Idempotency records and event replay disappear
on manager restart.

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
`id`, `direction`, `from`, `to`, `text`, `state`, `delivery_state`,
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

### `GET /api/engine/recording`

Engine-only endpoint authenticated with `X-Viti-Engine-Token`. Asterisk checks
this shared setting when each carrier or local participant channel starts and
fails closed if the manager cannot be reached.

## Compatibility policy

JSON field names and enum values are wire-level contracts and remain English,
lowercase, and stable. New optional fields may be added. Clients must ignore
unknown response fields and must not infer line health from HTTP liveness alone.
