from types import SimpleNamespace
import unittest

from traffic_bridge import Bridge


class Firewall:
    def __init__(self):
        self.rules = set()
        self.fail = None

    def run(self, arguments):
        action, chain = arguments[:2]
        rule = tuple(arguments[3:] if action == '-I' else arguments[2:])
        entry = (chain, rule)
        if self.fail == (action, chain):
            return SimpleNamespace(returncode=2)
        if action == '-C':
            return SimpleNamespace(returncode=0 if entry in self.rules else 1)
        if action == '-I':
            self.rules.add(entry)
        elif action == '-D':
            self.rules.remove(entry)
        return SimpleNamespace(returncode=0)


class TrafficBridgeTests(unittest.TestCase):
    def setUp(self):
        self.bridge = Bridge('100.75.113.60', 8317, 18317)
        self.firewall = Firewall()

    def test_bridge_is_repeatable_and_removal_preserves_unrelated_rules(self):
        other = ('OUTPUT', ('unrelated-existing-rule',))
        self.firewall.rules.add(other)
        wanted = {'PREROUTING': True, 'OUTPUT': True}
        self.assertEqual(self.bridge.update(True, self.firewall.run), wanted)
        self.assertEqual(self.bridge.update(True, self.firewall.run), wanted)
        expected_rule = ('-p', 'tcp', '-d', '100.75.113.60/32', '--dport', '8317',
                         '-m', 'comment', '--comment', 'cliproxy-bridge-8317-18317',
                         '-j', 'DNAT', '--to-destination', '100.75.113.60:18317')
        self.assertEqual(self.firewall.rules, {other, ('PREROUTING', expected_rule), ('OUTPUT', expected_rule)})
        self.assertEqual(self.bridge.update(False, self.firewall.run), {'PREROUTING': False, 'OUTPUT': False})
        self.assertEqual(self.firewall.rules, {other})

    def test_failed_install_rolls_back_only_the_rule_it_added(self):
        self.firewall.fail = ('-I', 'OUTPUT')
        with self.assertRaisesRegex(RuntimeError, 'OUTPUT'):
            self.bridge.update(True, self.firewall.run)
        self.assertEqual(self.firewall.rules, set())
        self.assertEqual(self.bridge.status(self.firewall.run), {'PREROUTING': False, 'OUTPUT': False})

    def test_failed_removal_restores_the_previous_working_bridge(self):
        self.bridge.update(True, self.firewall.run)
        before = self.firewall.rules.copy()
        self.firewall.fail = ('-D', 'OUTPUT')
        with self.assertRaisesRegex(RuntimeError, 'OUTPUT'):
            self.bridge.update(False, self.firewall.run)
        self.assertEqual(self.firewall.rules, before)

    def test_refuses_same_ports_and_public_listeners(self):
        for address, source, target in [('100.75.113.60', 8317, 8317), ('8.8.8.8', 8317, 18317)]:
            with self.assertRaises(ValueError):
                Bridge(address, source, target)


if __name__ == '__main__':
    unittest.main()
