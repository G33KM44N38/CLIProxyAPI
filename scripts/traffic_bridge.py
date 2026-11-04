#!/usr/bin/env python3
"""Temporarily send new Linux TCP connections to a verified candidate proxy.

The existing proxy and its established connections remain running. Remove the
bridge only after the canonical listener serves the new build. Do not stop a
proxy while it still owns established connections. Rules do not survive reboot.
"""
import argparse
from dataclasses import dataclass
import ipaddress
import json
import os
import subprocess


IPTABLES = '/usr/sbin/iptables'


def run(arguments):
    return subprocess.run([IPTABLES, '--wait', '5', '-t', 'nat', *arguments],
                          capture_output=True, text=True, timeout=10)


@dataclass(frozen=True)
class Bridge:
    address: str
    source: int
    target: int

    def __post_init__(self):
        address = ipaddress.IPv4Address(self.address)
        if not (address.is_loopback or address in ipaddress.IPv4Network('100.64.0.0/10')):
            raise ValueError('Use a loopback or Tailscale IPv4 listener')
        if not (1024 <= self.source <= 65535 and 1024 <= self.target <= 65535) or self.source == self.target:
            raise ValueError('Provide different unprivileged source and candidate ports')

    def rule(self):
        return ['-p', 'tcp', '-d', self.address + '/32', '--dport', str(self.source),
                '-m', 'comment', '--comment', f'cliproxy-bridge-{self.source}-{self.target}',
                '-j', 'DNAT', '--to-destination', f'{self.address}:{self.target}']

    def status(self, execute=run):
        result = {}
        for chain in ('PREROUTING', 'OUTPUT'):
            checked = execute(['-C', chain, *self.rule()])
            if checked.returncode not in (0, 1):
                raise RuntimeError('Cannot inspect the NAT bridge: ' + chain)
            result[chain] = checked.returncode == 0
        return result

    def update(self, enabled, execute=run):
        before = self.status(execute)
        changed = []
        try:
            for chain, exists in before.items():
                if exists == enabled:
                    continue
                operation = ['-I', chain, '1'] if enabled else ['-D', chain]
                if execute([*operation, *self.rule()]).returncode:
                    raise RuntimeError('Cannot update the NAT bridge: ' + chain)
                changed.append(chain)
            after = self.status(execute)
            if any(value != enabled for value in after.values()):
                raise RuntimeError('NAT bridge did not reach the requested state')
            return after
        except Exception:
            for chain in reversed(changed):
                operation = ['-D', chain] if enabled else ['-I', chain, '1']
                if execute([*operation, *self.rule()]).returncode:
                    raise RuntimeError('NAT bridge rollback failed; inspect both chains') from None
            raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--address', required=True)
    parser.add_argument('--source-port', type=int, default=8317)
    parser.add_argument('--candidate-port', type=int, required=True)
    action = parser.add_mutually_exclusive_group()
    action.add_argument('--apply', action='store_true')
    action.add_argument('--remove', action='store_true')
    action.add_argument('--status', action='store_true')
    args = parser.parse_args()
    bridge = Bridge(args.address, args.source_port, args.candidate_port)
    if not (args.apply or args.remove or args.status):
        print(json.dumps({'requires_root': True, 'add': [
            [IPTABLES, '--wait', '5', '-t', 'nat', '-I', chain, '1', *bridge.rule()]
            for chain in ('PREROUTING', 'OUTPUT')], 'proxy_processes_changed': False}))
        return
    if os.geteuid() != 0:
        parser.exit(1, 'NAT changes require root; no proxy was stopped.\n')
    state = bridge.status() if args.status else bridge.update(args.apply)
    print(json.dumps({'bridge': state, 'proxy_processes_changed': False}))


if __name__ == '__main__':
    main()
