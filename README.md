<p align="center">
  <img src="assets/icon/viti-operator-icon.png" alt="Viti Operator icon" width="128" height="128">
</p>

<h1 align="center">Viti Operator</h1>

<p align="center">
  <strong>Your SIM's calls and SMS, on every device you own.</strong>
</p>

<p align="center">
  A self-hosted calling and SMS service using a physical SIM for authentication, with shared web and Android clients over Tailscale.
</p>

## One Line, Many Devices

A mobile line lives in one phone. Viti Operator authenticates with the physical SIM, carries its
calls and SMS over the carrier's Wi-Fi Calling infrastructure, and exposes the line to your
browsers, Android phones and trusted services with one shared history.

## Features

- 💬 Shared history. SMS and calls are synchronized across web and Android, with carrier acceptance, delivery, failure and unconfirmed SMS states.
- 📞 Group calls. Incoming calls ring connected clients, additional clients can join the active call, and each participant leaves independently.
- 🔁 Resilient. Established calls survive temporary loss of a WebRTC leg or of the SIM authentication source; clients rejoin without redialing.
- 🤖 Integrations. Trusted services observe events, issue commands, attach WebRTC audio and screen incoming calls in an ordered, fail-open pipeline.
- 📱 Android native. Telecom integration, CallStyle notifications, contact synchronization and a beta Android Auto call directory.
- 🎙️ Recordings. Optional lossless recordings with selectable microphone tracks, a seekable timeline and WAV downloads.
- 🚨 Outage alerts. A sustained SWu or IMS outage rings registered clients with local calls labelled `Viti Server down`.

The complete list is in [docs/FEATURES.md](docs/FEATURES.md).

## Use It

| Piece | Where |
| --- | --- |
| Web client, on the server | `http://127.0.0.1:18788` |
| Web client and API, over Tailscale | `http://<VITI_BIND_IP>:18787` |
| Android app | Install the APK, connect Tailscale, enter the server URL and API key |
| HTTP API | `/api/v1/...`, see [docs/API.md](docs/API.md) |
| Trusted-service WebSocket | `GET /api/v1/integrations/ws`, see [docs/INTEGRATIONS.md](docs/INTEGRATIONS.md) |
| Health and line status | `GET /api/v1/health`, `GET /api/v1/status` |

Remote API calls need the configured credential (`Authorization: Bearer ...` or `X-API-Key`);
the loopback proxy on port 18788 injects it. Browser calling from another machine needs an HTTPS
reverse proxy: remote plain HTTP is not a microphone secure context. The bundled OpenAI Realtime
bot is idle until `OPENAI_API_KEY` is added to the untracked `.env` file.

## Install

There is no prebuilt server image or published client APK: the service is built and run from
source with Docker Compose. You need Tailscale, `/dev/net/tun`, and a SIM whose carrier supports
the required Wi-Fi Calling/IMS flows; see [docs/SIM-PREREQUISITES.md](docs/SIM-PREREQUISITES.md).

Copy `.env.example` to `.env`, `deploy/viti.example.json` to `deploy/viti.json` and
`deploy/instance.example.json` to `deploy/instance.json`, and replace every credential and
line/device placeholder with your own values. Set `VITI_BIND_IP` to this host's Tailscale IP
and `VITI_DIAGNOSTICS_DIR` to an absolute directory outside the checkout. Then:

```sh
docker compose up --build -d
```

The step-by-step guide, including SIM authentication options, the Windows ADB bridge, carrier
adaptation and troubleshooting, is [INSTALLATION.md](INSTALLATION.md). Day-to-day operation is
covered in [docs/OPERATIONS.md](docs/OPERATIONS.md).

## Build

```sh
npm --prefix web-src ci
npm --prefix web-src run build
go build ./cmd/viti-operator
docker build -t viti-operator-engine:local -f engine/Dockerfile engine
```

Build Android with JDK 17, Android SDK 36 and the included Gradle wrapper:

```sh
cd android
./gradlew testDebugUnitTest assembleDebug
```

The APK is generated at `android/app/build/outputs/apk/debug/app-debug.apk`.

## Development

```sh
npm --prefix web-src test
go test ./...
python test/swu-lab/run_lab.py --backend-image viti-operator-engine:local --no-build
```

The SWu laboratory is deterministic and does not contact a real carrier. Real SIM, IMS, SMS and
call checks are separate opt-in acceptance tests performed against the user's subscription.
Generated APKs, credentials, call recordings, SMS contents and diagnostics are not source assets
and must not be committed.

Further reading: [architecture and protocols](docs/ARCHITECTURE.md),
[background and source scope](docs/BACKGROUND.md), [SWu laboratory](test/swu-lab/README.md),
[third-party source provenance](THIRD_PARTY_NOTICES.md).

## Note

This is not a generic SIP-trunk client, and a working SIM alone does not establish carrier
compatibility. Call control state is volatile: surviving a manager or engine restart during a
call is not promised. Android Auto is a beta capability with restricted test-only distribution.
