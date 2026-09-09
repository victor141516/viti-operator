# OpenStick hardware: stock Android SIM bridge binaries

This is a component-only bundle for the HiMI/UFI MSM8916 stick running the
specific manufacturer's Android 4.4.4 firmware listed in `manifest.json`.
Despite the historical OpenStick name, this is **not** the Linux OpenStick OS,
an Android firmware image, or a complete Viti Operator release.

## Requirements

- Python 3.10 or newer (standard library only; no pip dependencies).
- Official Android SDK Platform-Tools (`adb`) installed on the host.
- A working USB ADB driver, an authorized ADB connection, and the supported stock firmware.
- The verified device's SELinux `Permissive` runtime; Enforcing mode is unvalidated
  and rejected. The installer never disables SELinux or changes its policy.
- No active authentication/call work while doing a fresh installation.

Python and Google's ADB executables are not redistributed in the ZIP. All three
bridge binaries, the installer and its manifest are included; the installer does
not download anything or need the source repository.

## Install

Extract the complete ZIP into a directory. Open a terminal in that directory:

```sh
python install.py --check
python install.py
```

On Windows, `py -3` can be used instead of `python`. ADB is discovered from PATH,
Android SDK environment variables or conventional SDK locations. If necessary:

```sh
python install.py --adb "/path/to/platform-tools/adb" --check
python install.py --adb "/path/to/platform-tools/adb"
```

If multiple compatible sticks are connected, specify `--serial SERIAL` from
`adb devices -l`. This selects a device; it does not bypass compatibility checks.
An ordinary phone, a Linux OpenStick installation or a different firmware build
is rejected. The installer is not tied to the original owner's device serial.

The allowlist compares manufacturer/product/board, CPU ABI, Android/API version,
full firmware fingerprint and baseband version. It also checks required vendor
runtime files. These are conservative software compatibility checks, **not**
cryptographic attestation of the physical PCB. Even another UFI/MSM8916 build is
rejected until it is separately validated; there is deliberately no force option.

## What installation changes

- Installs `payload/sim-identity-platform.apk` as `lab.codex.simidentity` using
  ordinary `adb install`. Android must accept its signing identity and phone
  shared UID; the installer does not root the device or bypass signature checks.
- Places the two helper executables at `/data/local/tmp/qmi_auth_aka` and
  `/data/local/tmp/qmi_uim_smsc`, with executable permissions.
- Verifies the exact installed bytes and required `android.uid.phone` UID.

It never flashes partitions, changes radio/network configuration, reboots,
uninstalls existing apps, sends SMS, initiates calls or runs a SIM authentication
challenge. It does not start the bridge activity or read subscriber metadata.

Rerunning is safe: matching files are skipped and helper permissions can be
repaired. If a different APK/helper is already present, the installer refuses
before writing anything. Back up and review that version manually; no automatic
overwrite/uninstall is offered. A failed connection can leave a partial install;
rerun to complete it. The installer is not an all-or-nothing firmware transaction.

## Complete server setup

After successful verification, use the printed `VITI_ADB_SERIAL` value in your
private server configuration and select the `openstick` local SIM adapter.
Follow the repository's Windows ADB bridge/Compose guide to make the device
reachable from the server container. Do not expose ADB on an untrusted network.

The installer does not set up Docker, carrier credentials or an ADB TCP listener.
Successful installation verifies the bridge files and Android identity, not SIM
readiness, carrier provisioning, ePDG/IMS registration or a live authentication.
Carrier authentication is performed later by Viti Operator when needed.

## Binary provenance and missing source

These exact three artifacts were recovered from the working stock-Android stick.
They were created during the project's early SIM experiments. Their original
source/build files are currently unavailable; this release does **not** claim a
reproducible source build. The installer source and tests are available in Git.
Recovering the original sources or reimplementing these components from scratch
is tracked in [issue #24](https://github.com/victor141516/viti-operator/issues/24).
Do not confuse these binaries with upstream OpenStick code.

`manifest.json` records payload sizes, SHA-256 hashes and the APK signing
certificate digest. The APK signature was verified before release. The payload
does not contain a firmware dump, SIM responses, subscriber metadata, app data,
ADB private keys, signing private keys or workstation configuration.

## Security limitations

This is a privileged **lab** bridge for an obsolete Android release, not a
hardened production component. The APK is debuggable, shares the phone UID,
requests `READ_PHONE_STATE` and `SEND_SMS`, and exports its activity and content
provider. Its binary includes legacy laboratory SMS functionality; the installer
does not invoke it. Use only on your trusted dedicated stick and protect ADB and
the device from untrusted applications. No signing private key is distributed.

Archive checksums detect accidental changes; they are not an independent trust
root if an attacker also replaces the manifest/checksum file. The binaries retain
their existing provenance; no new blanket license is assigned. Complete a source,
security and licensing review before public or production redistribution.

## Validation scope

The release process checks APK signatures, payload hashes and automated installer
tests. Detection and idempotent installation are exercised on the existing device.
A clean-device install is covered by simulation, not by resetting the working
stick. No test call/SMS or SIM challenge is automatically performed.
