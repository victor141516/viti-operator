# Repository guidance for coding agents

## Product and language

- Viti Operator is a self-hosted calling and SMS service that uses a physical
  SIM and the carrier's Wi-Fi Calling path.
- Keep user-facing copy, code comments, commit messages, issue/PR text, and
  documentation in English.
- Do not commit credentials, subscriber identifiers, phone numbers, recordings,
  packet captures, diagnostic dumps, APKs, or workstation-specific settings.
- Treat logs and device metadata as sensitive. Filter or redact identifiers
  before displaying output; compare SIM identities without printing them.

## Repository and deployment

- Use `docs/ARCHITECTURE.md` for component ownership, `docs/API.md` for HTTP
  contracts, `docs/INTEGRATIONS.md` for bots/events/media, and
  `docs/OPERATIONS.md` and `INSTALLATION.md` for runtime procedures.
- Preserve existing uncommitted work. The source checkout and the directory
  running Docker Compose may differ. Inspect container Compose labels, mounts,
  and image IDs before rebuilding or deploying; do not assume editing this
  checkout changes the running service.
- Recreate only affected services when possible. Verify the deployed image and
  actual runtime behavior before reporting a fix as installed.
- This is a personal phone line. Optional bots and automation must fail open
  to human ringing. Keep runtime recovery simple and consistent with the
  existing volatile call state.

## Carrier tunnel architecture

- strongSwan `charon` is the only SWu/IKEv2 backend. It uses
  `kernel-libipsec`, `/dev/net/tun`, and the `ipsec0` userspace ESP interface.
- Do not introduce an alternate SWu backend or a runtime selector. Python
  remains valid for orchestration and adapters; it must not implement IKEv2 or
  ESP.
- Keep strongSwan and Asterisk in the same engine network namespace. Only
  traffic sourced from the carrier-assigned inner address may use table 220.
- Preserve the public tunnel contract in `engine/tunnel_contract.py`: atomic
  status plus ordered, durable `tunnel_start`, `tunnel_up`, and `tunnel_down`
  events for each generation.
- Treat published `DOWN` as a completion boundary: its `tunnel_down` event must
  already be durable, and production PID 1 must also wait for the exact adapter
  process to exit before leaving the network namespace.
- Losing an AKA provider must not proactively destroy an established tunnel.
  Recovery must not leave duplicate `charon` processes, stale route rules, or
  a stale `CONNECTED` status.
- P-CSCF and inner-address reconciliation must be transactional. Commit their
  applied state together in `tunnel.applied.json`, and do not advance it until
  rendering, restart/reload, and IMS registration actions have all succeeded.

## Outage diagnosis and Windows recovery

- Check each layer independently: Docker engine, containers, HTTP API,
  client/Tailscale reachability, SIM/AKA provider, SWu tunnel, and IMS registration.
  A healthy manager container does not prove that carrier calls are available.
- Confirm carrier recovery using both `/run/vowifi/swu_status.json`
  (`CONNECTED`) and Asterisk `pjsip show registrations` (`Registered`). A
  listener, queued operation, or `CONNECTING` state is not proof of recovery.
- A SIM reporting `READY` only proves card availability. Conversely, a missing
  SIM must not block calls while the existing tunnel and IMS remain operational.
- If initial IKE receives no response, inspect the effective host route and
  VPN exit node before investigating AKA or SMS ACKs. Preserve intentional
  direct carrier routing and private tailnet access; do not silently enable an
  exit node or change the user's routing policy.
- An AKA `authentication-failure` identifies the failing layer; it does not
  by itself prove bad carrier provisioning or a permanently faulty SIM. Inspect
  the helper result, identity match, and modem state. Distinguish authentication
  failure from synchronization failure. Verify recovery after any modem restart.
- The Windows ADB daemon normally listens on loopback port 5037. The restricted
  `viti-adb-bridge` forwards from loopback/private port 5038. Host bridge port
  5038 and engine Asterisk AMI port 5038 belong to different endpoints.
- An open bridge port does not prove its upstream ADB daemon is running. Check
  host ADB and the manager's configured `ADB_SERVER_SOCKET` separately. Errors
  such as `protocol fault` or `get-state` timeouts can occur when the bridge is
  alive but host ADB has exited.
- Use `Install-VitiAdbBridge.ps1` for persistent Windows setup. Preserve both
  tasks: `Viti Operator ADB Bridge` starts ADB before the bridge, and
  `Viti Operator ADB Server Watch` ensures ADB is started every 30 seconds.
  These are per-user logon tasks; do not describe them as pre-login services.
- Keep Docker AI/Gordon and Docker Model Runner disabled in the Viti setup.
  `EnableDockerAI=false` alone does not disable Model Runner; verify
  `EnableInference=false` too. These settings do not disable GPU/NVENC access
  for ordinary containers.
