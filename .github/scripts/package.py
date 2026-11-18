"""Package a verified native build with checksums and build provenance."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import zipfile

SEMVER_PATTERN = re.compile(
    r'^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'
)


def output(*args):
    return subprocess.check_output(args, text=True).strip()


parser = argparse.ArgumentParser()
parser.add_argument('--out', default='dist/packages')
parser.add_argument('--release', default=os.environ.get('RELEASE_TAG', ''))
args = parser.parse_args()

host = json.loads(output('go', 'env', '-json', 'GOOS', 'GOARCH', 'CGO_ENABLED'))
suffix = '.exe' if host['GOOS'] == 'windows' else ''
launcher = Path('dist') / ('AID' + suffix)
catalog = json.loads(output(str(launcher.resolve()), '--version', '--json'))
version = catalog['release_version']
if not SEMVER_PATTERN.fullmatch(version):
    raise SystemExit(f'Invalid built release_version: {version}')
if args.release.strip():
    expected_release = args.release.strip().removeprefix('v')
    if version != expected_release:
        raise SystemExit(
            f'Built release_version {version!r} does not match requested release {expected_release!r}'
        )

manifests = [json.loads(p.read_text()) for p in sorted(Path('tools').glob('*/tool.json'))]
expected = {m['command']: m['version'] for m in manifests}
observed = {m['command']: m['tool_version'] for m in catalog['tools']}
if observed != expected or any(m['status'] != 'ok' for m in catalog['tools']):
    raise SystemExit('Built binaries do not match tool manifests')

paths = [Path('dist') / (name + suffix) for name in ['AID', *sorted(expected)]]
hashes = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in paths}
provenance = {
    'schema_version': 1,
    'release_version': version,
    'commit': output('git', 'rev-parse', 'HEAD'),
    'go_version': output('go', 'version'),
    'host': host,
    'versions': catalog,
    'sha256': hashes,
    'workflow_run': (
        os.environ.get('GITHUB_SERVER_URL', 'https://github.com')
        + '/'
        + os.environ['GITHUB_REPOSITORY']
        + '/actions/runs/'
        + os.environ['GITHUB_RUN_ID']
        if os.environ.get('GITHUB_RUN_ID') and os.environ.get('GITHUB_REPOSITORY')
        else None
    ),
}
destination = Path(args.out)
destination.mkdir(parents=True, exist_ok=True)
archive = destination / f'AIDecisions-Toolkit-v{version}-{host["GOOS"]}-{host["GOARCH"]}.zip'
with zipfile.ZipFile(archive, 'w', compression=zipfile.ZIP_DEFLATED) as bundle:
    for p in paths:
        bundle.write(p, p.name)
    bundle.write('README.md', 'README.md')
    for p in sorted(Path('docs').rglob('*.md')):
        bundle.write(p, str(p))
    bundle.writestr('build-info.json', json.dumps(provenance, indent=2) + '\n')
    bundle.writestr('SHA256SUMS', ''.join(f'{value}  {name}\n' for name, value in sorted(hashes.items())))
print(archive)
