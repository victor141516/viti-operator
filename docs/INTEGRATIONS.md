# Trusted service integration protocol

This document is the complete protocol guide for trusted automation services.
The HTTP resource reference remains in [API.md](API.md); this guide focuses on
the bidirectional event, command, media, and screening lifecycles.

## Trust and transport

Services connect to `GET /api/v1/integrations/ws` with the manager API key in
`Authorization: Bearer <key>` or `X-API-Key`. The endpoint is intended for
trusted processes on the same host, private container network, tailnet, or VLAN.
There are no per-service credentials or approval prompts. Do not expose it to an
untrusted network.

One WebSocket carries control messages and JSON events. Each attached call has
an independent WebRTC peer connection, so control traffic is not blocked by
media and a service can replace one call's media generation without affecting
another. The manager uses one UDP ICE-mux listener for all service peers; adding
services does not consume one listening port per WebSocket or per call.

## Registration

The first message must be `service.hello`:

```json
{
  "type": "service.hello",
  "version": 1,
  "name": "example-service",
  "capabilities": {
    "call.media": {},
    "call.hangup": {}
  },
  "resume": {"stream_id": "stream-generation", "sequence": 42}
}
```

The name is a stable 1–64 character identifier containing letters, digits,
dots, underscores, or hyphens. Only one live connection may own a name. All
advertised version-1 capabilities are granted because every service inside the
trust boundary is considered trusted.

Capabilities govern commands, not event visibility:

| Capability | Commands enabled |
|---|---|
| `call.media` | `media.attach`, `media.ice` |
| `call.screen` | `screening.decision` for the targeted pipeline stage |
| `call.hangup` | `call.hangup` and global-priority screening `hangup` |
| `call.dtmf` | `call.dtmf` |
| `call.start` | `call.start`; also requires `call.media` while service media is enabled |

`call.screen` accepts these options:

```json
{
  "decision_timeout_ms": 2000,
  "media_timeout_ms": 15000,
  "processing_timeout_ms": -1
}
```

Decision and media deadlines must be positive and finite. A processing timeout
may be `-1` for no deadline. The service, not the manager, chooses values that
fit its work.

The manager replies with `service.ready`, including the connection ID, granted
capabilities, stream generation, latest sequence, and `auxiliary_active`. An
invalid hello receives `service.error` and a policy-violation close. The manager
sends WebSocket pings every 30 seconds and closes a connection that does not
answer for 75 seconds.

A service advertising `call.media` starts inactive on every connection. It is
visible in `GET /api/v1/bots`, but a pure auxiliary bot receives no event replay,
snapshot, live domain event, accepted command, or media until enabled through
`PUT /api/v1/bots/{name}/activation`. Activation is ephemeral: reconnecting
always starts inactive. Enabling sends the direct `service.access.updated`
control message and a fresh
`system.snapshot`; disabling closes non-controller media immediately. A service
that also advertises `call.screen` continues receiving screening events and may
use controller media independently of its auxiliary activation state.

## Replay, snapshots, and event order

Every manager process has a new `stream_id` and a single increasing `sequence`.
The last 256 events are retained in memory. Registration is atomic with the live
subscription, so a valid resume cannot lose an event between replay and live
delivery.

If the resume generation and sequence are still available, replayed events are
sent after `service.ready`. Otherwise `system.snapshot` is sent with calls,
messages, known clients, connected services, contacts and their version,
recording settings, bot configuration, SIM-auth assignment, IMS registration,
and runtime status. Media transports and in-flight command deduplication are
never restored from a snapshot.

Live events use this envelope:

```json
{
  "type": "call.updated",
  "version": 1,
  "event_id": "stream-generation-43",
  "stream_id": "stream-generation",
  "sequence": 43,
  "occurred_at": "2026-09-11T10:00:00Z",
  "call_id": "call-id",
  "payload": {}
}
```

`at` and `data` are compatibility aliases for `occurred_at` and `payload`.
Compatible fields may be added to version 1. Consumers must ignore unknown
fields. A slow consumer whose bounded output queue fills is disconnected rather
than delaying the manager or other services.

### Event catalogue

Every event-enabled service receives every event:

| Event | Payload |
|---|---|
| `status.updated` | Changed runtime or IMS status data; refresh `/api/v1/status` for a complete view |
| `contacts.updated` | Source client and replacement count; refresh `/api/v1/contacts` |
| `recording.updated` | Current shared recording setting |
| `message.received` | Complete new inbound message record |
| `message.updated` | Complete changed message record |
| `call.incoming` | Compact incoming-call metadata before answer |
| `call.active` | Compact call metadata after carrier answer |
| `call.updated` | Complete changed call record, including terminal `ended_at` |
| `call.participant.joined` / `call.participant.left` | Logical human or service membership and source identifiers |
| `call.media.connected` / `call.media.disconnected` | One concrete media generation and its source identifiers |
| `call.dtmf` | Digit, duration, source role/name, and source media session when known |
| `service.connected` / `service.disconnected` | Stable name, connection generation, capabilities, and connection time |
| `service.activation.updated` | Current ephemeral auxiliary activation state for a live service connection |
| `screening.pipeline.updated` | Persisted pipeline names and new configuration revision |
| `screening.offer` | Target service, stage, revision, context, deadline, call metadata, and prior outputs |
| `screening.delegated` | A controlling screener's arbitrary output, broadcast for another service to consume |
| `screening.delegation.ended` | The call, controller, revision, and outcome that revoke an earlier delegation |
| `screening.action.failed` | A carrier action that failed after an accepted screening decision |
| `screening.persistence.failed` | Screening audit data that could not be stored |
| `sim-auth.assignment.updated` | Current SIM-auth assignment |

