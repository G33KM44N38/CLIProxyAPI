#!/usr/bin/env python3
"""Verify a new private fork installation and native Codex discovery with fixtures."""
import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
from urllib.request import Request, urlopen

from install_fork import install


def verify(binary, panel, codex):
    with tempfile.TemporaryDirectory(prefix='proxy-fork-check-') as folder:
        root = Path(folder)
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        prefix = root / 'installation'
        install(prefix, binary, panel, port=port)
        path = prefix / 'config/config.json'
        config = json.loads(path.read_text())
        expected = {'gpt-5.5', 'gpt-6.1-sol', *[f'external-model-{i:02}' for i in range(60)]}
        config['openai-compatibility'] = [{
            'name': 'offline-fixture', 'base-url': 'http://127.0.0.1:1/v1',
            'api-key-entries': [{'api-key': 'offline-fixture-key'}],
            'models': [{'name': name, 'alias': name} for name in sorted(expected)]}]
        path.write_text(json.dumps(config))
        endpoint = f'http://127.0.0.1:{port}'

        def request(route, token):
            with urlopen(Request(endpoint + route, headers={'Authorization': 'Bearer ' + token}), timeout=5) as response:
                return response.read()

        with (root / 'server.log').open('wb') as log:
            process = subprocess.Popen([str(prefix / 'bin/cli-proxy-api'), '--config', str(path), '--local-model'],
                                       cwd=prefix, stdout=log, stderr=log)
            try:
                for _ in range(100):
                    if process.poll() is not None:
                        raise RuntimeError('Fork server exited during isolated verification')
                    try:
                        body = request('/v1/models?client_version=0.165.0', config['api-keys'][0])
                        break
                    except OSError:
                        time.sleep(0.1)
                else:
                    raise RuntimeError('Fork server did not become ready')
                payload = json.loads(body)
                assert {entry['slug'] for entry in payload['models']} == expected
                assert len(body) < 1 << 20, 'Native catalog still exceeds the HTTP limit'
                active = json.loads(request('/v0/management/config', (prefix / 'config/management-key').read_text().strip()))
                assert active['request-retry'] == 5
                assert active['max-retry-interval'] == 60
                assert active['routing']['strategy'] == 'fill-first'
                assert active['codex']['stream-bootstrap-buffering'] is True
                config_path = prefix / 'codex/config.toml'
                with config_path.open('a') as stream:
                    stream.write('model_catalog_url = ' + json.dumps(endpoint + '/v1/models') + '\n')
                environment = dict(os.environ, CODEX_HOME=str(prefix / 'codex'))
                for key in ('OPENAI_API_KEY', 'CODEX_API_KEY'):
                    environment.pop(key, None)
                result = subprocess.run([str(codex), 'debug', 'models'], env=environment,
                                        capture_output=True, text=True, timeout=45, check=True)
                discovered = json.loads(result.stdout)['models']
                assert {entry['slug'] for entry in discovered} == expected, 'Installed Codex rejected the HTTP catalog'
                assert (prefix / 'config/static/management.html').read_bytes() == panel.read_bytes()
                return {'fresh_install': True, 'native_catalog_bytes': len(body),
                        'native_models': len(discovered), 'runtime_retry_policy_verified': True,
                        'existing_installation_changed': False, 'fixture_provider': True}
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--panel', type=Path, required=True)
    parser.add_argument('--codex', type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(verify(args.binary, args.panel, args.codex)))
