import csv
import gzip
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location('report', HERE / 'report.py')
report = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(report)
START = 1793049000000


class ReportTests(unittest.TestCase):
    def fixture(self, folder, name, start, latencies, dropped=0, metadata=True):
        scenario = {'executor': 'constant-arrival-rate', 'rate': 2, 'timeUnit': '1s', 'duration': '2s'}
        summary = {'metrics': {'dropped_iterations': {'values': {'count': dropped}}}, 'state': {'testRunDurationMs': 2001}}
        if metadata:
            summary['idns'] = {'version': 1, 'scenarios': {'private': scenario}}
        (folder / (name + '.json')).write_text(json.dumps(summary))
        with gzip.open(folder / (name + '.csv.gz'), 'wt', newline='') as stream:
            writer = csv.writer(stream, lineterminator='\n')
            writer.writerow(['metric_name', 'timestamp', 'metric_value', 'scenario', 'extra_tags'])
            if metadata:
                writer.writerow(['idns_scenario_start_ms', start, start, 'private', 'kind=private'])
            for index, latency in enumerate(latencies):
                outcome = 'Timeout' if latency == 5000 else 'ok'
                writer.writerow(['idns_query_ms', start + index * 500 + latency, latency, 'private', 'kind=private&stage=2&outcome=' + outcome])

    def analyze(self, folder):
        subprocess.run(['bash', str(HERE / 'analyze.sh'), str(folder)], check=True, capture_output=True)

    def test_two_generators_recompute_percentiles_and_align_offered_rate(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.fixture(folder, 'a', START, [1, 2, 3, 4], dropped=2)
            self.fixture(folder, 'b', START + 1000, [5, 6, 7, 5000], dropped=3)
            (folder / 'generators.txt').write_text('a\nb\n')
            self.analyze(folder)
            payload, warnings = report.export(folder, 'quoted"run', 'query', 'unknown', 'ghcr.io/test/dns@sha256:' + 'a' * 64)
            self.assertEqual(warnings, [])
            samples = payload.splitlines()
            offered = {int(line.rsplit(' ', 1)[1]): float(line.rsplit(' ', 2)[1]) for line in samples if line.startswith('idns_perf_query_offered_qps')}
            self.assertEqual(offered, {START: 2, START + 1000: 4, START + 2000: 2})
            self.assertIn(' 5 ' + str(START + 2000), next(line for line in samples if line.startswith('idns_perf_query_dropped_iterations')))
            self.assertEqual(sum(int(line.rsplit(' ', 2)[1]) for line in samples if line.startswith('idns_perf_query_qps')), 8)
            self.assertEqual(sum(int(line.rsplit(' ', 2)[1]) for line in samples if line.startswith('idns_perf_query_failed')), 1)
            self.assertTrue(all('run="quoted\\"run"' in line for line in samples))
            self.assertTrue(all('serving_image="ghcr.io/test/dns@sha256:' in line for line in samples))
            with (folder / 'timeline.tsv').open() as stream:
                overlap = next(row for row in csv.DictReader(stream, delimiter='\t') if int(row['second']) == START // 1000 + 1)
            # Combined raw [3,4,5,6] p50=4 and p99=6, not an average of p99s.
            self.assertEqual(float(overlap['p50_ms']), 4)
            self.assertEqual(float(overlap['p99_ms']), 6)

    def test_run_markers_stop_at_last_send_without_timeout_or_tick_extension(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.fixture(folder, 'a', START + 123, [1, 5000, 2])
            self.fixture(folder, 'b', START + 1000, [3, 5000, 4, 5])
            self.analyze(folder)
            payload, _ = report.export(folder, 'run', 'query', 'unknown')
            markers = [(int(line.rsplit(' ', 1)[1]), int(line.rsplit(' ', 2)[1]))
                       for line in payload.splitlines() if line.startswith('idns_perf_run{')]
            self.assertEqual(markers, [(START + 123, 1), (START + 2500, 1), (START + 2501, 0)])
            self.assertEqual(len({timestamp for timestamp, _ in markers}), len(markers))

    def test_old_metadata_does_not_publish_partial_offered_load(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.fixture(folder, 'a', START, [1, 1, 1, 1], metadata=False)
            self.fixture(folder, 'b', START, [1, 1, 1, 1])
            self.analyze(folder)
            payload, warnings = report.export(folder, 'run', 'query', 'unknown')
            self.assertEqual(len(warnings), 1)
            self.assertNotIn('idns_perf_query_offered_qps', payload)
            self.assertIn('idns_perf_query_dropped_iterations', payload)

    def test_missing_generator_and_stale_timeline_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.fixture(folder, 'a', START, [1, 1, 1, 1])
            self.analyze(folder)
            (folder / 'generators.txt').write_text('a\nb\n')
            with self.assertRaisesRegex(ValueError, 'partial or stale'):
                report.export(folder, 'run', 'query', 'unknown')
            (folder / 'generators.txt').unlink()
            text = (folder / 'timeline.tsv').read_text()
            (folder / 'timeline.tsv').write_text(text.replace('\t2\t0\t100.000', '\t1\t0\t100.000', 1))
            with self.assertRaisesRegex(ValueError, 'totals differ'):
                report.export(folder, 'run', 'query', 'unknown')

    def test_offline_wrapper_reanalyzes_and_rejects_invalid_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.fixture(folder, 'a', START, [1, 2, 3, 4])
            (folder / 'timeline.tsv').write_text('stale percentile output')
            env = {key: value for key, value in os.environ.items()
                   if key not in ('PROJECT', 'IMPORT_URL', 'SERVING_IMAGE', 'COMMIT')}
            subprocess.run(['bash', str(HERE / 'run.sh'), 'report', str(folder), 'query-private'],
                           env=env, check=True, capture_output=True)
            self.assertIn('scenario="query-private"', (folder / 'report.prom').read_text())
            with self.assertRaisesRegex(ValueError, 'immutable'):
                report.export(folder, 'run', 'query', 'unknown', 'dns:latest')
            (folder / 'INVALID.txt').touch()
            with self.assertRaisesRegex(ValueError, 'INVALID'):
                report.export(folder, 'run', 'query', 'unknown')

    def test_interrupted_schedule_omits_offered_load(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.fixture(folder, 'a', START, [1, 1])
            path = folder / 'a.json'
            summary = json.loads(path.read_text())
            summary['state']['testRunDurationMs'] = 1000
            path.write_text(json.dumps(summary))
            self.analyze(folder)
            payload, warnings = report.export(folder, 'run', 'query', 'unknown')
            self.assertEqual(len(warnings), 1)
            self.assertNotIn('idns_perf_query_offered_qps', payload)

    def test_run_refuses_to_reuse_results_before_starting_load(self):
        with tempfile.TemporaryDirectory() as directory:
            env = {**os.environ, 'PROJECT': 'fixture', 'ALLOW_ANY_ENV': '1', 'RESULTS': directory}
            result = subprocess.run(['bash', str(HERE / 'run.sh'), 'run'], env=env,
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('refusing to reuse results directory', result.stderr)

    def test_empty_ramp_segment(self):
        schedule = report.segments({'executor': 'ramping-arrival-rate', 'startRate': 0,
                                    'stages': [{'duration': '0s', 'target': 4},
                                               {'duration': '1s', 'target': 4}]})
        self.assertEqual(dict(report.offered_seconds(START, schedule)), {START // 1000: 4})

    def test_linear_ramp_and_fractional_boundary(self):
        schedule = report.segments({'executor': 'ramping-arrival-rate', 'startRate': 0, 'timeUnit': '1s', 'stages': [{'duration': '2s', 'target': 4}, {'duration': '1s', 'target': 4}]})
        self.assertEqual(dict(report.offered_seconds(START + 500, schedule)), {START // 1000: .25, START // 1000 + 1: 2, START // 1000 + 2: 3.75, START // 1000 + 3: 2})


if __name__ == '__main__':
    unittest.main()
