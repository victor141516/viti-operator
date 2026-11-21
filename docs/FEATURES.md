# Features

The complete feature list. The [README](../README.md) carries a short summary.

- Shared SMS and call history across web and Android, with carrier acceptance,
  destination delivery, failure, and honest unconfirmed SMS states.
- A replayable bidirectional WebSocket for trusted local services to observe all
  system events, advertise call capabilities, and issue idempotent commands.
- Call-scoped full-duplex WebRTC attachments for trusted services, using
  ephemeral manager-owned sessions and the existing Asterisk conference mix.
- An ordered, fail-open incoming-call screening pipeline whose trusted services
  can inspect metadata, take WebRTC control, append JSON results, hand off to
  another stage, ring human clients, or reject the call.
- An optional OpenAI Realtime auxiliary participant that joins active calls,
  passes PCMU audio through without transcoding, and never owns call lifecycle.
- An optional contact-aware commercial-call screener that plays a local greeting,
  classifies caller speech with text-only Realtime output, and delegates detected
  sales calls to a separately activated conversational handler while failing open
  to human ringing. It passes callers resolved from synchronized contacts to web
  and Android clients before taking control or playing media.
- Explicitly authorized Android contact synchronization with cross-device name
  resolution, phone-number deduplication and source-device provenance.
- Incoming calls ring connected clients; additional clients can join the active call.
- Established carrier calls remain alive when one or every local WebRTC leg is
  temporarily lost. Web and Android recreate media against the same conference
  without redialing, while an explicit final participant departure still ends it.
- National-format calling from web, Android, and Android Auto with per-session
  country detection, established numbering rules, and a fail-closed international
  fallback when the current country is ambiguous.
- Per-participant audio mixing and independent departure.
- Client-local microphone mute on web and Android; other participants and
  incoming audio remain unaffected.
- Android service calls integrate with the system Telecom concurrency and audio
  model without replacing the default dialer, with a capability fallback for
  Android 7 devices.
- Native incoming-call controls use Android CallStyle notifications, an
  eligible lock-screen surface, and the Android-managed heads-up fallback,
  with stale actions constrained to the exact current service leg. A delayed,
  local-only test call can exercise the same presentation path without the SIM.
- Compatible phones can project a bounded recent/contact call directory to
  Android Auto; active calls use Android Auto's system in-call surface and car
  audio controls. This beta capability currently has restricted test-only
  distribution.
- Browser-local microphone and speaker preferences with safe default fallback
  and automatic restoration when a remembered device reconnects.
- Expandable live WebRTC quality details on web and Android, including
  direction-specific packet loss, incoming jitter, scoped RTT, and the observed
  direct or TURN media route with its transport protocol.
- Dynamic SIM authentication through a remote Android agent or configured USB fallback.
- Established carrier sessions survive temporary loss of the SIM authentication
  source. Calls and SMS remain available while SWu and IMS stay operational;
  fresh authentication waits for the SIM to return.
- A sustained SWu or IMS outage repeatedly rings registered clients with
  server-originated local calls labelled `Viti Server down`, without using the
  carrier line, until recovery. Real calls take priority over these alerts.
- The Android foreground voice service checks both local voice registration
  and carrier readiness, even while the UI is suspended. After a reconnection
  grace period, losing its own server connection starts a persistent local
  `No connection` warning. Carrier-only outages update its status text but
  ring exclusively through the server-originated calls.
- Shared connection topology with live device presence, selected SIM-auth source,
  and evidence-backed Android carrier, registration, access-technology, and
  signal details when the OS exposes them.
- Optional lossless call recordings with independently selectable microphone tracks,
  a seekable timeline, participant-presence indicators, 1x/1.5x/2x playback and
  selected-track WAV downloads.
