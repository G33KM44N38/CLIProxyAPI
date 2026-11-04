#!/usr/bin/env python3
"""Install this v7 fork into a new private directory without changing services."""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil


ROOT = Path(__file__).resolve().parents[1]


def install(prefix, binary, panel, plugin=None, port=8317):
    prefix = prefix.expanduser().absolute()
    binary, panel = binary.resolve(strict=True), panel.resolve(strict=True)
    if not binary.is_file() or not panel.is_file():
        raise ValueError('Provide a built v7 binary and your built management.html')
    if plugin is not None:
        plugin = plugin.resolve(strict=True)
        if not plugin.is_file():
            raise ValueError('Usage plugin must be a built library for this machine')
    if not 1024 <= port <= 65535:
        raise ValueError('Port must be between 1024 and 65535')
    config = json.loads((ROOT / 'config.fork.json').read_text())
    # mkdir is exclusive: an existing installation is never replaced.
    prefix.mkdir(mode=0o700, parents=True)
    for name in ('bin', 'config', 'config/auth', 'config/static', 'codex'):
        (prefix / name).mkdir(mode=0o700)
    target = prefix / 'bin/cli-proxy-api'
    shutil.copyfile(binary, target)
    target.chmod(0o700)
    shutil.copyfile(panel, prefix / 'config/static/management.html')
    (prefix / 'config/static/management.html').chmod(0o600)
    key, management = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    config.update({'port': port, 'auth-dir': str(prefix / 'config/auth'), 'api-keys': [key]})
    config['remote-management']['secret-key'] = management
    if plugin is not None:
        directory = prefix / 'lib/plugins'
        directory.mkdir(mode=0o700, parents=True)
        destination = directory / plugin.name
        shutil.copyfile(plugin, destination)
        destination.chmod(0o600)
        (prefix / 'config/usage-report').mkdir(mode=0o700)
        config['plugins'] = {
            'enabled': True, 'dir': str(directory),
            'configs': {'usage-report': {'enabled': True,
                        'db_path': str(prefix / 'config/usage-report/usage.sqlite')}}}
    write_private(prefix / 'config/config.json', json.dumps(config, indent=2) + '\n')
    write_private(prefix / 'config/management-key', management + '\n')
    write_private(prefix / 'codex/config.toml',
                  'model = "gpt-6.1-sol"\nmodel_provider = "personal-proxy"\n\n'
                  '[model_providers.personal-proxy]\nname = "Personal CLIProxyAPI"\n'
                  f'base_url = "http://127.0.0.1:{port}/v1"\nwire_api = "responses"\n'
                  'experimental_bearer_token = ' + json.dumps(key) + '\n'
                  'request_max_retries = 4\nstream_max_retries = 5\n'
                  'supports_websockets = false\n')
    return {'prefix': str(prefix), 'server_config': str(prefix / 'config/config.json'),
            'codex_home': str(prefix / 'codex'), 'management_key_file': str(prefix / 'config/management-key'),
            'usage_plugin_installed': plugin is not None, 'service_started': False}


def write_private(path, content):
    with open(path, 'x', opener=lambda name, flags: os.open(name, flags, 0o600)) as stream:
        stream.write(content)


def main():
    parser = argparse.ArgumentParser(description=__doc__, epilog=(
        'Build the server with go build -o cli-proxy-api ./cmd/server. '
        'Build management.html from G33KM44N38/Cli-Proxy-API-Management-Center '
        '(v7-compatible commit 7c703bc). Then pass both artifacts here. '
        'No panel release or provider credentials are downloaded. '
        'Start PREFIX/bin/cli-proxy-api --config PREFIX/config/config.json after '
        'authenticating your accounts in PREFIX/config/auth. For Codex use '
        'CODEX_HOME=PREFIX/codex. Existing per-account retry overrides still take '
        'precedence; omit request_retry to inherit the server policy. '
        'Persistent usage needs the separate compatible plugin via --usage-plugin.'))
    parser.add_argument('--prefix', type=Path, required=True, help='New installation directory; must not exist')
    parser.add_argument('--binary', type=Path, required=True, help='Built maintenance/msi-v7 binary')
    parser.add_argument('--panel', type=Path, required=True, help='Single-file dashboard built from your v7-compatible fork')
    parser.add_argument('--usage-plugin', type=Path, help='Optional trusted usage-report library built for the target OS/CPU')
    parser.add_argument('--port', type=int, default=8317)
    args = parser.parse_args()
    try:
        result = install(args.prefix, args.binary, args.panel, args.usage_plugin, args.port)
    except (OSError, ValueError) as error:
        parser.exit(1, 'Installation refused: ' + type(error).__name__ + '\n')
    print(json.dumps(result))


if __name__ == '__main__':
    main()
