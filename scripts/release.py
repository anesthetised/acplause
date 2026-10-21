#!/usr/bin/env python3
"""Small GitHub release coordinator. Build and archive creation belong to GoReleaser.

The immutable Actions artifacts are the retry journal: save the plan before
building, and save the bundle before publishing anything. Never rebuild bytes
that have already been published or silently allocate another version on retry.
Only Python's standard library and the runner's gh/git CLIs are used.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

TAG = re.compile(r'v0\.(20\d{2}(?:0[1-9]|1[0-2]))\.(0|[1-9]\d*)\Z')
ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def api(path, body=None, missing=False):
    args = ['gh', 'api', path]
    if body is not None:
        args += ['--input', '-']
    result = subprocess.run(args, input=json.dumps(body) if body is not None else None,
                            text=True, capture_output=True)
    if result.returncode:
        if missing and '(HTTP 404)' in result.stderr:
            return None
        raise RuntimeError(result.stderr.strip())
    return json.loads(result.stdout) if result.stdout.strip() else None


def tag_key(tag):
    match = TAG.fullmatch(tag)
    if not match:
        raise ValueError(f'Invalid release tag: {tag}')
    return tuple(map(int, match.groups()))


def next_tag(tags, date):
    month = date[:7].replace('-', '')
    keys = [tag_key(t) for t in tags if TAG.fullmatch(t)]
    if keys and max(keys)[0] > int(month):
        raise ValueError('A newer calendar month is already released')
    number = max((n for m, n in keys if m == int(month)), default=-1) + 1
    tag = f'v0.{month}.{number}'
    tag_key(tag)
    return tag


def validate_plan(plan, sha):
    tag_key(plan['tag'])
    if plan['sha'] != sha or not re.fullmatch(r'[0-9a-f]{40}', sha):
        raise ValueError('Saved release plan does not match this commit')
    if plan['previous']:
        if tag_key(plan['previous']) >= tag_key(plan['tag']):
            raise ValueError('Invalid previous release')


def restore_artifact(name, directory):
    repo, run_id = os.environ['GITHUB_REPOSITORY'], os.environ['GITHUB_RUN_ID']
    pages = json.loads(run('gh', 'api', '--paginate', '--slurp',
                          f'repos/{repo}/actions/runs/{run_id}/artifacts'))
    found = [a for page in pages for a in page['artifacts'] if a['name'] == name]
    if not found:
        return False
    if len(found) != 1 or found[0]['expired']:
        raise ValueError(f'{name} expired or is ambiguous; manual recovery required')
    run('gh', 'run', 'download', run_id, '--repo', repo, '--name', name, '--dir', str(directory))
    return True


def output(key, value):
    with open(os.environ['GITHUB_OUTPUT'], 'a') as file:
        file.write(f'{key}={value}\n')


def prepare(state):
    state.mkdir(parents=True, exist_ok=True)
    restored = restore_artifact('release-plan', state)
    repo, sha = os.environ['GITHUB_REPOSITORY'], os.environ['GITHUB_SHA']
    if restored:
        plan = json.loads((state / 'plan.json').read_text())
    else:
        if os.environ['GITHUB_RUN_ATTEMPT'] != '1':
            raise ValueError('No saved release plan; start a new run only after checking that nothing was published')
        tags = [t for t in run('git', 'tag', '--list').splitlines() if TAG.fullmatch(t)]
        previous = max(tags, key=tag_key, default='')
        if previous:
            release = api(f'repos/{repo}/releases/tags/{previous}', missing=True)
            if not release or release['draft']:
                raise ValueError(f'{previous} is unfinished; re-run its workflow first')
            run('git', 'merge-base', '--is-ancestor', previous, sha)
            if run('git', 'rev-parse', previous + '^{commit}') == sha:
                raise ValueError('This commit is already released')
        date = api(f'repos/{repo}/actions/runs/{os.environ["GITHUB_RUN_ID"]}')['created_at']
        plan = {'tag': next_tag(tags, date), 'sha': sha, 'previous': previous}
        (state / 'plan.json').write_text(json.dumps(plan) + '\n')
    validate_plan(plan, sha)
    # This tag is LOCAL: publication happens only after all checks and attestation.
    tags = run('git', 'tag', '--list', plan['tag']).splitlines()
    if tags:
        if run('git', 'rev-parse', plan['tag'] + '^{commit}') != sha:
            raise ValueError('Release tag already points to another commit')
    else:
        run('git', 'tag', plan['tag'], sha)
    output('plan-restored', str(restored).lower())
    output('tag', plan['tag'])
    output('bundle-restored', str(restore_artifact('release-bundle', Path('dist'))).lower())


def notes(plan, path):
    if not plan['previous']:
        path.write_text('')
    else:
        run('git-cliff', '--config', str(ROOT / 'cliff.toml'),
            plan['previous'] + '..' + plan['tag'], '--strip', 'all', '--output', str(path))


def verify_assets(directory):
    assets = []
    for line in (directory / 'checksums.txt').read_text().splitlines():
        digest, name = line.split(maxsplit=1)
        name = name.removeprefix('*')
        if Path(name).name != name or not re.fullmatch(r'[0-9a-f]{64}', digest):
            raise ValueError('Invalid checksums manifest')
        asset = directory / name
        if hashlib.sha256(asset.read_bytes()).hexdigest() != digest:
            raise ValueError(f'Checksum mismatch: {name}')
        assets.append(asset)
    if not assets or len(set(assets)) != len(assets):
        raise ValueError('Empty or duplicate checksums manifest')
    return assets + [directory / 'checksums.txt']


def bundle(plan, directory):
    assets = verify_assets(directory)
    expected = {f'acplause_{plan["tag"]}_{system}_{arch}.tar.gz'
                for system in ('darwin', 'linux') for arch in ('amd64', 'arm64')}
    source = directory / f'acplause_{plan["tag"]}_source.tar.gz'
    expected.update([source.name, 'checksums.txt'])
    if {p.name for p in assets} != expected:
        raise ValueError('Missing or unexpected release assets')
    digest = hashlib.sha256(source.read_bytes()).hexdigest()
    template = (ROOT / 'scripts/acplause.rb.in').read_text()
    formula = template.replace('@TAG@', plan['tag']).replace('@COMMIT@', plan['sha']).replace('@SHA256@', digest)
    (directory / 'acplause.rb').write_text(formula)
    notes(plan, directory / 'notes.md')
    (directory / 'plan.json').write_text(json.dumps(plan) + '\n')


def verify_bundle(plan, directory):
    assets = verify_assets(directory)
    if json.loads((directory / 'plan.json').read_text()) != plan:
        raise ValueError('Bundle does not match the saved plan')
    return assets


def publish(plan, directory):
    assets = verify_bundle(plan, directory)
    repo, tag, sha = os.environ['GITHUB_REPOSITORY'], plan['tag'], plan['sha']
    release = api(f'repos/{repo}/releases/tags/{tag}', missing=True)
    if not release or release['draft']:
        remote_tags = json.loads(run('gh', 'api', '--paginate', '--slurp', f'repos/{repo}/tags'))
        if any(tag_key(t['name']) > tag_key(tag) for page in remote_tags for t in page if TAG.fullmatch(t['name'])):
            raise ValueError('A newer tag exists; refusing to publish an older release')
    ref = api(f'repos/{repo}/git/ref/tags/{tag}', missing=True)
    if ref is None:
        api(f'repos/{repo}/git/refs', {'ref': 'refs/tags/' + tag, 'sha': sha})
    else:
        # Resolve annotated as well as lightweight tags; never move an existing tag.
        commit = api(f'repos/{repo}/commits/{tag}')['sha']
        if commit != sha:
            raise ValueError('Remote tag points to another commit')
    if release is None:
        run('gh', 'release', 'create', tag, '--repo', repo, '--verify-tag', '--draft',
            '--title', tag, '--notes-file', str(directory / 'notes.md'))
        release = api(f'repos/{repo}/releases/tags/{tag}')
    existing = {a['name'] for a in release['assets']}
    # A retry verifies uploaded bytes and only supplies missing assets. Published
    # releases are read-only even if repository immutability is not enabled.
    with tempfile.TemporaryDirectory() as temp:
        for asset in assets:
            if asset.name in existing:
                run('gh', 'release', 'download', tag, '--repo', repo, '--pattern', asset.name, '--dir', temp)
                if (Path(temp) / asset.name).read_bytes() != asset.read_bytes():
                    raise ValueError(f'Published asset differs: {asset.name}')
            elif release['draft']:
                run('gh', 'release', 'upload', tag, '--repo', repo, str(asset))
            else:
                raise ValueError(f'Published release is missing {asset.name}')
    if release['draft']:
        run('gh', 'release', 'edit', tag, '--repo', repo, '--draft=false', '--latest',
            '--notes-file', str(directory / 'notes.md'))


def should_update_formula(old, new):
    if old == new:
        return False
    if old:
        old_tag = re.search(r'version "([^"]+)"', old)
        new_tag = re.search(r'version "([^"]+)"', new)
        if not old_tag or not new_tag or tag_key(old_tag[1]) >= tag_key(new_tag[1]):
            raise ValueError('Refusing to replace the same or a newer formula version')
    return True


def update_tap(plan, directory):
    # Only this command receives the separate, tap-scoped PAT.
    endpoint = 'repos/anesthetised/homebrew-tap/contents/Formula/acplause.rb'
    current = api(endpoint, missing=True)
    old = base64.b64decode(current['content']).decode() if current else ''
    new = (directory / 'acplause.rb').read_text()
    if not should_update_formula(old, new):
        return
    author = {'name': 'anesthetised', 'email': 'methoxyphencyclidine@pm.me'}
    body = {'message': f'chore(acplause): update to {plan["tag"]}',
            'content': base64.b64encode(new.encode()).decode(), 'branch': 'main',
            'author': author, 'committer': author}
    if current:
        body['sha'] = current['sha']
    # Contents API requires PUT (gh --input defaults to POST).
    subprocess.run(['gh', 'api', '--method', 'PUT', endpoint, '--input', '-'],
                   input=json.dumps(body), text=True, stdout=subprocess.DEVNULL, check=True)


def main():
    command = sys.argv[1]
    state = Path(os.environ['RUNNER_TEMP']) / 'release-state'
    if command == 'prepare':
        prepare(state)
        return
    plan = json.loads((state / 'plan.json').read_text())
    validate_plan(plan, os.environ['GITHUB_SHA'])
    {'bundle': bundle, 'verify': verify_bundle, 'publish': publish, 'tap': update_tap}[command](plan, Path('dist'))


if __name__ == '__main__':
    main()
