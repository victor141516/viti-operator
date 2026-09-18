# Installation and carrier adaptation guide

Viti Operator exposes a physical mobile line to web, Android, and trusted service
clients. The SIM performs authentication; the server carries calls and SMS over
the carrier's Wi-Fi Calling infrastructure. This is not a generic SIP-trunk
client, and a working SIM alone does not establish carrier compatibility.

This guide covers the checked-in Compose deployment. Optional components in a
development checkout may differ: use `docker compose config --services` to see
what your revision actually contains. The bundled OpenAI bots and visual bot
editor are optional and are not prerequisites for bringing up the carrier connection.

## 1. Architecture and prerequisites

The Go manager provides the UI, API, history, client coordination, and SIM
authentication broker. The engine contains the patched Asterisk build and
strongSwan `charon` in the same network namespace. strongSwan establishes the
SWu/IKEv2 tunnel to the ePDG; Asterisk registers with IMS through its P-CSCF and
handles SIP, SMS, and conference audio. Clients use WebRTC, with private TURN
available for media relay.

Use the supplied engine image build: stock Asterisk does not include all of the
IMS-AKA, SMS-over-IMS, and carrier behavior used here. strongSwan is the only SWu
backend; Python is used for orchestration and adapters, not IKEv2 or ESP.

You need:

- A Linux host with Docker Engine and Compose v2, or Windows with Docker Desktop
  running Linux containers through WSL2. The engine needs `/dev/net/tun` and
  `NET_ADMIN`; an ordinary unprivileged/rootless deployment is not this recipe.
- Tailscale on the host and each remote client. Obtain the host address with
  `tailscale ip -4` and allow the required connections in your tailnet policy.
- A SIM/subscription provisioned for the carrier's Wi-Fi Calling and IMS service,
  plus one supported SIM authentication path described below.
- Git and Internet access to build the images. Host Go/Node installations are
  unnecessary for the container build, but the Windows ADB bridge needs Go 1.26
  or the version required by `go.mod` when built on the host.
- Android SDK Platform-Tools and an authorized USB connection for OpenStick;
  Python 3.10+ for its component installer.
- JDK 17 and Android SDK 36 if building the Android application locally.

The engine requests an **IPv6 inner carrier address** over an IPv4-sourced outer
tunnel. IPv6 inside this tunnel is distinct from having native IPv6 on the host.
An IPv4-only carrier PDN requires engine changes; see section 8.

## 2. Obtain the source and create private configuration

```sh
git clone https://github.com/victor141516/viti-operator.git
cd viti-operator
```

Linux:

```sh
cp .env.example .env
cp deploy/viti.example.json deploy/viti.json
cp deploy/instance.example.json deploy/instance.json
mkdir -p deploy/data/recordings
```

PowerShell:

```powershell
Copy-Item .env.example .env
Copy-Item deploy/viti.example.json deploy/viti.json
Copy-Item deploy/instance.example.json deploy/instance.json
New-Item -ItemType Directory -Force deploy/data/recordings | Out-Null
```

Run these copy commands only for a new installation; preserve existing private
configuration during upgrades. Use `deploy/viti.example.json` for Compose, not
the root `config.example.json`, which demonstrates a memory-backed setup.

Edit `.env`:

| Setting | Value to supply |
| --- | --- |
| `VITI_BIND_IP` | This host's Tailscale IPv4 address; not `0.0.0.0` |
| `VITI_HTTP_PORT` / `VITI_LOCAL_HTTP_PORT` | Defaults: `18787` / `18788`; change if occupied or reserved by Windows |
| `VITI_DOCKER_SUBNET` | An unused subnet that does not overlap LAN, VPN, Docker, or WSL networks |
| `VITI_API_KEY` | Independent, randomly generated client API credential |
| `VITI_ENGINE_EVENT_TOKEN` | Independent engine event credential |
| `VITI_AMI_SECRET` | Independent manager-to-Asterisk credential |
| `VITI_VOICE_PASSWORD` | Independent local SIP/TURN credential |
| `VITI_LINE_NUMBER` | Your actual line number in E.164 format |
| `VITI_IMS_REALM` | The actual IMS realm, matching the engine descriptor |
| `VITI_SIM_AUTH_TYPE` | `openstick`, `remote`, or `command`; PC/SC uses its separate override |
| `VITI_DIAGNOSTICS_DIR` | An absolute host directory outside the checkout |
| `VITI_DIAGNOSTICS_ENABLED` | Keep `0` for normal operation |
| `VITI_SCREENING_PIPELINE` | Leave empty for initial carrier validation; also keep the JSON pipeline empty |