Participant membership and media connectivity are deliberately separate. A
temporary ICE disconnect does not mean the logical participant left, and a
replacement peer connection creates a new media generation.

## Commands and idempotency

Commands use this common envelope:

```json
{
  "type": "call.hangup",
  "version": 1,
  "command_id": "unique-operation-id",
  "call_id": "call-id",
  "payload": {}
}
```

The manager first emits `command.result` with `status: "accepted"`, then a final
`completed`, `rejected`, or `failed` result. `rejected` means the request or
current state did not permit execution. `failed` means execution was attempted
but did not complete. Final results may contain `error_code`, `message`, and
`payload`.

Within one manager generation, repeating an identical command ID for the same
stable service returns the cached final result. Reusing it with different
content is rejected. `media.attach` and `media.ice` IDs are additionally fenced
to the exact control-connection generation and must never be replayed after a
reconnect. Command IDs and their cache are process-local.

Version 1 commands are:

- `call.start`: E.164 destination plus a full-duplex SDP offer. The manager
  creates media first and dials only after WebRTC connects.
- `call.hangup`: terminally ends the named open call.
- `media.attach`: attaches full-duplex media to an existing call.
- `media.ice`: adds a later ICE candidate to the exact media session.
- `call.dtmf`: injects one RFC 4733 digit through the service's conference leg.
- `screening.decision`: advances or terminates a screening stage.

Call lifecycle commands are serialized because the deployment controls one
physical line.

## Auxiliary media

An auxiliary participant observes `call.active` and then creates its own WebRTC
offer. The offer must contain full-duplex audio using at least one of Opus,
G.722, PCMU, or PCMA:

```json
{
  "type": "media.attach",
  "version": 1,
  "command_id": "attach-1",
  "call_id": "call-id",
  "payload": {
    "sdp_offer": "v=0\r\n...",
    "purpose": "auxiliary"
  }
}
```

The manager sends `media.session.created` and also includes the same
`session_id`, `call_id`, `sdp_answer`, and `purpose` in the completed command
result. Apply the answer to the peer connection. If the offer uses trickle ICE,
send subsequent candidates with:

```json
{
  "type": "media.ice",
  "version": 1,
  "command_id": "candidate-1",
  "payload": {
    "session_id": "media-session-id",
    "candidate": "candidate:...",
    "sdp_mid": "0",
    "sdp_mline_index": 0
  }
}
```

The manager selects the first mutually supported codec in offer order and asks
Asterisk to use the same codec. Each stable service owns at most one media
session per call; another attachment replaces it. Closing the control socket or
ending the call closes all matching service media.

The Asterisk conference provides mix-minus: a service hears the carrier and all
other participants, but not its own injected audio. Auxiliary services can hear
and speak and may also advertise `call.hangup` or `call.dtmf`. They do not own
the call, do not delay human hangup, and cannot keep the carrier leg alive.

DTMF injection uses:

```json
{
  "type": "call.dtmf",
  "version": 1,
  "command_id": "tone-1",
  "call_id": "call-id",
  "payload": {"session_id": "media-session-id", "digit": "#", "duration_ms": 250}
}
```

The digit is one of `0`–`9`, `*`, `#`, or `A`–`D`; duration is 40–6000 ms and
defaults to 250 ms. The resulting source-attributed `call.dtmf` event is sent to
every integration, and the tone is delivered to conference participants.

## Screening pipeline

`VITI_SCREENING_PIPELINE` is an ordered comma-separated list of stable service
names. The manager snapshots that order and stores it with the call. A stage
that is not connected with `call.screen` when reached is skipped. No service
chooses pipeline order at runtime.

For an unanswered incoming call:

1. `call.incoming` is broadcast once.
2. The manager broadcasts `screening.offer` for the current stage and revision.
3. Any stage may precompute a response, but only the target stage's decision is
   authoritative for `take_control`, `next`, `ring_clients`, or `delegate` at
   that revision.
4. `next` appends optional JSON output, increments the revision, and emits a new
   offer to the next available stage while the carrier remains unanswered.
5. `take_control` reserves the stage. That service attaches with
   `purpose: "controller"`; the carrier is answered only after WebRTC connects.
6. The controlling stage later uses the same decision command with `next`,
   `ring_clients`, `delegate`, or `hangup`. Later stages receive
   `context: "active"`.

A decision is:

