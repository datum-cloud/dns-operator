#!/usr/bin/env python3
"""Export query results and generator capacity evidence with exact timestamps."""
import argparse
import collections
import csv
import gzip
import json
import math
import os
from pathlib import Path
import re
import sys
import urllib.parse
import urllib.request


def seconds(value):
    text = str(value)
    if re.fullmatch(r'\d+(?:\.\d+)?', text):
        return float(text)
    parts = re.findall(r'(\d+(?:\.\d+)?)(ms|s|m|h)', text)
    if not parts or ''.join(number + unit for number, unit in parts) != text:
        raise ValueError(f'invalid duration {text!r}')
    return sum(float(number) * {'ms': .001, 's': 1, 'm': 60, 'h': 3600}[unit] for number, unit in parts)


def segments(scenario):
    """The same linear arrival-rate schedule k6 executes, in relative seconds."""
    unit = seconds(scenario.get('timeUnit', '1s'))
    if scenario['executor'] == 'constant-arrival-rate':
        rate = scenario['rate'] / unit
        return [(0, seconds(scenario['duration']), rate, rate)]
    if scenario['executor'] != 'ramping-arrival-rate':
        return []
    result, start, rate = [], 0, scenario['startRate'] / unit
    for stage in scenario['stages']:
        end, target = start + seconds(stage['duration']), stage['target'] / unit
        result.append((start, end, rate, target))
        start, rate = end, target
    return result


def offered_seconds(start_ms, schedule):
    """Integrate each rate segment over seconds, including partial boundaries."""
    values = collections.defaultdict(float)
    for lo, hi, first, last in schedule:
        start, end = start_ms / 1000 + lo, start_ms / 1000 + hi
        if end <= start:
            continue
        slope = (last - first) / (end - start)
        for second in range(math.floor(start), math.ceil(end)):
            a, b = max(start, second), min(end, second + 1)
            values[second] += first * (b - a) + slope / 2 * ((b - start) ** 2 - (a - start) ** 2)
    return values


