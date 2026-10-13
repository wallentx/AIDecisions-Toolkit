"""Validate source version contracts before builds and releases."""
import argparse
import json
from pathlib import Path
import re

parser = argparse.ArgumentParser()
parser.add_argument('--tag')
args = parser.parse_args()
version = Path('VERSION').read_text().strip()
pattern = r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
if not re.fullmatch(pattern, version):
    raise SystemExit('VERSION must be a stable semantic version')
if args.tag is not None and args.tag != 'v' + version:
    raise SystemExit('Release tag must equal v + VERSION')
defaults = Path('internal/buildinfo/version.go').read_text()
if not re.search(r'var Version = "' + re.escape(version) + r'"', defaults):
    raise SystemExit('Build metadata default does not match VERSION')
for manifest in sorted(Path('tools').glob('*/tool.json')):
    metadata = json.loads(manifest.read_text())
    if not re.fullmatch(pattern, metadata['version']):
        raise SystemExit(f'Invalid tool version in {manifest}')
print(f'Version contracts verified: {version}')
