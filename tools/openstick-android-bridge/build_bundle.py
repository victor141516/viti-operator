#!/usr/bin/env python3
"""Package the installer with explicitly supplied recovered binaries."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import shutil
import zipfile

ROOT = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('installer', ROOT / 'install.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--payload', type=Path, required=True, help='Directory containing the three recovered binaries')
    parser.add_argument('--output', type=Path, required=True, help='Output ZIP path (outside the source tree)')
    args = parser.parse_args()
    manifest = json.loads((ROOT / 'manifest.json').read_text())
    with tempfile.TemporaryDirectory(prefix='viti-bridge-package-') as directory:
        staged = Path(directory)
        (staged / 'payload').mkdir()
        for item in manifest['files']:
            shutil.copyfile(args.payload / item['name'], staged / 'payload' / item['name'])
        installer.verify_payload(staged, manifest)
        for name in ('install.py', 'manifest.json', 'README.md', 'test_install.py'):
            shutil.copyfile(ROOT / name, staged / name)
        files = sorted(path for path in staged.rglob('*') if path.is_file())
        checksums = ''.join(installer.digest(path) + '  ' + path.relative_to(staged).as_posix() + '\n' for path in files)
        (staged / 'SHA256SUMS.txt').write_text(checksums, encoding='utf-8')
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with zipfile.ZipFile(args.output, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
            for path in sorted(staged.rglob('*')):
                if path.is_file():
                    archive.write(path, arcname=path.relative_to(staged).as_posix())
    checksum = args.output.with_name(args.output.name + '.sha256')
    checksum.write_text(installer.digest(args.output) + '  ' + args.output.name + '\n', encoding='utf-8')
    print(args.output)
    print(checksum)


if __name__ == '__main__':
    main()
