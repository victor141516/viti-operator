import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('bridge_installer', Path(__file__).with_name('install.py'))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)
MANIFEST = json.loads(Path(__file__).with_name('manifest.json').read_text())


class FakeADB:
    def __init__(self):
        self.serial = 'TEST_DEVICE'
        self.executable = 'adb'
        self.props = dict(MANIFEST['expected_properties'])
        self.files = {}
        self.executable_files = set()
        self.writes = []
        self.phone_uid = True
        self.apk_failure = False
        self.selinux = 'Permissive'

    def shell(self, *args, allow_failure=False):
        if args[0] == 'getprop':
            return self.props.get(args[1], ''), 0
        if args[0] == 'getenforce':
            return self.selinux, 0
        if args[:2] == ('pm', 'path'):
            return ('package:/data/app/lab.codex.simidentity-1.apk' if 'apk' in self.files else ''), 0
        if args[:2] == ('dumpsys', 'package'):
            return ('userId=1001 sharedUser=android.uid.phone' if self.phone_uid else 'userId=10042'), 0
        if args[0] == 'test':
            if args[1] == '-r':
                return '', 0
            if args[1] == '-x':
                return '', int(args[2] not in self.executable_files)
            return '', int(args[2] not in self.files)
        self.writes.append(args)
        if args[0] == 'chmod':
            self.executable_files.add(args[2])
        elif args[0] == 'mv':
            self.files[args[2]] = self.files.pop(args[1])
            self.executable_files.add(args[2])
        elif args[0] == 'rm':
            self.files.pop(args[2], None)
        else:
            raise AssertionError(args)
        return '', 0

    def pull_hash(self, remote):
        return self.files['apk' if remote.endswith('.apk') else remote]

    def run(self, *args):
        self.writes.append(args)
        if args[0] == 'install':
            if self.apk_failure:
                return 'Failure [INSTALL_FAILED_SHARED_USER_INCOMPATIBLE]'
            self.files['apk'] = MANIFEST['files'][0]['sha256']
            return 'Success'
        if args[0] == 'push':
            self.files[args[2]] = next(item['sha256'] for item in MANIFEST['files'] if item['name'] == Path(args[1]).name)
            return ''
        raise AssertionError(args)


