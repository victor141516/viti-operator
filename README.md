# Viti Operator

A self-hosted calling and SMS service using a physical SIM for authentication,
with shared web and Android clients over Tailscale. The Go manager owns history,
recordings and client state. The bundled Asterisk/VoWiFi engine handles ePDG,
IMS and conference audio.

## Features

- Shared SMS and call history across web and Android.
- Incoming calls ring connected clients; additional clients can join the active call.
- Per-participant audio mixing and independent departure.
- Client-local microphone mute on web and Android; other participants and
  incoming audio remain unaffected.
- Dynamic SIM authentication through a remote Android agent or configured USB fallback.
- Shared connection topology with live device presence.
- Optional lossless call recordings with independently selectable microphone tracks,
  a seekable timeline, 1x/1.5x/2x playback and selected-track WAV downloads.

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

The current implementation ends the carrier leg when its last participant leaves
and may reconcile calls when SIM availability is lost. Preserving calls through
unexpected client or SIM disconnections is tracked as upcoming work in GitHub issues.

## Source scope

This is a clean source snapshot with a new Git history. It includes reusable
application, engine, tests, configuration templates and opt-in diagnostics code.
It excludes deployment credentials, local configuration, raw traces, recordings,
screenshots, APKs, modem recovery/flashing tools and workstation-specific scripts.
The original local histories are not included. The three recovered stock-Android
bridge binaries are distributed separately as component release assets, not in Git.
