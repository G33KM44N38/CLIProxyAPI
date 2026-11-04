import importlib.util
import json
from pathlib import Path
import tempfile
import tomllib
import unittest


SPEC = importlib.util.spec_from_file_location('install_fork', Path(__file__).with_name('install_fork.py'))
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


class InstallForkTests(unittest.TestCase):
    def test_fresh_install_retains_policy_and_refuses_to_replace_existing_data(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            binary, panel = root / 'binary', root / 'management.html'
            binary.write_bytes(b'built-binary-fixture')
            panel.write_text('<html>personal dashboard</html>')
            prefix = root / 'installation'
            result = installer.install(prefix, binary, panel, port=18317)
            config = json.loads((prefix / 'config/config.json').read_text())
            codex = tomllib.loads((prefix / 'codex/config.toml').read_text())['model_providers']['personal-proxy']
            self.assertEqual(config['routing'], {'strategy': 'fill-first', 'session-affinity': False})
            self.assertEqual(config['request-retry'], 5)
            self.assertEqual(config['max-retry-interval'], 60)
            self.assertEqual(config['codex'], {'model-level-cooling': True, 'stream-bootstrap-buffering': True, 'identity-confuse': False})
            self.assertEqual((codex['request_max_retries'], codex['stream_max_retries']), (4, 5))
            self.assertEqual(codex['experimental_bearer_token'], config['api-keys'][0])
            self.assertEqual(codex['base_url'], 'http://127.0.0.1:18317/v1')
            self.assertEqual(codex['model_catalog_url'], 'http://127.0.0.1:18317/v1/models')
            self.assertEqual((prefix / 'config/static/management.html').read_bytes(), panel.read_bytes())
            self.assertEqual((prefix / 'config/config.json').stat().st_mode & 0o777, 0o600)
            self.assertFalse(result['service_started'])
            auth = prefix / 'config/auth/preserve.json'
            auth.write_text('existing auth')
            original = (prefix / 'config/config.json').read_bytes()
            with self.assertRaises(FileExistsError):
                installer.install(prefix, binary, panel)
            self.assertEqual(auth.read_text(), 'existing auth')
            self.assertEqual((prefix / 'config/config.json').read_bytes(), original)

    def test_optional_plugin_uses_the_new_installations_database(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            binary, panel, plugin = root / 'binary', root / 'panel', root / 'usage-report.so'
            for path in (binary, panel, plugin):
                path.write_text('fixture')
            prefix = root / 'installation'
            installer.install(prefix, binary, panel, plugin)
            config = json.loads((prefix / 'config/config.json').read_text())
            self.assertEqual(config['plugins']['configs']['usage-report']['db_path'], str(prefix / 'config/usage-report/usage.sqlite'))
            self.assertEqual((prefix / 'lib/plugins/usage-report.so').read_text(), 'fixture')


if __name__ == '__main__':
    unittest.main()