Generate each secret independently, for example with `openssl rand -hex 32`.
Never reuse the template's placeholder credentials. Protect `.env`, subscriber
configuration, recordings, and diagnostic output; none belongs in Git.

Create the diagnostic directory and its `engine/asterisk` and `engine/runtime`
subdirectories even when packet capture is disabled: Compose still mounts them.
On Windows use an absolute path with forward slashes in `.env`; on Linux use an
absolute Linux path. If your revision includes `VITI_BOT_MEDIA_DIR`, give it an
existing directory too, even while leaving the bots idle.

On Linux, `deploy/data` and the manager's diagnostic directory must be writable
by the manager container's UID/GID `10001:10001`. Prepare ownership for those
specific directories; do not make the repository or secrets world-writable.

Edit `deploy/instance.json` with your real `imsi`, `mcc`, `mnc`, `imei`, `msisdn`,
and `smsc`. Preserve identifiers as JSON strings, including leading zeros.
The sample values are synthetic. `msisdn` must agree with `VITI_LINE_NUMBER`, and
the explicit or derived `realm` must agree with `VITI_IMS_REALM`.

The manager and engine read separate configuration. Changing only the manager's
realm or line number does not reconfigure the engine's subscriber identity.

## 3. Configure SIM authentication

### Option A: supported OpenStick hardware over ADB

Here, “OpenStick” means the validated HiMI/UFI MSM8916 stick running the supported
**stock Android 4.4.4 firmware**, not Linux OpenStick OS. An arbitrary USB modem
or ordinary Android phone is not interchangeable with this adapter.

