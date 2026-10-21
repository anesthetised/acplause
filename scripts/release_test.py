"""Release safety invariants; no GitHub writes."""
import tempfile
import hashlib
import json
import os
import subprocess
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
                if '/releases/' in endpoint:
                    return existing
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
            with patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo'), patch.object(release, 'api', side_effect=api), patch.object(release, 'run', side_effect=run):
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

    def test_prepare_rerun_uses_saved_version_even_in_new_month(self):
        with tempfile.TemporaryDirectory() as d:
            state = Path(d)
            plan = {'tag': 'v0.202610.0', 'sha': 'a' * 40, 'previous': ''}
            (state / 'plan.json').write_text(json.dumps(plan))
            with patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo', GITHUB_SHA='a' * 40, GITHUB_RUN_ATTEMPT='2'), patch.object(release, 'restore_artifact', side_effect=[True, True]), patch.object(release, 'run', side_effect=['v0.202610.0', 'a' * 40]), patch.object(release, 'output') as output, patch.object(release, 'api') as api:
                release.prepare(state)
                output.assert_any_call('tag', 'v0.202610.0')
                api.assert_not_called()
            with patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo', GITHUB_SHA='a' * 40, GITHUB_RUN_ATTEMPT='2'), patch.object(release, 'restore_artifact', return_value=False):
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