def export(folder, run, scenario, commit, serving_image=''):
    if (folder / 'INVALID.txt').exists():
        raise ValueError('run is marked INVALID; do not publish it as performance evidence')
    files = sorted(folder.glob('*.csv.gz'))
    if not files:
        raise ValueError('no generator CSV records')
    names = {path.name.removesuffix('.csv.gz') for path in files}
    manifest = folder / 'generators.txt'
    if manifest.exists() and set(manifest.read_text().splitlines()) != names:
        raise ValueError('generator records do not match generators.txt; refusing a partial or stale run')
    actual = collections.Counter()
    offered = collections.defaultdict(float)
    dropped = 0
    warnings = []
    for path in files:
        summary = json.loads(path.with_name(path.name.removesuffix('.csv.gz') + '.json').read_text())
        if 'metrics' not in summary:
            raise ValueError(f'{path.name}: missing k6 summary metrics')
        dropped += summary['metrics'].get('dropped_iterations', {}).get('values', {}).get('count', 0)
        starts = {}
        with gzip.open(path, 'rt') as stream:
            for row in csv.DictReader(stream):
                tags = urllib.parse.parse_qs(row['extra_tags'])
                if row['metric_name'] == 'idns_scenario_start_ms':
                    kind, start = tags['kind'][0], float(row['metric_value'])
                    if kind in starts and starts[kind] != start:
                        raise ValueError(f'{path.name}: inconsistent scenario starts')
                    starts[kind] = start
                if row['metric_name'] == 'idns_query_ms':
                    if float(row['timestamp']) < 1e12:
                        raise ValueError('CSV timestamps must use K6_CSV_TIME_FORMAT=unix_milli')
                    kind = tags['kind'][0]
                    actual[kind, 'queries'] += 1
                    actual[kind, 'failed'] += tags['outcome'][0] != 'ok'
        metadata = summary.get('idns', {})
        schedules = {kind: segments(value) for kind, value in metadata.get('scenarios', {}).items()}
        schedules = {kind: value for kind, value in schedules.items() if value}
        duration = max((value[-1][1] for value in schedules.values()), default=0)
        if (metadata.get('version') != 1 or not schedules or set(schedules) - set(starts)
                or summary.get('state', {}).get('testRunDurationMs', 0) < duration * 1000):
            warnings.append(f'{path.name}: offered load unavailable (old metadata, missing start, or interrupted run)')
        else:
            for kind, schedule in schedules.items():
                for second, value in offered_seconds(starts[kind], schedule).items():
                    offered[kind, second] += value
    # Omitting the entire offered series is safer than publishing only some
    # generators when an older image or incomplete run lacks schedule metadata.
    if warnings:
        offered.clear()
    with (folder / 'timeline.tsv').open() as stream:
        rows = list(csv.DictReader(stream, delimiter='\t'))
    if not rows:
        raise ValueError('empty query timeline')
    timeline = collections.Counter()
    for row in rows:
        for measure in ['queries', 'failed']:
            timeline[row['kind'], measure] += int(row[measure])
    if actual != timeline:
        raise ValueError('timeline totals differ from raw CSV; rerun analyze.sh')
    labels = {'run': run, 'scenario': scenario, 'commit': commit}
    if serving_image:
        if not re.fullmatch(r'.+@sha256:[0-9a-f]{64}', serving_image):
            raise ValueError('SERVING_IMAGE must be an observed immutable image@sha256:digest')
        labels['serving_image'] = serving_image
    def sample(metric, value, timestamp, kind=None):
        fields = labels if kind is None else {**labels, 'kind': kind}
        encoded = ','.join(key + '=' + json.dumps(label) for key, label in fields.items())
        return f'{metric}{{{encoded}}} {value} {int(timestamp)}'
    output = []
    mapping = {'p50_ms': 'p50_ms', 'p99_ms': 'p99_ms', 'max_ms': 'max_ms', 'qps': 'queries', 'failed': 'failed'}
    for row in rows:
        for metric, column in mapping.items():
            output.append(sample('idns_perf_query_' + metric, row[column], int(row['second']) * 1000, row['kind']))
    first, last = min(int(row['second']) for row in rows), max(int(row['second']) for row in rows)
    output.append(sample('idns_perf_query_dropped_iterations', dropped, last * 1000))
    for (kind, second), value in sorted(offered.items()):
        output.append(sample('idns_perf_query_offered_qps', f'{value:.6f}', second * 1000, kind))
    for second in range(first, last + 11, 10):
        output.append(sample('idns_perf_run', 1, second * 1000))
    return '\n'.join(output) + '\n', warnings


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory', type=Path)
    parser.add_argument('scenario', nargs='?', default='query')
    parser.add_argument('--run')
    parser.add_argument('--commit', default=os.environ.get('COMMIT', 'unknown'))
    parser.add_argument('--serving-image', default=os.environ.get('SERVING_IMAGE', ''))
    parser.add_argument('--import-url', default=os.environ.get('IMPORT_URL', ''))
    args = parser.parse_args()
    payload, warnings = export(args.directory, args.run or args.directory.name, args.scenario, args.commit, args.serving_image)
    output = args.directory / 'report.prom'
    output.write_text(payload)
    for warning in warnings:
        print(warning, file=sys.stderr)
    if args.import_url:
        request = urllib.request.Request(args.import_url, payload.encode(), {'Content-Type': 'text/plain'})
        with urllib.request.urlopen(request, timeout=30) as response:
            if response.status not in (200, 204):
                raise RuntimeError(f'import failed with HTTP {response.status}')
        print(f'Imported {len(payload.splitlines())} samples for {args.run or args.directory.name}')
    else:
        print(f'Wrote {output}; set IMPORT_URL to publish')


if __name__ == '__main__':
    main()
