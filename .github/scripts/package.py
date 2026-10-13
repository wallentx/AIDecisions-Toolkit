"""Package a verified native build with checksums and build provenance."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import zipfile

def output(*args):
    return subprocess.check_output(args, text=True).strip()

parser = argparse.ArgumentParser()
parser.add_argument('--out', default='dist/packages')
args = parser.parse_args()
version = Path('VERSION').read_text().strip()
host = json.loads(output('go', 'env', '-json', 'GOOS', 'GOARCH', 'CGO_ENABLED'))
suffix = '.exe' if host['GOOS'] == 'windows' else ''
launcher = Path('dist') / ('AID' + suffix)
catalog = json.loads(output(str(launcher.resolve()), '--version', '--json'))
manifests = [json.loads(p.read_text()) for p in sorted(Path('tools').glob('*/tool.json'))]
expected = {m['command']: m['version'] for m in manifests}
observed = {m['command']: m['tool_version'] for m in catalog['tools']}
if catalog['release_version'] != version or observed != expected or any(m['status'] != 'ok' for m in catalog['tools']):
    raise SystemExit('Built binaries do not match source versions')
paths = [Path('dist') / (name + suffix) for name in ['AID', *sorted(expected)]]
hashes = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in paths}
provenance = {'schema_version': 1, 'release_version': version,
              'commit': output('git', 'rev-parse', 'HEAD'), 'go_version': output('go', 'version'),
              'host': host, 'versions': catalog, 'sha256': hashes,
              'workflow_run': os.environ.get('GITHUB_SERVER_URL', 'https://github.com') + '/' + os.environ['GITHUB_REPOSITORY'] + '/actions/runs/' + os.environ['GITHUB_RUN_ID'] if os.environ.get('GITHUB_RUN_ID') else None}
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