class InstallerTests(unittest.TestCase):
    def test_supported_firmware(self):
        adb = FakeADB()
        installer.identify(adb, MANIFEST['expected_properties'])
        self.assertEqual(adb.writes, [])

    def test_phone_linux_and_other_firmware_rejected(self):
        for key, value in [('ro.product.model', 'Pixel'), ('ro.build.version.sdk', ''), ('ro.build.fingerprint', 'different'), ('gsm.version.baseband', 'different')]:
            with self.subTest(key=key):
                adb = FakeADB()
                adb.props[key] = value
                with self.assertRaises(installer.InstallError):
                    installer.identify(adb, MANIFEST['expected_properties'])
                self.assertEqual(adb.writes, [])

    def test_fresh_plan_is_read_only(self):
        adb = FakeADB()
        plan = installer.inspect_installation(adb, MANIFEST)
        self.assertEqual([action for _, action in plan], ['install'] * 3)
        self.assertEqual(adb.writes, [])

    def test_unvalidated_selinux_policy_is_not_disabled(self):
        adb = FakeADB()
        adb.selinux = 'Enforcing'
        with self.assertRaises(installer.InstallError):
            installer.identify(adb, MANIFEST['expected_properties'])
        self.assertEqual(adb.writes, [])

    def test_complete_install_then_idempotent(self):
        adb = FakeADB()
        installer.install(adb, Path('.'), MANIFEST, installer.inspect_installation(adb, MANIFEST))
        self.assertTrue(any(command[0] == 'install' for command in adb.writes))
        adb.writes.clear()
        installer.install(adb, Path('.'), MANIFEST, installer.inspect_installation(adb, MANIFEST))
        self.assertEqual(adb.writes, [])

    def test_conflict_prevents_all_writes(self):
        adb = FakeADB()
        adb.files['/data/local/tmp/qmi_uim_smsc'] = 'unknown-digest'
        with self.assertRaises(installer.InstallError):
            installer.inspect_installation(adb, MANIFEST)
        self.assertEqual(adb.writes, [])

    def test_existing_wrong_uid_rejected(self):
        adb = FakeADB()
        adb.files['apk'] = MANIFEST['files'][0]['sha256']
        adb.phone_uid = False
        with self.assertRaises(installer.InstallError):
            installer.inspect_installation(adb, MANIFEST)
        self.assertEqual(adb.writes, [])

    def test_install_signature_failure_does_not_push_helpers(self):
        adb = FakeADB()
        adb.apk_failure = True
        with self.assertRaises(installer.InstallError):
            installer.install(adb, Path('.'), MANIFEST, installer.inspect_installation(adb, MANIFEST))
        self.assertFalse(any(command[0] == 'push' for command in adb.writes))

    def test_permission_repair(self):
        adb = FakeADB()
        installer.install(adb, Path('.'), MANIFEST, installer.inspect_installation(adb, MANIFEST))
        adb.executable_files.clear()
        adb.writes.clear()
        installer.install(adb, Path('.'), MANIFEST, installer.inspect_installation(adb, MANIFEST))
        self.assertEqual([command[0] for command in adb.writes], ['chmod', 'chmod'])

    def test_device_selection_and_ambiguity(self):
        host = FakeADB()
        def make_device(executable, serial):
            candidate = FakeADB()
            candidate.serial = serial
            if serial == 'phone':
                candidate.props['ro.product.model'] = 'Other'
            return candidate
        with patch.object(installer, 'ADB', side_effect=make_device), patch.object(installer, 'list_devices', return_value=[('phone', 'device'), ('stick', 'device'), ('locked', 'unauthorized')]):
            self.assertEqual(installer.select_device(host, MANIFEST['expected_properties']).serial, 'stick')
            with self.assertRaises(installer.InstallError):
                installer.select_device(host, MANIFEST['expected_properties'], 'phone')
            with self.assertRaises(installer.InstallError):
                installer.select_device(host, MANIFEST['expected_properties'], 'locked')
        with patch.object(installer, 'ADB', side_effect=make_device), patch.object(installer, 'list_devices', return_value=[('one', 'device'), ('two', 'device')]):
            with self.assertRaises(installer.InstallError):
                installer.select_device(host, MANIFEST['expected_properties'])
            self.assertEqual(installer.select_device(host, MANIFEST['expected_properties'], 'two').serial, 'two')

    def test_legacy_adb_remote_exit_status(self):
        adb = installer.ADB('adb', 'test')
        with patch.object(adb, 'run', return_value='failure text\n__VITI_RC__1'):
            with self.assertRaises(installer.InstallError):
                adb.shell('test', '-e', '/missing')
            self.assertEqual(adb.shell('test', '-e', '/missing', allow_failure=True)[1], 1)
        with patch.object(adb, 'run', return_value='value\n__VITI_RC__0'):
            self.assertEqual(adb.shell('getprop', 'ro.hardware'), ('value', 0))
        with patch.object(adb, 'run', return_value='connection lost'):
            with self.assertRaises(installer.InstallError):
                adb.shell('id')

    def test_payload_checks_and_tampering(self):
        manifest = copy.deepcopy(MANIFEST)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'payload').mkdir()
            for item in manifest['files']:
                data = b'fake-apk' if item['kind'] == 'apk' else b'\x7fELF\x01\x01' + bytes(12) + b'\x28\x00'
                (root / 'payload' / item['name']).write_bytes(data)
                item.update(size=len(data), sha256=hashlib.sha256(data).hexdigest())
            installer.verify_payload(root, manifest)
            (root / 'payload' / manifest['files'][0]['name']).write_bytes(b'tampered')
            with self.assertRaises(installer.InstallError):
                installer.verify_payload(root, manifest)

    def test_check_mode_never_calls_install(self):
        adb = FakeADB()
        with patch.object(installer, 'verify_payload'), patch.object(installer, 'find_adb', return_value='adb'), patch.object(installer, 'select_device', return_value=adb), patch.object(installer, 'install') as do_install:
            self.assertEqual(installer.main(['--check']), 0)
            do_install.assert_not_called()
            self.assertEqual(adb.writes, [])


if __name__ == '__main__':
    unittest.main()
