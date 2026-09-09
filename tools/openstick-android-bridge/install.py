#!/usr/bin/env python3
"""Install the recovered bridge only on the explicitly supported stock firmware."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import uuid


class InstallError(RuntimeError):
    pass


def digest(path):
    with open(path, 'rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest() if hasattr(hashlib, 'file_digest') else hashlib.sha256(stream.read()).hexdigest()


def verify_payload(root, manifest):
    expected_names = {'sim-identity-platform.apk', 'qmi_auth_aka', 'qmi_uim_smsc'}
    if manifest.get('schema') != 1 or manifest.get('package') != 'lab.codex.simidentity':
        raise InstallError('Unsupported bundle manifest.')
    if len(manifest['files']) != 3 or {item['name'] for item in manifest['files']} != expected_names:
        raise InstallError('Unexpected payload inventory.')
    if not manifest.get('expected_properties', {}).get('ro.build.fingerprint'):
        raise InstallError('Missing firmware allowlist.')
    for item in manifest['files']:
        path = root / 'payload' / item['name']
        if not path.is_file() or path.stat().st_size != item['size'] or digest(path) != item['sha256']:
            raise InstallError('Payload missing or checksum mismatch: ' + item['name'])
        if item['name'].endswith('.apk'):
            if item['kind'] != 'apk':
                raise InstallError('Invalid APK entry.')
        else:
            if item['kind'] != 'helper' or item['remote'] != '/data/local/tmp/' + item['name']:
                raise InstallError('Invalid helper destination.')
            header = path.read_bytes()[:20]
            if header[:6] != b'\x7fELF\x01\x01' or int.from_bytes(header[18:20], 'little') != 40:
                raise InstallError('Helper is not a 32-bit little-endian ARM ELF.')


def find_adb(explicit=None):
    candidates = [explicit] if explicit else [shutil.which('adb')]
    if not explicit:
        for key in ('ANDROID_HOME', 'ANDROID_SDK_ROOT'):
            if os.environ.get(key):
                candidates.append(str(Path(os.environ[key]) / 'platform-tools' / ('adb.exe' if os.name == 'nt' else 'adb')))
        if os.name == 'nt':
            candidates.append(str(Path.home() / 'AppData/Local/Android/Sdk/platform-tools/adb.exe'))
        else:
            candidates.extend(str(Path.home() / p) for p in ('Android/Sdk/platform-tools/adb', 'Library/Android/sdk/platform-tools/adb'))
    for candidate in candidates:
        if candidate and Path(candidate).is_file():
            return str(Path(candidate).resolve())
    raise InstallError('ADB not found. Install official Android SDK Platform-Tools and use --adb PATH if needed.')


class ADB:
    def __init__(self, executable, serial=None):
        self.executable, self.serial = executable, serial

    def run(self, *args):
        command = [self.executable] + (['-s', self.serial] if self.serial else []) + list(args)
        try:
            result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    text=True, encoding='utf-8', errors='replace', timeout=90)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise InstallError('ADB execution failed or timed out; check the USB connection.') from exc
        if result.returncode:
            raise InstallError('ADB command failed: ' + result.stderr.strip() + ' ' + result.stdout.strip())
        return result.stdout.replace('\r', '').strip()

    def shell(self, *args, allow_failure=False):
        # Android 4.4 ADB does not reliably propagate a remote exit status.
        command = ' '.join(shlex.quote(arg) for arg in args)
        # This stock shell has echo but no printf executable/builtin.
        command += '; viti_rc=$?; echo; echo __VITI_RC__$viti_rc'
        output = self.run('shell', command)
        match = re.search(r'\n__VITI_RC__(\d+)\s*$', '\n' + output)
        if not match:
            raise InstallError('Remote command did not return a status marker.')
        body = ('\n' + output)[:match.start()].strip()
        if int(match[1]) and not allow_failure:
            raise InstallError('Remote command failed: ' + args[0] + ': ' + body)
        return body, int(match[1])

    def pull_hash(self, remote):
        with tempfile.TemporaryDirectory(prefix='viti-bridge-verify-') as directory:
            target = Path(directory) / 'binary'
            self.run('pull', remote, str(target))
            return digest(target)


def list_devices(adb):
    devices = []
    for line in adb.run('devices', '-l').splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[1] in ('device', 'offline', 'unauthorized'):
            devices.append((fields[0], fields[1]))
    return devices


def identify(adb, expected):
    mismatches = []
    for key, wanted in expected.items():
        actual, _ = adb.shell('getprop', key)
        if actual.strip() != wanted:
            mismatches.append(key)
    if mismatches:
        raise InstallError('Unsupported hardware/firmware; mismatched properties: ' + ', '.join(mismatches))
    selinux, _ = adb.shell('getenforce')
    if selinux != 'Permissive':
        raise InstallError('Only the verified SELinux Permissive runtime is supported. This installer does not disable SELinux or bypass policy.')
    for path in ('/system/bin/linker', '/system/vendor/lib/libqmi.so'):
        adb.shell('test', '-r', path)


def select_device(adb, expected, serial=None):
    devices = list_devices(adb)
    if serial:
        if (serial, 'device') not in devices:
            raise InstallError('Requested device is absent, offline or unauthorized.')
        chosen = ADB(adb.executable, serial)
        identify(chosen, expected)
        return chosen
    matches = []
    for candidate, state in devices:
        if state != 'device':
            continue
        device = ADB(adb.executable, candidate)
        try:
            identify(device, expected)
            matches.append(device)
        except InstallError:
            continue
    if not matches:
        raise InstallError('No supported, authorized stock-Android HiMI/UFI device found. Check ADB/USB drivers. Linux OpenStick and ordinary phones are not supported.')
    if len(matches) != 1:
        raise InstallError('Several compatible devices found. Select one explicitly with --serial SERIAL.')
    return matches[0]


def package_path(adb):
    output, _ = adb.shell('pm', 'path', 'lab.codex.simidentity')
    if not output:
        return None
    paths = [line[8:] for line in output.splitlines() if line.startswith('package:')]
    if len(paths) != 1 or not re.fullmatch(r'/(?:data|system)/(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.apk', paths[0]):
        raise InstallError('Unexpected installed package path; refusing to proceed.')
    return paths[0]


def verify_phone_identity(adb):
    output, _ = adb.shell('dumpsys', 'package', 'lab.codex.simidentity')
    if not re.search(r'\buserId=1001\b', output) or 'android.uid.phone' not in output:
        raise InstallError('Bridge does not have the required Android phone shared UID. No root/signature bypass will be attempted.')


def inspect_installation(adb, manifest):
    plan = []
    apk_path = package_path(adb)
    for item in manifest['files']:
        remote = apk_path if item['kind'] == 'apk' else item['remote']
        exists = remote is not None
        if item['kind'] == 'helper':
            _, code = adb.shell('test', '-e', remote, allow_failure=True)
            exists = code == 0
        if exists and adb.pull_hash(remote) != item['sha256']:
            raise InstallError('A different version is already installed: ' + item['name'] + '. Nothing has been changed; back up and review it manually.')
        if exists and item['kind'] == 'helper':
            _, code = adb.shell('test', '-x', remote, allow_failure=True)
            action = 'present' if code == 0 else 'permissions'
        else:
            action = 'present' if exists else 'install'
        plan.append((item, action))
    if apk_path:
        verify_phone_identity(adb)
    return plan


def install(adb, root, manifest, plan):
    # All conflicts are inspected before the first device write.
    for item, action in plan:
        if action == 'present':
            continue
        source = root / 'payload' / item['name']
        if item['kind'] == 'apk':
            output = adb.run('install', str(source))
            if not re.search(r'^Success\s*$', output, re.MULTILINE):
                raise InstallError('APK installation was not successful: ' + output)
            verify_phone_identity(adb)
        elif action == 'permissions':
            adb.shell('chmod', '755', item['remote'])
        else:
            staged = '/data/local/tmp/.viti-bridge-' + uuid.uuid4().hex
            try:
                adb.run('push', str(source), staged)
                if adb.pull_hash(staged) != item['sha256']:
                    raise InstallError('Transferred helper failed checksum verification.')
                adb.shell('chmod', '755', staged)
                # Recheck just before publishing; never replace an existing helper.
                _, code = adb.shell('test', '-e', item['remote'], allow_failure=True)
                if code == 0:
                    raise InstallError('Destination appeared during installation; refusing to overwrite it.')
                adb.shell('mv', staged, item['remote'])
            finally:
                try:
                    adb.shell('rm', '-f', staged)
                except InstallError:
                    pass
    final_plan = inspect_installation(adb, manifest)
    if any(action != 'present' for _, action in final_plan):
        raise InstallError('Installation verification is incomplete; rerun after checking connectivity.')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--adb', help='Path to the official adb executable')
    parser.add_argument('--serial', help='Select among multiple compatible devices; compatibility checks still apply')
    parser.add_argument('--check', action='store_true', help='Validate payload, device and installation state without device writes')
    args = parser.parse_args(argv)
    try:
        root = Path(__file__).resolve().parent
        manifest = json.loads((root / 'manifest.json').read_text(encoding='utf-8'))
        verify_payload(root, manifest)
        adb = select_device(ADB(find_adb(args.adb)), manifest['expected_properties'], args.serial)
        plan = inspect_installation(adb, manifest)
        print('Compatible stock Android 4.4.4 HiMI/UFI MSM8916 device detected.')
        for item, action in plan:
            print(item['name'] + ': ' + action)
        if args.check:
            print('Check complete. No device files, settings or SIM operations were changed.')
        else:
            install(adb, root, manifest, plan)
            print('Bridge installation verified. No reboot or SIM challenge was performed.')
        print('Server setting: VITI_ADB_SERIAL=' + adb.serial)
        print('Continue with the server setup guide. This is not a carrier connectivity test.')
        return 0
    except (InstallError, OSError, ValueError, KeyError) as exc:
        print('ERROR: ' + str(exc), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
