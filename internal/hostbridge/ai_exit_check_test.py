"""Synthetic probes; no network access."""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('checker', Path(__file__).parent / 'defaults/ai-exit-check/server.py')
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class ExitCheckTests(unittest.TestCase):
    def report(self, google_country, supported):
        def item(provider, country, support=True):
            return dict(provider=provider, name=provider, reachable=True, country=country,
                        ip='192.0.2.1' if country else '', supported_region=support, services={})
        with patch.object(checker, 'probe_baseline_geoip', return_value={'ip': '192.0.2.1', 'country': 'US'}), \
             patch.object(checker, 'probe_google', return_value=item('google', google_country, supported)), \
             patch.object(checker, 'probe_anthropic', return_value=item('anthropic', 'US')), \
             patch.object(checker, 'probe_openai', return_value=item('openai', 'US')):
            return checker.run_exit_check()

    def test_missing_region_cannot_report_consistency(self):
        report = self.report('', None)
        self.assertEqual(report['overall_status'], 'warn')
        self.assertFalse(report['all_regions_consistent'])
        self.assertFalse(report['all_ips_consistent'])
        self.assertEqual(report['providers'][0]['verdict'], 'warn')

    def test_unknown_support_cannot_report_success(self):
        self.assertEqual(self.report('US', None)['overall_status'], 'warn')

    def test_complete_supported_results_pass(self):
        self.assertEqual(self.report('US', True)['overall_status'], 'ok')

    def test_blocked_result_still_takes_priority(self):
        self.assertEqual(self.report('US', False)['overall_status'], 'blocked')


if __name__ == '__main__':
    unittest.main()