- Docker Desktop on Windows can fail before the Linux engine starts because
  it cannot access stale AF_UNIX endpoints under its `run` directory or
  `docker-secrets-engine`. Confirm the exact backend-log error first. Stop
  Docker and rename only the affected runtime directory aside for recovery,
  preserving its contents. Do not use factory reset, prune, or delete Docker
  data/VHDX files to repair a runtime socket.
- State explicitly whether recovery was automatic or resulted from an action.
  Report observed causes separately from hypotheses and unresolved causes.

## Android connection alerts and installation

- The foreground voice service owns background connection monitoring; checks
  must continue when the Activity or WebView is suspended. Call readiness
  requires voice registration, reachable API, and operational carrier/IMS.
- Carrier-only outages are announced by server-originated local SIP calls to
  registered clients. Local Android voice/API loss uses the persistent
  `No connection` alert. Do not merge these two alert mechanisms.
- The local alert's `Silence` action stops sound for the current continuous
  outage while retaining the notification, service, and reconnection attempts.
  Reason changes, repeated probes, and call-related audio pauses must not clear
  that silence. Recovery clears it so the next outage rings again.
- Scope notification actions to the current outage token. Stale actions must
  not silence a new outage. Silence must never reject a real call, disconnect
  the voice service, or terminate the app.
- With multiple ADB devices attached, inspect `adb devices -l` and verify the
  requested phone's manufacturer/model. Use explicit `adb -s <serial>` for
  installation and diagnostics; the OpenStick modem is not the user phone.
- Install updates with `adb install -r` to preserve app settings, then reopen
  the app so its voice service can resume. Report installation separately from
  building or making an APK available for download.
- Keep APK downloads on the existing private Tailscale endpoint. Inspect its
  current Serve configuration instead of hardcoding hostnames or exposing it
  through public Funnel. Do not commit generated APKs.

## Bots, media, and SMS

- Preserve the distinction between screening controllers and auxiliary media
  consumers. Screening has an ordered per-call pipeline snapshot and
  per-service opaque JSON history; auxiliary consumers do not keep a call alive.
  Follow the command ownership, timeouts, and fail-open rules in the integration
  documentation rather than inferring control from receipt of a broadcast event.
- Disabled auxiliary bots must not receive call events or media. Bot inventory
  follows connection liveness; do not add a permanent "forget bot" operation.
- OpenAI bots must open no billable API session while idle. Close the session
  when the call, media, control connection, or service access ends. Use short
  opt-in live tests and keep user greetings/audio outside committed source.
- Incoming SMS acknowledgement includes the response to the incoming SIP
  MESSAGE and a separate outbound RP-ACK MESSAGE. A queued RP-ACK is not carrier
  acceptance: inspect the final `VITI_MT_SMS_ACK` result. Even SIP acceptance
  does not prove the SMSC permanently stopped redelivery. Do not hide repeats
  by deduplicating message text alone.
- Client media or network loss is not an explicit carrier hangup. Preserve
  logical participants/rejoin behavior and the documented terminal events.

## Verification

Run focused tests while editing, then verify the production image and isolated
SWu laboratory before merging tunnel changes:

```sh
(cd engine && python -m unittest test_strongswan_adapter.py test_tunnel_reconcile.py)
python test/test_swu_lab_runner.py
docker build -t viti-operator-engine:local -f engine/Dockerfile engine
python test/swu-lab/run_lab.py --backend-image viti-operator-engine:local --no-build
```

The packaged laboratory must exercise the real `/entrypoint.sh`, cancellation
during a blocked startup `IKE_AUTH`, in-process `charon` crash recovery, real
UDP/TCP transport, route isolation, and terminal shutdown state. Keep live
carrier tests opt-in; they are user acceptance tests, not part of the
deterministic laboratory.

For unrelated application changes, use the narrower commands documented in
`README.md` and `docs/OPERATIONS.md`. Update architecture and operations
documentation only after behavior and tests are stable.

- For Go application changes, run focused package tests; use `go test ./...`
  when the change spans packages. Web/voice bundle changes require the web tests
  and build; rebuild the embedded bundle before Go or Android packaging.
- For Android changes, use JDK 17, SDK 36, and the included wrapper:
  `cd android` then `./gradlew testDebugUnitTest assembleDebug` (Windows:
  `.\gradlew.bat testDebugUnitTest assembleDebug`). For alert changes, cover
  silence during one outage, recovery/rearming, and stale notification actions.
- For Windows installer changes, verify PowerShell parsing and the installed
  scheduled-task state. Test the actual recovery path when safe; a successful
  task-registration command alone does not prove the daemon is supervised.
- Keep live calls, SMS, and paid bot tests opt-in. User authorization persists
  within the agreed workflow, but identify the destination before initiating
  a new live outbound test call. Prefer isolated labs and local test calls for
  deterministic checks.
