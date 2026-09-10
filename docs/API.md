# HTTP API

The manager serves JSON under `/api/v1`, the web application at `/`, a SIP
WebSocket proxy at `/sipws`, and one engine-only webhook at
`/api/engine/event`.

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

Phone destinations use E.164 form. Timestamps are UTC RFC 3339 values. Unknown
JSON fields are rejected and request bodies have endpoint-specific size limits.

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
`recording.updated`. The in-memory buffer is bounded, so clients must refresh
the corresponding resource after a wake-up instead of treating events as
permanent storage.

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
WebSocket, so normal web and Android callers do not use it.

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

- `tunnel_up`, `tunnel_down`;
- `sms_in`;
- `call_in`, `call_out`, `call_active`, `call_result`;
- `recording_track`, which attaches a validated recording storage key and
  source timestamp to the correlated call.

### `GET /api/engine/recording`

Engine-only endpoint authenticated with `X-Viti-Engine-Token`. Asterisk checks
this shared setting when each carrier or local participant channel starts and
fails closed if the manager cannot be reached.

The manager correlates events by engine call ID and instance, persists the
result, and publishes a client event. Informational engine events are accepted
without creating history rows.

## Compatibility policy

JSON field names and enum values are wire-level contracts and remain English,
lowercase, and stable. New optional fields may be added. Clients must ignore
unknown response fields and must not infer line health from HTTP liveness alone.