```json
{
  "type": "screening.decision",
  "version": 1,
  "command_id": "decision-1",
  "call_id": "call-id",
  "payload": {
    "revision": 2,
    "action": "next",
    "output": {"classification": "allowed", "confidence": 0.91}
  }
}
```

Actions are:

- `take_control`: current target reserves and then attaches controller media;
- `next`: finish the current stage and offer the next stage;
- `ring_clients`: stop screening and ring human clients immediately;
- `delegate`: keep the current controller attached and broadcast a
  `screening.delegated` event containing the stage's opaque `output`; Viti does
  not interpret routing fields in that JSON;
- `hangup`: end the carrier call. Any connected service with `call.hangup` may
  send this at the current revision, and it has priority over other replies.

`output` is arbitrary valid JSON up to 64 KiB. Each update is stored with its
service name, action, revision, and timestamp, so later stages can distinguish
the producer of every result. The pipeline snapshot and history are available
from `GET /api/v1/calls/{id}`; collection responses expose only a compact
screening summary. SDP, ICE, and media generations are never persisted.

Unavailable services, decision/media/processing timeout, controller loss, and
pipeline exhaustion all fail open toward the next stage and ultimately human
ringing. If a manager restart makes ownership ambiguous, it does not reconstruct
the pipeline and rings humans. Once a human joins, that human or carrier hangup
is final even if auxiliary services remain.

## Bundled OpenAI Realtime auxiliary bot

`viti-openai-realtime-bot` is a reference auxiliary service and the Compose
deployment includes it as `openai-realtime-bot`. It advertises only
`call.media`, joins each `call.active`, and creates one OpenAI Realtime session
per active call.

The bot offers PCMU, so call audio is forwarded as raw G.711 μ-law bytes to
OpenAI and returned to Viti without decode/resample/re-encode. Input RTP is
batched into 100 ms WebSocket messages. Output is paced back into WebRTC in
20 ms frames with a bounded queue. Server VAD creates responses and permits
barge-in; when speech starts, queued model audio is discarded and the played
position is reported with `conversation.item.truncate`.

The bot completes the Viti media attachment before opening a billable Realtime
session. Audio received during the short Realtime handshake is held in a bounded
memory queue, so an Asterisk attachment failure consumes no OpenAI session and
the caller's first words are still delivered after setup.

The OpenAI WebSocket exists only while the bot owns an attached media session
for an active call. Starting the bot without a call does not contact OpenAI.
Terminal call updates, Viti media closure, control-connection loss, process
shutdown, and `service.access.updated` deactivation all cancel the call context
and close the OpenAI WebSocket immediately, including during its initial
handshake. Reactivation creates a new Realtime session only if an active call is
then available.

Set `VITI_OPENAI_JOIN_MODE=targeted` to make an instance ignore ordinary
`call.active` events and join only after a `screening.delegated` event whose
`output.target_service` exactly matches its stable service name. This is still
ordinary event-driven behavior: the manager broadcasts the event and does not
select or start the recipient.

## Bundled commercial-call screener

`viti-commercial-screening-bot` is a fail-open screening service. A call whose
resolved `contact_id` is non-empty is sent directly to human ringing without
being answered. For an unknown caller, the bot takes controller media, answers
the carrier, plays `/bot-media/hello.wav`, and sends only caller audio to an
OpenAI Realtime session configured for text output. It does not request or play
model-generated speech and does not store or log transcripts.

The classifier returns `commercial`, `non_commercial`, or `uncertain`.
Uncertain utterances keep the classifier listening. A confident non-commercial
result rings humans. A commercial result uses `delegate` with opaque output:

```json
{
  "classification": "commercial",
  "commercial": true,
  "reason": "The caller is offering an energy contract",
  "target_service": "openai-commercial-handler"
}
```

The separate targeted Realtime bot recognizes its own stable name, attaches as
an auxiliary participant, and talks to the caller. The screener waits for that
participant to join and continues watching it; departure revokes the delegation
and rings humans. `screening.delegation.ended` also cancels a late or retrying
targeted attachment. A missing greeting, missing API key, media or classifier
failure, classification timeout, or target join timeout always falls back to
human ringing. The greeting is loaded for each call, so replacing the file does
not require rebuilding the image.

Configuration is entirely through environment variables documented in
[OPERATIONS.md](OPERATIONS.md). `OPENAI_API_KEY` is never stored in manager
state. When it is absent, the process stays idle and makes no Viti or OpenAI
connection. The bot logs lifecycle and error metadata but not audio or
transcripts.

## Failure boundaries

- Event replay, command results, screening coordination, and media ownership are
  process-local unless explicitly described as call-history audit data.
- A control WebSocket loss closes its media sessions. Reconnect with a fresh
  hello, obtain replay or snapshot, and perform a new SDP exchange.
- Services must treat terminal `call.updated` data and peer closure as final for
  that call generation.
- Optional service failure never blocks the normal human call path. Screening
  failures advance; auxiliary failures remove only that participant.
- Do not log or persist SDP, ICE addresses, raw audio, transcripts, telephone
  numbers, or API keys unless a separately reviewed feature explicitly requires
  it.
