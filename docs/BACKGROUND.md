# Background

Overview notes moved from the [README](../README.md). The full design is in
[ARCHITECTURE.md](ARCHITECTURE.md).

## Engine

The Go manager owns history, recordings and client state. The bundled
Asterisk/VoWiFi engine handles ePDG, IMS and conference audio.

The engine has one SWu implementation: strongSwan `charon` negotiates IKEv2
and EAP-AKA, while `kernel-libipsec` carries userspace ESP through `ipsec0`.
Python orchestration adapts strongSwan's VICI state to the stable
manager-facing tunnel contract.

## Deployment notes

On Windows with an OpenStick, keep the restricted ADB bridge alive across
logouts and reboots with a per-user scheduled task:

```powershell
go build -o viti-adb-bridge.exe ./cmd/viti-adb-bridge
.\Install-VitiAdbBridge.ps1
```

The task starts host ADB before the bridge at logon, restarts after failure and binds to
Windows loopback by default. Docker Desktop reaches it through
`VITI_ADB_SERVER_SOCKET=tcp:host.docker.internal:5038`; ADB is never exposed to
the LAN and the setup does not depend on WSL receiving a stable address.

Containers listen on all internal interfaces. Compose limits published ports
to the configured host address. Open the remote web/API at
`http://<VITI_BIND_IP>:18787`, or the local web at `http://127.0.0.1:18788`.
The private TURN service uses UDP; its relay ports stay inside the Docker network.
The trusted-service protocol and media/screening lifecycle are specified in
[docs/INTEGRATIONS.md](INTEGRATIONS.md). The bundled OpenAI Realtime bot is
idle until `OPENAI_API_KEY` is added to the untracked `.env` file. Commercial
screening also requires an uncommitted `hello.wav`; see
[docs/OPERATIONS.md](OPERATIONS.md).

## Call lifecycle

The carrier leg ends when its last logical participant explicitly leaves or when
the carrier or engine reports a real terminal transport event. Losing a browser,
Android network, WebRTC leg, SIM reader, or readiness probe is not itself an
explicit hangup; a confirmed current tunnel-down boundary remains terminal.
Clients retain logical identity and rejoin the existing conference when media
returns. This control state is volatile: surviving a manager or engine restart
during a call is not promised.

## SWu laboratory

The isolated SWu laboratory exercises authentication faults, real UDP/TCP
transport, client- and responder-initiated rekeys, reauthentication, crash
recovery, route isolation, cancellation during a blocked startup `IKE_AUTH`,
and the production entrypoint lifecycle. It is deterministic and does not
contact a real carrier; see the [laboratory guide](../test/swu-lab/README.md).

## Source scope

This is a clean source snapshot with a new Git history. It includes reusable
application, engine, tests, configuration templates and opt-in diagnostics code.
It excludes deployment credentials, local configuration, raw traces, recordings,
screenshots, APKs, modem recovery/flashing tools and workstation-specific scripts.
The original local histories are not included. The three recovered stock-Android
bridge binaries are distributed separately as component release assets, not in Git.