1. Download and extract the
   [stock Android bridge component release](https://github.com/victor141516/viti-operator/releases/tag/openstick-android-bridge-v1.0.0).
2. Follow its [compatibility and installation guide](tools/openstick-android-bridge/README.md).
   From the extracted release directory, run:

   ```sh
   python install.py --check
   python install.py
   ```

3. Run `adb start-server` and `adb devices -l`. The selected device must report
   `device`, not `offline` or `unauthorized`.
4. Set `VITI_SIM_AUTH_TYPE=openstick` and `VITI_ADB_SERIAL` to that device's serial.

The component installer does not flash/root the device or configure Docker.
Its privileged APK and native helpers are recovered binaries whose original
source is currently unavailable; see the component guide for the precise scope.
The SIM remains the source of cryptographic AKA responses; do not attempt to
replace it with an IMSI or a made-up password.

On Windows, build and start the private host bridge:

```powershell
go build -o viti-adb-bridge.exe ./cmd/viti-adb-bridge
adb start-server
.\viti-adb-bridge.exe -listen 127.0.0.1:5038 -upstream 127.0.0.1:5037
```

The bridge stays in the foreground for this initial check. Set:

```dotenv
VITI_ADB_SERVER_SOCKET=tcp:host.docker.internal:5038
```

Docker Desktop reaches the host through that special hostname. The bridge
proxies ADB; it does **not** start the upstream ADB daemon. Both processes must
be running. Do not expose ADB with a wildcard `adb -a` listener.

On native Linux, `host.docker.internal:host-gateway` resolves to the host gateway,
not necessarily host loopback. Bind the restricted bridge to an explicit private
host address reachable from the Compose network, keep the upstream on loopback,
and restrict access to that bridge port. A Windows loopback recipe is not a
portable substitute for configuring the Linux host route and firewall.

### Option B: remote Android SIM authentication

Set `VITI_SIM_AUTH_TYPE=remote`. In `deploy/viti.json`, configure
`sim_auth.android_agent` to the intended agent name and an appropriate
`remote_timeout_seconds`. Setting an extra variable in `.env` has no effect
unless Compose passes it to a consumer; the configuration file is the explicit
route for this agent selection.

Install and connect the Android client, select its remote authentication role,
and use **Prepare permission with Shizuku** on the Connection screen. Device and
vendor support are required. Keep the remote service permitted to run in the
background. The phone must have the SIM corresponding to `instance.json`.

### Option C: PC/SC reader or command adapter

For a compatible Linux PC/SC setup, run `pcscd` on the host, configure the correct
reader and PIN in `instance.json`, and use:

```sh
docker compose -f compose.yml -f compose.pcsc.yml up --build -d
```

Set `PCSC_SOCKET` if the host socket is not `/run/pcscd/pcscd.comm`. The override
disables the manager's SIM provider and routes engine authentication to local
PC/SC helpers. A green manager `sim_ready` flag is therefore not a universal
acceptance criterion for this mode. Check tunnel and IMS registration as well.

A command adapter is an integration path, not an included generic modem driver.
Implement the contract in [docs/API.md](docs/API.md) and configure `sim_auth` in
the manager; verify real AKA support before attempting carrier registration.

## 4. Build, start, and verify

```sh
docker version
docker compose version
docker compose config --quiet
docker compose up --build -d
docker compose ps
```

The initial engine build compiles native dependencies and can take substantially
longer than later starts. Use `config --quiet` for validation: plain
`docker compose config` expands secret values into terminal output.

Check process health and carrier health separately:

```sh
curl http://127.0.0.1:18788/api/v1/health
curl http://127.0.0.1:18788/api/v1/status
docker compose logs --tail 80 engine
docker compose exec engine asterisk -rx "pjsip show registrations"
```

Use `curl.exe` on Windows PowerShell if `curl` is an alias. For the usual
manager-brokered SIM setup, expect `sim_ready`, `tunnel_ready`, `ims_ready`, and
`ready` to become true, and the IMS registration to show `Registered`.
A healthy HTTP process does not mean the carrier line is ready.

The loopback proxy injects the API credential. Remote API calls to the manager
require the configured credential (`Authorization: Bearer ...` or `X-API-Key`).
Tailscale supplies network access control; it does not replace application API
authentication. Treat the credential-injecting proxy as a trusted local endpoint.

## 5. Connect web and Android clients

Open `http://127.0.0.1:18788` on the server or
`http://<host-tailscale-ip>:18787` on a tailnet client, and configure the API
credential where required. For browser calling from another machine, use an
HTTPS reverse proxy with a trusted certificate and WebSocket upgrade support:
remote plain HTTP is not a browser microphone secure context. Keep the backend
private and forward both ordinary HTTP and the application's WebSockets.

Build the Android client if you do not already have a matching APK:

```powershell
Set-Location android
.\gradlew.bat testDebugUnitTest assembleDebug
Set-Location ..
```

On Linux use `./gradlew`. The output is
`android/app/build/outputs/apk/debug/app-debug.apk`. Install it on the client
phone, connect Tailscale, and enter the reachable server URL and API key. Grant
microphone and notification permissions and configure incoming-call alerts.
Shizuku is needed for the remote SIM role, not ordinary VoIP client use.

Verify both incoming and outgoing calls, audio in both directions, hangup from
both ends, an outgoing SMS, and an incoming SMS. Use numbers you control and
expect normal carrier charges. Run the first checks without screening or
auxiliary bots so that carrier behavior can be assessed independently.

Default network exposure:

| Path | Port/protocol | Scope |
| --- | --- | --- |
| Remote UI/API and WebSockets | TCP 18787 | Host Tailscale address |
| Credential-injecting local proxy | TCP 18788 | Host loopback only |
| Trusted-service media ICE mux | UDP 8790 | Host Tailscale address |
| TURN listener | UDP 3479 | Host Tailscale address; some revisions also publish loopback |
| Local SIP clients | UDP 5060, TCP 5061 | Host Tailscale address |
| Client RTP | UDP 10000–10059 | Host Tailscale address |
| Carrier IKE/NAT-T | Outbound UDP 500/4500 | Engine to carrier ePDG |

AMI, the engine's internal WebSocket, and TURN relay allocations stay within the
Compose network. Public Internet forwarding of the client ports is unnecessary.

## 6. Startup after a reboot

Compose uses `restart: unless-stopped`. This restarts containers when Docker
starts; it cannot start Docker Desktop, Tailscale, or the host ADB daemon.
Explicitly stopped containers also remain stopped until started again.

On Linux, enable Docker/Tailscale through the host service manager and arrange
for the selected SIM provider to start. On Windows, enable Docker Desktop's
start-at-sign-in setting. This is **sign-in startup**, not a guarantee that the
line is available before anyone logs into Windows.

For OpenStick on Windows, create a local PowerShell launcher outside Git, using
the actual absolute paths on that host:

```powershell
$ErrorActionPreference = 'Stop'
& 'C:\path\to\platform-tools\adb.exe' start-server
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& 'C:\path\to\viti-adb-bridge.exe' -listen 127.0.0.1:5038 -upstream 127.0.0.1:5037
exit $LASTEXITCODE
```

Configure a per-user Scheduled Task to run that launcher at sign-in with
`powershell.exe -NoProfile -NonInteractive -WindowStyle Hidden -File <launcher>`.
Use an unlimited execution time, restart on failure, and do not start duplicate
instances. Update an existing bridge task instead of installing a second one.
The included `Install-VitiAdbBridge.ps1` automates this setup and starts ADB before
the proxy. After stopping the initial foreground bridge, run it from the root:

```powershell
.\Install-VitiAdbBridge.ps1 -AdbExecutable 'C:\path\to\platform-tools\adb.exe'
```

After a planned reboot, verify the whole chain: Docker, containers, ADB device,
SIM provider, tunnel, IMS, then a client. A bridge process can be alive while its
upstream ADB daemon is absent, causing `protocol fault` or disconnected-SIM errors.

## 7. Optional automation and storage

The checked-in integration API supports trusted WebSocket services, WebRTC media,
and an ordered incoming-call screening pipeline. Start with an empty pipeline;
add stable service names only after baseline calling works. See
[docs/API.md](docs/API.md) and [docs/OPERATIONS.md](docs/OPERATIONS.md).

The OpenAI Compose services remain idle without `OPENAI_API_KEY`. Leave it empty
until intentionally enabling them. Commercial screening additionally needs a
private greeting file and an enabled handler. Follow the
[bot setup instructions](docs/OPERATIONS.md) and
[integration protocol](docs/INTEGRATIONS.md) for media formats, activation,
delegation, and session cleanup.

Back up `.env`, `deploy/viti.json`, `deploy/instance.json`, and `deploy/data`
securely. Recordings and history contain personal data. Take consistent backups
during a maintenance window. Active call/media state is not recoverable simply
by restoring a state file; avoid upgrades and restarts during calls.

## 8. Adapting the service to another carrier

Compatibility must be established for each carrier and subscription. The local
reference deployment uses Digi Spain; that does not certify other carriers.
An MVNO can use domains or provisioning different from the network carrying its
radio traffic. Obtain values from the carrier, SIM/ISIM metadata exposed by a
supported provider, or authorized diagnostics from a working device.

### Configuration changes supported today

Edit `deploy/instance.json` and keep the manager's values synchronized:

| Parameter | Engine configuration | Manager/configuration counterpart |
| --- | --- | --- |
| SIM identity | `imsi` | Select the provider containing that exact SIM |
| Home network | `mcc`, `mnc` | Set `VITI_IMS_REALM` consistently; do not use a visited network's codes by assumption |
| Line number | `msisdn` | `VITI_LINE_NUMBER` and any explicit `telephony.ami.from_number` |
| Device identity | `imei` | No automatic substitution; use the appropriate actual device identity |
| IMS realm | Optional `realm`; otherwise derived from MCC/MNC | `VITI_IMS_REALM` and any explicit `telephony.ami.realm` |
| ePDG | Optional `epdg`; otherwise derived from MCC/MNC | No UI/API port change needed |
| IMS APN | Optional `apn`, default `ims` | No corresponding client setting |
| Requested ePDG identity | `idr_mode`: `apn` or `fqdn` | Changes the IDr request format, not SIM identity |
| P-CSCF | Optional `pcscf`; otherwise discovered | Prefer discovery; do not copy an address from an old tunnel |
| SMS service centre | `smsc` | Where supported, `VITI_SMSC` seeds manager debug defaults; engine `smsc` remains essential |
| Access-network information | `sip.pani`, or structured `sip.pani_*` fields | `VITI_ACCESS_NETWORK_INFO`, where available, is an editor default, not the engine registration setting |
| SIP/SDP presentation | `sip.user_agent`, `sip.sdp_owner`, `sip.sdp_session` | No corresponding client setting |
| Rekey timing | `rekey_minutes` (default 30), `ike_rekey_minutes` (default 45) | Must be validated over long-lived sessions |

With three-digit MCC and MNC components, the derived defaults are:

```text
IMS realm: ims.mnc<MNC3>.mcc<MCC3>.3gppnetwork.org
ePDG:      epdg.epc.mnc<MNC3>.mcc<MCC3>.pub.3gppnetwork.org
EAP NAI:   0<IMSI>@nai.epc.mnc<MNC3>.mcc<MCC3>.3gppnetwork.org
```

The renderer pads MNC to three digits; supply MCC as three digits too. Confirm
the MNC length from actual subscription data, not by guessing where to split
the IMSI. Domain overrides do not automatically change the constructed EAP NAI.

For P-Access-Network-Info, use the actual access context. Supported structured
fields include `pani_access_type`, `pani_country_enable`, `pani_country`,
`pani_node_id_enable`, `pani_node_id`, and `pani_local_time_zone`. Do not invent a
BSSID, country, or location value to make a registration appear successful.

Apply reviewed configuration during a maintenance window:

```sh
docker compose config --quiet
docker compose up -d --force-recreate manager engine
```

Use the same `-f` overrides as your deployment, including `compose.pcsc.yml` if
applicable. Recreate dependent services if their network namespace or settings
changed. Editing a generated Asterisk file inside the container is temporary:
the next render replaces it.

### Carrier differences that require code changes

The current engine is not a complete carrier-profile system. These are real
implementation boundaries, not additional `.env` switches:

| Requirement from another carrier | Current behavior and required work |
| --- | --- |
| IPv4-only or dual-stack IMS PDN | `engine/strongswan_adapter.py` requests `vips=["::"]` and `remote_ts=["::/0"]`. Extend address requests, selectors, reconciliation, route isolation, and tests together. Rendering an IPv4 P-CSCF alone is insufficient. |
| Different IKE/ESP algorithms | `build_connection()` supplies fixed proposal lists. `ike.proposals` and `ike.esp_proposals` are read by the renderer but are not wired into that VICI payload; editing those JSON fields alone does not change negotiation. Add and test validated profile inputs. |
| Different EAP method or identity convention | The adapter uses EAP-AKA and constructs the permanent NAI shown above. AKA-prime or alternative identity handling requires strongSwan/plugin/broker integration work, not a realm-only edit. |
| Different IMS private/public identities | `engine/templates/pjsip.conf.j2` derives registration/authentication identities from IMSI and realm, with MSISDN used for the calling identity. Separate ISIM identities need explicit configuration and template/authentication changes. |
| Different IMS SIP transport/security | The carrier transport in `pjsip.conf.j2` is TCP, bound to `ipsec0`, with IMS security behavior. `sip.transport` controls local client SIP; it does not switch the carrier leg to UDP or TLS. Adapt the carrier template and verify security negotiation. |
| Different voice codecs or SDP/precondition behavior | The carrier endpoint currently enables AMR and fixed IMS behavior. Add supported codec/profile choices and validate transcoding, PRACK/UPDATE, early media, and both call directions. Changing WebRTC codecs alone is insufficient. |
| Different SMS routing or acknowledgements | Review `smsc_uri`, URI formatting, SMS-over-IMS patches, RP acknowledgements, multipart handling, and delivery-report correlation. A successful SIP registration does not prove SMS interoperability. |
| Different P-CSCF delivery or DNS requirements | The adapter normally waits for P-CSCF discovery. Review the discovery plugin, `render.py`, and `tunnel_reconcile.py`; do not bypass readiness without a verified usable proxy. |

Source-level changes must preserve the single strongSwan backend, shared engine
network namespace, and source-specific table 220 routing. Only traffic sourced
from the carrier-assigned inner address may enter that routing table. Preserve
the ordered tunnel event/status contract and transactional P-CSCF/inner-address
application. Losing AKA access must not proactively destroy an established SA.

Carrier account policy also matters: Wi-Fi Calling provisioning, device support,
roaming restrictions, and supported authentication methods cannot be fixed merely
by changing a hostname or User-Agent. Record the actual limitation when testing
a new carrier rather than claiming universal support.

### Validate a new carrier in layers

1. Confirm the selected SIM provider can serve authentication and matches the
   configured identity. Never repeatedly guess a SIM PIN.
2. Confirm ePDG DNS resolution and outbound UDP 500/4500 reachability.
3. Confirm IKE/EAP-AKA completion, assigned inner address, and P-CSCF discovery.
4. Confirm IMS AKA and `Registered` status in Asterisk.
5. Test incoming/outgoing calls, two-way audio, DTMF, and remote/local hangup.
6. Test incoming/outgoing SMS, multipart text, delivery reporting, and whether
   incoming messages are acknowledged rather than delivered repeatedly.
7. Observe CHILD/IKE rekeys and reauthentication over a longer idle period and
   during controlled calls. Test recovery and client reconnection separately.
8. Enable optional screening and auxiliary bots only after that baseline passes.

For engine changes, run the repository's required deterministic checks:

```sh
(cd engine && python -m unittest test_strongswan_adapter.py test_tunnel_reconcile.py)
python test/test_swu_lab_runner.py
docker build -t viti-operator-engine:local -f engine/Dockerfile engine
python test/swu-lab/run_lab.py --backend-image viti-operator-engine:local --no-build
```

The first command uses a POSIX subshell; on PowerShell use `Push-Location engine`,
run the unittest command, and `Pop-Location`. The isolated laboratory does not
contact the carrier. Real SIM/call/SMS acceptance checks are separate and opt-in.
Laboratory success does not establish compatibility with an untested operator.

## 9. Troubleshooting by failing layer

| Symptom | Check first |
| --- | --- |
| App reports an unexpected end of stream | Docker daemon, running manager/proxy, local API response, then Tailscale forwarding; an alive forwarder can point to a stopped backend |
| Containers healthy but SIM unavailable | `adb devices -l`, upstream ADB daemon, bridge listener, selected serial/provider, and container-to-host route |
| Bridge running but ADB protocol fault | Start the host ADB daemon; the proxy cannot provide it by itself |
| SIM ready but tunnel down | ePDG DNS/reachability, subscriber/MCC/MNC/APN values, AKA result, IKE proposal and carrier provisioning |
| Tunnel up but IMS unregistered | P-CSCF discovery, realm/identities, SIP AKA, carrier transport/security and registration response |
| UI works but audio does not | Browser secure context, microphone permission, SIP/WebSocket registration, ICE/TURN and UDP ports |
| Calls work but SMS does not | Engine SMSC, URI/PANI requirements, SMS acknowledgements and delivery correlation |
| Failure returns after reboot | Docker sign-in startup, Tailscale, container restart policies, and ADB-before-bridge task ordering |

Enable `VITI_DIAGNOSTICS_ENABLED=1` only for a bounded diagnostic session and
recreate affected services to apply it. Captures and protocol logs can contain
subscriber identities, messages, audio, and authentication material. Keep them
outside Git, redact before sharing, and turn tracing off after collecting the
necessary evidence.

Further reference: [README.md](README.md),
[operations](docs/OPERATIONS.md), [architecture](docs/ARCHITECTURE.md),
[API](docs/API.md), and [SWu laboratory](test/swu-lab/README.md).
