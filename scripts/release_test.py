"""Release safety invariants; no GitHub writes."""
import hashlib
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
import release


class ReleaseTest(unittest.TestCase):
    def test_calendar_and_gaps(self):
        self.assertEqual(release.next_tag([], '2026-10-21'), 'v0.202610.0')
        self.assertEqual(release.next_tag(['v0.202610.0', 'v0.202610.9'], '2026-10-21'), 'v0.202610.10')
        self.assertEqual(release.next_tag(['v0.202609.12'], '2026-10-01'), 'v0.202610.0')
        with self.assertRaises(ValueError):
            release.next_tag(['v0.202611.0'], '2026-10-21')
    def test_retries_preserve_plan(self):
        plan = {'tag': 'v0.202610.0', 'sha': 'a' * 40, 'previous': ''}
        release.validate_plan(plan, 'a' * 40)
        with self.assertRaises(ValueError):
            release.validate_plan(plan, 'b' * 40)
        with self.assertRaises(ValueError):
            release.validate_plan(dict(plan, tag='bad;tag'), 'a' * 40)
    def test_checksum_tampering_and_missing_file(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d)
            (path / 'archive.tar.gz').write_bytes(b'original')
            checksum = hashlib.sha256(b'original').hexdigest()
            (path / 'checksums.txt').write_text(checksum + '  archive.tar.gz\n')
            release.verify_assets(path)
            (path / 'archive.tar.gz').write_bytes(b'tampered')
            with self.assertRaises(ValueError):
                release.verify_assets(path)
            (path / 'archive.tar.gz').unlink()
            with self.assertRaises(FileNotFoundError):
                release.verify_assets(path)
    def test_tap_retry_and_downgrade(self):
        self.assertFalse(release.should_update_formula('same', 'same'))
        self.assertTrue(release.should_update_formula('version "v0.202609.0"', 'version "v0.202610.0"'))
        with self.assertRaises(ValueError):
            release.should_update_formula('version "v0.202611.0"', 'version "v0.202610.0"')
        with self.assertRaises(ValueError):
            release.should_update_formula('version "v0.202610.0"\nold', 'version "v0.202610.0"\nnew')
    def test_first_notes_empty_without_cliff(self):
        with tempfile.TemporaryDirectory() as d, patch.object(release, 'run') as run:
            path = Path(d) / 'notes.md'
            release.notes({'tag': 'v0.202610.0', 'previous': ''}, path)
            self.assertEqual(path.read_text(), '')
            run.assert_not_called()

    def test_bundle_requires_windows_zips_and_preserves_their_bytes(self):
        plan = {'tag': 'v0.202610.0', 'sha': 'a' * 40, 'previous': ''}
        names = [f'acplause_{plan["tag"]}_{system}_{arch}.{extension}'
                 for system, extension in [('darwin', 'tar.gz'), ('linux', 'tar.gz'), ('windows', 'zip')]
                 for arch in ('amd64', 'arm64')]
        names.append(f'acplause_{plan["tag"]}_source.tar.gz')
        with tempfile.TemporaryDirectory() as d:
            directory = Path(d)
            for name in names:
                (directory / name).write_bytes(name.encode())
            def manifest(selected):
                (directory / 'checksums.txt').write_text(''.join(
                    hashlib.sha256((directory / name).read_bytes()).hexdigest() + '  ' + name + '\n'
                    for name in selected))
            manifest(names)
            release.bundle(plan, directory)
            self.assertEqual({p.name for p in release.verify_bundle(plan, directory)}, set(names + ['checksums.txt']))
            zip_name = f'acplause_{plan["tag"]}_windows_arm64.zip'
            manifest([name for name in names if name != zip_name])
            with self.assertRaisesRegex(ValueError, 'Missing or unexpected'):
                release.bundle(plan, directory)
            manifest(names)
            (directory / zip_name).write_bytes(b'changed')
            with self.assertRaisesRegex(ValueError, 'Checksum mismatch'):
                release.verify_bundle(plan, directory)

    def test_publish_resumes_partial_draft_without_replacing_bytes(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d)
            plan = {'tag': 'v0.202610.0', 'sha': 'a' * 40, 'previous': ''}
            (path / 'plan.json').write_text(json.dumps(plan))
            (path / 'notes.md').write_text('')
            (path / 'one.tar.gz').write_bytes(b'archive')
            (path / 'checksums.txt').write_text(hashlib.sha256(b'archive').hexdigest() + '  one.tar.gz\n')
            existing = {'draft': True, 'assets': [{'name': 'one.tar.gz'}]}
            def api(endpoint, *args, **kwargs):
                if '/git/ref/' in endpoint:
                    return {'object': {'sha': plan['sha']}}
                if '/commits/' in endpoint:
                    return {'sha': plan['sha']}
                self.fail(endpoint)
            calls = []
            def run(*args):
                calls.append(args)
                if args[:2] == ('gh', 'api'):
                    return '[[]]'
                if args[:3] == ('gh', 'release', 'download'):
                    (Path(args[-1]) / 'one.tar.gz').write_bytes(b'archive')
                return ''
            with (
                patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo'),
                patch.object(release, 'api', side_effect=api),
                patch.object(release, 'find_release', return_value=existing),
                patch.object(release, 'run', side_effect=run),
            ):
                release.publish(plan, path)
                uploads = [c for c in calls if c[:3] == ('gh', 'release', 'upload')]
                self.assertEqual(len(uploads), 1)
                self.assertEqual(uploads[0][-1], str(path / 'checksums.txt'))
                self.assertTrue(any(c[:3] == ('gh', 'release', 'edit') for c in calls))
                self.assertFalse(any('--clobber' in c for c in calls))
                # A published release with missing bytes must fail, never repair
                # itself by modifying already public artifacts.
                existing['draft'] = False
                calls.clear()
                with self.assertRaisesRegex(ValueError, 'missing'):
                    release.publish(plan, path)
                self.assertFalse(any(c[2] in ('upload', 'edit') for c in calls if len(c) > 2))
    def test_first_publish_and_completed_retry(self):
        with tempfile.TemporaryDirectory() as d:
            directory = Path(d)
            plan = {'tag': 'v0.202610.0', 'sha': 'a' * 40, 'previous': ''}
            (directory / 'plan.json').write_text(json.dumps(plan))
            (directory / 'notes.md').write_text('')
            (directory / 'archive.tar.gz').write_bytes(b'archive')
            digest = hashlib.sha256(b'archive').hexdigest()
            (directory / 'checksums.txt').write_text(digest + '  archive.tar.gz\n')
            remote = {'release': None, 'ref': None, 'assets': {}}
            mutations = []

            def api(endpoint, body=None, **kwargs):
                if endpoint.endswith('/releases'):
                    self.assertEqual(body, {
                        'tag_name': plan['tag'], 'target_commitish': plan['sha'],
                        'name': plan['tag'], 'body': '', 'draft': True, 'prerelease': False,
                    })
                    mutations.append(('draft', body))
                    remote['release'] = {'draft': True, 'assets': []}
                    return remote['release']
                if endpoint.endswith('/git/refs'):
                    mutations.append(('tag', body))
                    remote['ref'] = body
                    return body
                if '/git/ref/' in endpoint:
                    return remote['ref']
                if '/commits/' in endpoint:
                    return {'sha': plan['sha']}
                self.fail(endpoint)

            def run(*args):
                if args[:2] == ('gh', 'api'):
                    return '[[]]'
                action = args[2]
                if action == 'upload':
                    asset = Path(args[-1])
                    remote['assets'][asset.name] = asset.read_bytes()
                    remote['release']['assets'].append({'name': asset.name})
                elif action == 'edit':
                    remote['release']['draft'] = False
                elif action == 'download':
                    name = args[args.index('--pattern') + 1]
                    (Path(args[-1]) / name).write_bytes(remote['assets'][name])
                    return ''
                else:
                    self.fail(args)
                mutations.append(args)
                return ''

            with (
                patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo'),
                # The first request sees no release; the retry sees it only after
                # publication. Never re-read a newly created draft through the list.
                patch.object(release, 'find_release', side_effect=lambda *_: remote['release'] if remote['release'] and not remote['release']['draft'] else None),
                patch.object(release, 'api', side_effect=api),
                patch.object(release, 'run', side_effect=run),
            ):
                release.publish(plan, directory)
                self.assertEqual(remote['ref'], {'ref': 'refs/tags/v0.202610.0', 'sha': 'a' * 40})
                self.assertFalse(remote['release']['draft'])
                self.assertEqual(len(remote['assets']), 2)
                mutations.clear()
                release.publish(plan, directory)
                self.assertEqual(mutations, [])


    def test_find_draft_across_release_pages(self):
        draft = {'tag_name': 'v0.202610.0', 'draft': True}
        pages = [[{'tag_name': 'v0.202610.1', 'draft': False}], [draft]]
        with patch.object(release, 'run', return_value=json.dumps(pages)):
            self.assertEqual(release.find_release('owner/repo', 'v0.202610.0'), draft)
            self.assertIsNone(release.find_release('owner/repo', 'v0.202609.0'))
        with patch.object(release, 'run', return_value=json.dumps([[draft, draft]])):
            with self.assertRaises(ValueError):
                release.find_release('owner/repo', 'v0.202610.0')

    def test_prepare_rerun_uses_saved_version_even_in_new_month(self):
        with tempfile.TemporaryDirectory() as d:
            state = Path(d)
            plan = {'tag': 'v0.202610.0', 'sha': 'a' * 40, 'previous': ''}
            (state / 'plan.json').write_text(json.dumps(plan))
            with (
                patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo', GITHUB_SHA='a' * 40, GITHUB_RUN_ATTEMPT='2'),
                patch.object(release, 'restore_artifact', side_effect=[True, True]),
                patch.object(release, 'run', side_effect=['v0.202610.0', 'a' * 40]),
                patch.object(release, 'output') as output,
                patch.object(release, 'api') as api,
            ):
                release.prepare(state)
                output.assert_any_call('tag', 'v0.202610.0')
                api.assert_not_called()
            with (
                patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo', GITHUB_SHA='a' * 40, GITHUB_RUN_ATTEMPT='2'),
                patch.object(release, 'restore_artifact', return_value=False),
            ):
                with self.assertRaisesRegex(ValueError, 'No saved release plan'):
                    release.prepare(state)
    def test_cliff_sections_and_breaking_details(self):
        with tempfile.TemporaryDirectory() as d:
            def git(*args):
                return subprocess.check_output(['git', '-C', d, *args], text=True).strip()
            git('init', '-q')
            git('config', 'user.name', 'Release test')
            git('config', 'user.email', 'test@example.invalid')
            git('commit', '--allow-empty', '-qm', 'chore: initial')
            git('tag', 'v0.202610.0')
            for message in ['feat: visible feature', 'fix: visible fix', 'docs: visible docs',
                            'ci: hidden CI', 'test: hidden test', 'refactor: hidden refactor',
                            'chore(release): omit bookkeeping', 'Merge branch bookkeeping',
                            'ci!: breaking CI\n\nBREAKING CHANGE: migrate your config',
                            'chore(release)!: breaking bookkeeping']:
                git('commit', '--allow-empty', '-qm', message)
            git('tag', 'v0.202610.1')
            text = subprocess.check_output(['git-cliff', '--repository', d, '--config', str(release.ROOT / 'cliff.toml'), 'v0.202610.0..v0.202610.1', '--strip', 'all'], text=True)
            public, internal = text.split('<details>')
            self.assertIn('## Breaking changes', public)
            self.assertIn('migrate your config', public)
            self.assertIn('Breaking bookkeeping', public)
            self.assertIn('Visible feature', public)
            self.assertNotIn('Hidden CI', public)
            self.assertIn('Hidden CI', internal)
            self.assertIn('### Tests', internal)
            self.assertNotIn('omit bookkeeping', text.lower())
            self.assertNotIn('merge branch', text.lower())


if __name__ == '__main__':
    unittest.main()
