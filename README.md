# Viti Operator

A self-hosted calling and SMS service using a physical SIM for authentication,
with shared web and Android clients over Tailscale. The Go manager owns history,
recordings and client state. The bundled Asterisk/VoWiFi engine handles ePDG,
IMS and conference audio.

## Features

- Shared SMS and call history across web and Android, with carrier acceptance,
  destination delivery, failure, and honest unconfirmed SMS states.
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
  with stale actions constrained to the exact current service leg.
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
  source; new operations remain unavailable until fresh AKA can be served.
- Shared connection topology with live device presence, selected SIM-auth source,
  and evidence-backed Android carrier, registration, access-technology, and
  signal details when the OS exposes them.
- Optional lossless call recordings with independently selectable microphone tracks,
  a seekable timeline, participant-presence indicators, 1x/1.5x/2x playback and
  selected-track WAV downloads.

## Setup

Copy `.env.example` to `.env`, `deploy/viti.example.json` to `deploy/viti.json`
and `deploy/instance.example.json` to `deploy/instance.json`.
Replace every credential and line/device placeholder with your own values.
Example subscriber identifiers are synthetic and must not be used as real identities.
Set `VITI_BIND_IP` to this host's Tailscale IP and `VITI_DIAGNOSTICS_DIR` to an
absolute directory outside the checkout. Diagnostics are disabled by default.

With Docker Compose, Tailscale and `/dev/net/tun` available:

```sh
docker compose up --build -d
```

Containers listen on all internal interfaces. Compose limits published ports
to the configured host address. Open the remote web/API at
`http://<VITI_BIND_IP>:18787`, or the local web at `http://127.0.0.1:18788`.
The private TURN service uses UDP; its relay ports stay inside the Docker network.

## SIM prerequisites

The carrier must support the required Wi-Fi Calling/IMS flows. Android remote
authentication requires a compatible device and the documented Shizuku setup.
For the USB adapter, first install the dedicated
[OpenStick Stock Android Bridge Binaries v1.0.0 component release](https://github.com/victor141516/viti-operator/releases/tag/openstick-android-bridge-v1.0.0).
Download and extract its ZIP, then run `python install.py --check` followed by
`python install.py` with Python 3.10+ and ADB installed. See the
[bridge setup and compatibility guide](tools/openstick-android-bridge/README.md).
The historical `openstick` adapter name refers here to HiMI/UFI hardware running
the verified **stock Android 4.4.4 firmware**, not the Linux OpenStick OS.

The release contains the recovered privileged APK and two native helper binaries,
plus their installer. Their original source is currently unavailable; recovering
or reimplementing it remains pending. No firmware or private device data is shipped.
The installer rejects unvalidated firmware/ordinary phones and does not flash or
root devices. Remote Android, command adapters and optional PC/SC remain alternative
SIM authentication paths.

## Development

```sh
npm --prefix web-src ci
npm --prefix web-src test
npm --prefix web-src run build
go test ./...
go build ./cmd/viti-operator
```

Build Android with JDK 17, Android SDK 36 and the included Gradle wrapper:

```sh
cd android
./gradlew testDebugUnitTest assembleDebug
```

The APK is generated at `android/app/build/outputs/apk/debug/app-debug.apk`.
Generated APKs, credentials, call recordings, SMS contents and diagnostics are not
source assets and must not be committed.

## Documentation

- [Architecture and protocols](docs/ARCHITECTURE.md)
- [HTTP API](docs/API.md)
- [Operations and deployment](docs/OPERATIONS.md)
- [Third-party source provenance](THIRD_PARTY_NOTICES.md)

The carrier leg ends when its last logical participant explicitly leaves or when
the carrier or engine reports a real terminal transport event. Losing a browser,
Android network, WebRTC leg, SIM reader, or readiness probe is not itself an
explicit hangup; a confirmed current tunnel-down boundary remains terminal.
Clients retain logical identity and rejoin the existing conference when media
returns. This control state is volatile: surviving a manager or engine restart
during a call is not promised.

## Source scope

This is a clean source snapshot with a new Git history. It includes reusable
application, engine, tests, configuration templates and opt-in diagnostics code.
It excludes deployment credentials, local configuration, raw traces, recordings,
screenshots, APKs, modem recovery/flashing tools and workstation-specific scripts.
The original local histories are not included. The three recovered stock-Android
bridge binaries are distributed separately as component release assets, not in Git.
