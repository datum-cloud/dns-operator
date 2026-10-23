// Load test for the internal DNS query path, run from inside a compute
// instance against its VPC resolver. See README.md for the knobs.
import dns from 'k6/x/dns';
import { sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import exec from 'k6/execution';

const env = (k, d) => (__ENV[k] === undefined || __ENV[k] === '' ? d : __ENV[k]);
const list = (k, d) => env(k, d).split(',').map((s) => s.trim()).filter(Boolean);
const int = (k, d) => parseInt(env(k, String(d)), 10);

const RESOLVER = env('RESOLVER', '[fd53::53]:53');
const DURATION = env('DURATION', '2m');
const STAGE_DURATION = env('STAGE_DURATION', '1m');

// Names an instance in this VPC should resolve to at least one address.
const PRIVATE_NAMES = list('PRIVATE_NAMES', '');
if (PRIVATE_NAMES.length === 0 && !env('ALLOW_NO_PRIVATE', '')) {
  throw new Error('PRIVATE_NAMES is empty, so private DNS would not be tested; set ALLOW_NO_PRIVATE=1 to run without it');
}
const PRIVATE_TYPE = env('PRIVATE_TYPE', 'AAAA');
// Names that exist only in another project; this VPC must get NXDOMAIN.
const NXDOMAIN_NAMES = list('NXDOMAIN_NAMES', '');
// Public names resolved through the recursor. AAAA answers for IPv4-only names
// come from DNS64. Pick apex names: xk6-dns rejects answers that hold a CNAME.
const PUBLIC_NAMES = list('PUBLIC_NAMES', 'google.com,cloudflare.com,github.com');
// TXT name whose value propagate.sh rewrites to "t=<unix ms>".
const PROPAGATION_NAME = env('PROPAGATION_NAME', '');
const PROPAGATION_POLL_MS = int('PROPAGATION_POLL_MS', 100);

const answered = new Rate('idns_expected');
const queryMs = new Trend('idns_query_ms', true);
const failures = new Counter('idns_failures');
// One counter per failure reason so the summary can break failures down.
const REASONS = ['NonExistingDomain', 'ServerFailure', 'Refused', 'Timeout', 'Answered', 'Empty', 'Other'];
const failuresBy = Object.fromEntries(REASONS.map((r) => [r, new Counter(`idns_failures_${r}`)]));
const propagationMs = new Trend('idns_propagation_ms', true);
const propagationSeen = new Counter('idns_propagation_updates');

// A comma list in RAMP (queries per second) turns the private scenario into a
// step ramp, holding each step for STAGE_DURATION, to find where latency or
// errors break down.
function arrival(rate, ramp) {
  const vus = (r) => ({ preAllocatedVUs: Math.max(10, Math.ceil(r / 20)), maxVUs: Math.max(50, r * 2) });
  if (ramp.length === 0) {
    return { executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration: DURATION, ...vus(rate) };
  }
  const stages = [];
  for (const r of ramp) {
    stages.push({ target: r, duration: '10s' }, { target: r, duration: STAGE_DURATION });
  }
  return { executor: 'ramping-arrival-rate', startRate: ramp[0], timeUnit: '1s', stages, ...vus(Math.max(...ramp)) };
}

const scenarios = {};
const thresholds = {};
// Thresholds also make k6 report each kind separately, so NO_THRESHOLDS keeps
// them but makes them always pass.
const enforce = !env('NO_THRESHOLDS', '');
function add(kind, fn, names, rate, ramp = []) {
  if (names.length === 0 || rate <= 0) return;
  scenarios[kind] = { ...arrival(rate, ramp), exec: fn, tags: { kind } };
  thresholds[`idns_expected{kind:${kind}}`] = [enforce ? `rate>=${env('MIN_SUCCESS', '0.999')}` : 'rate>=0'];
  thresholds[`idns_query_ms{kind:${kind}}`] = [enforce ? `p(95)<${int('P95_MS', 50)}` : 'p(95)>=0'];
}
add('private', 'queryPrivate', PRIVATE_NAMES, int('PRIVATE_RATE', 150), list('RAMP', '').map(Number));
add('nxdomain', 'queryNXDomain', NXDOMAIN_NAMES, int('NXDOMAIN_RATE', 25));
add('public', 'queryPublic', PUBLIC_NAMES, int('PUBLIC_RATE', 25));
// The node resolver is shared with every other instance on the node, so cap the
// load one generator may offer unless the run says otherwise.
const MAX_RATE = int('MAX_RATE', 200);
const offered = Object.values(scenarios).reduce((sum, s) => sum + (s.rate || Math.max(...s.stages.map((x) => x.target))), 0);
if (offered > MAX_RATE) {
  throw new Error(`offered load ${offered} qps is above MAX_RATE=${MAX_RATE}; raise MAX_RATE only when the resolver can take it`);
}
if (PROPAGATION_NAME) {
  scenarios.propagation = { executor: 'constant-vus', vus: 1, duration: env('PROPAGATION_DURATION', DURATION), exec: 'propagation', tags: { kind: 'propagation' } };
}

export const options = {
  scenarios,
  thresholds,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

function errorName(e) {
  if (e && typeof e === 'object' && e.name) return String(e.name);
  const s = String(e);
  const m = s.match(/^([A-Za-z]+): /);
  if (m && REASONS.includes(m[1])) return m[1];
  if (/timed out|timeout|i\/o/i.test(s)) return 'Timeout';
  return 'Other';
}

function fail(kind, reason) {
  failures.add(1, { kind, reason });
  (failuresBy[reason] || failuresBy.Other).add(1, { kind });
}

// Proves NXDOMAIN answers are recognised before trusting the nxdomain numbers:
// a name under .invalid can never exist.
export async function setup() {
  try {
    const answers = await dns.resolve('idns-perf-selfcheck.invalid', 'AAAA', RESOLVER);
    throw new Error(`self-check: expected NXDOMAIN for a .invalid name, got ${answers}`);
  } catch (e) {
    if (errorName(e) !== 'NonExistingDomain') {
      throw new Error(`self-check: expected NonExistingDomain for a .invalid name, got ${e}`);
    }
  }
}

const pick = (names) => names[exec.scenario.iterationInTest % names.length];

async function query(kind, name, type, expectNX) {
  const start = Date.now();
  let ok = false;
  let reason = '';
  try {
    const answers = await dns.resolve(name, type, RESOLVER);
    ok = !expectNX && answers.length > 0;
    reason = expectNX ? 'Answered' : 'Empty';
  } catch (e) {
    reason = errorName(e);
    ok = expectNX && reason === 'NonExistingDomain';
  }
  queryMs.add(Date.now() - start, { kind });
  answered.add(ok, { kind });
  if (!ok) fail(kind, reason);
}

export async function queryPrivate() {
  await query('private', pick(PRIVATE_NAMES), PRIVATE_TYPE, false);
}
export async function queryNXDomain() {
  await query('nxdomain', pick(NXDOMAIN_NAMES), 'AAAA', true);
}
export async function queryPublic() {
  await query('public', pick(PUBLIC_NAMES), 'AAAA', false);
}

// Polls the TXT name and, when its value changes, records how long ago the
// writer stamped it. Clock skew between the writer and this instance adds
// directly to the result, so keep both on NTP.
let lastSeen = '';
export async function propagation() {
  try {
    const values = await dns.resolve(PROPAGATION_NAME, 'TXT', RESOLVER);
    const value = values.find((v) => v.startsWith('t=')) || '';
    if (value && value !== lastSeen) {
      if (lastSeen !== '') {
        const lag = Date.now() - Number(value.slice(2));
        propagationMs.add(lag);
        propagationSeen.add(1);
        console.log(`propagation: saw ${value} after ${lag}ms`);
      }
      lastSeen = value;
    }
  } catch (e) {
    fail('propagation', errorName(e));
  }
  sleep(PROPAGATION_POLL_MS / 1000);
}

function fmt(m) {
  if (!m) return '-';
  const v = m.values;
  if (m.type === 'trend') {
    return ['avg', 'med', 'p(95)', 'p(99)', 'max'].map((k) => `${k}=${v[k].toFixed(1)}ms`).join(' ');
  }
  if (m.type === 'rate') return `${(v.rate * 100).toFixed(3)}% (${v.passes}/${v.passes + v.fails})`;
  if (m.type === 'counter') return `${v.count} (${v.rate.toFixed(1)}/s)`;
  return JSON.stringify(v);
}

export function handleSummary(data) {
  const lines = [`internal DNS load test against ${RESOLVER}`];
  for (const kind of Object.keys(scenarios)) {
    if (kind === 'propagation') continue;
    lines.push(`  ${kind}`);
    lines.push(`    latency   ${fmt(data.metrics[`idns_query_ms{kind:${kind}}`])}`);
    lines.push(`    expected  ${fmt(data.metrics[`idns_expected{kind:${kind}}`])}`);
  }
  if (PROPAGATION_NAME) {
    lines.push('  propagation');
    lines.push(`    lag       ${fmt(data.metrics.idns_propagation_ms)}`);
    lines.push(`    updates   ${fmt(data.metrics.idns_propagation_updates)}`);
  }
  lines.push(`  failures    ${fmt(data.metrics.idns_failures)}`);
  for (const r of REASONS) {
    const m = data.metrics[`idns_failures_${r}`];
    if (m) lines.push(`    ${r}: ${m.values.count}`);
  }
  lines.push(`  dropped iterations (generator saturated): ${data.metrics.dropped_iterations ? data.metrics.dropped_iterations.values.count : 0}`);
  for (const [name, m] of Object.entries(data.metrics)) {
    if (m.thresholds) {
      for (const [t, r] of Object.entries(m.thresholds)) lines.push(`  ${enforce ? (r.ok ? 'PASS' : 'FAIL') : 'REPORT ONLY'} ${name} ${t}`);
    }
  }
  return {
    stdout: `${lines.join('\n')}\n`,
    [env('SUMMARY_PATH', '/tmp/k6-summary.json')]: JSON.stringify(data),
  };
}
